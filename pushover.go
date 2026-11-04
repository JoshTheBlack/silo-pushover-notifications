package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Pushover message limits (https://pushover.net/api#limits).
const (
	maxTitleRunes    = 250
	maxMessageRunes  = 1024
	maxURLBytes      = 512
	maxURLTitleRunes = 100
	maxAttachment    = 5 << 20
)

var (
	pushoverKeyPattern    = regexp.MustCompile(`^[A-Za-z0-9]{30}$`)
	pushoverDevicePattern = regexp.MustCompile(`^[A-Za-z0-9_,-]{0,100}$`)
	pushoverSoundPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{0,64}$`)
)

// builtinSounds are Pushover's stock sounds. Accounts can also upload custom
// sounds, which Validate returns alongside these.
var builtinSounds = []option{
	{"pushover", "Pushover (default)"}, {"bike", "Bike"}, {"bugle", "Bugle"},
	{"cashregister", "Cash Register"}, {"classical", "Classical"}, {"cosmic", "Cosmic"},
	{"falling", "Falling"}, {"gamelan", "Gamelan"}, {"incoming", "Incoming"},
	{"intermission", "Intermission"}, {"magic", "Magic"}, {"mechanical", "Mechanical"},
	{"pianobar", "Piano Bar"}, {"siren", "Siren"}, {"spacealarm", "Space Alarm"},
	{"tugboat", "Tug Boat"}, {"alien", "Alien Alarm (long)"}, {"climb", "Climb (long)"},
	{"persistent", "Persistent (long)"}, {"echo", "Pushover Echo (long)"},
	{"updown", "Up Down (long)"}, {"vibrate", "Vibrate Only"}, {"none", "None (silent)"},
}

type option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type pushoverMessage struct {
	Token          string
	User           string
	Title          string
	Message        string
	URL            string
	URLTitle       string
	Device         string
	Sound          string
	Priority       int
	HTML           bool
	Timestamp      time.Time
	Attachment     []byte
	AttachmentType string
}

// Emergency priority (2) repeats the alert until the user acknowledges it. The
// plugin doesn't handle acknowledgements, so it asks Pushover for a fixed
// cadence: every 60 s (Pushover's minimum is 30) for an hour (its maximum is
// 10800 s).
const (
	priorityEmergency = 2
	emergencyRetry    = 60 * time.Second
	emergencyExpire   = 3600 * time.Second
)

// pushoverError is a failed Pushover API call. Retryable is false for
// rejections that will fail again unchanged (bad key, monthly limit).
type pushoverError struct {
	HTTPStatus int
	Errors     []string
	Retryable  bool
}

func (e *pushoverError) Error() string {
	if len(e.Errors) > 0 {
		return "Pushover: " + strings.Join(e.Errors, "; ")
	}
	if e.HTTPStatus != 0 {
		return fmt.Sprintf("Pushover returned HTTP %d", e.HTTPStatus)
	}
	return "Pushover request failed"
}

type pushoverClient struct {
	http    *http.Client
	baseURL string
	// slots limits this process to maxPushoverConns requests at a time, as
	// Pushover asks.
	slots chan struct{}
	// onQuota, when set, hears the monthly quota Pushover reports on each
	// answer that carries it, with the application token it was for.
	onQuota func(ctx context.Context, token string, q quota)
}

// quota is an application's monthly message allowance, from the X-Limit-App-*
// headers or the limits endpoint. Reset is a Unix time.
type quota struct {
	Limit, Remaining int
	Reset            int64
}

// quotaFromHeaders reads the X-Limit-App-* headers; ok is false when they are
// missing or unreadable.
func quotaFromHeaders(h http.Header) (quota, bool) {
	limit, err1 := strconv.Atoi(strings.TrimSpace(h.Get("X-Limit-App-Limit")))
	remaining, err2 := strconv.Atoi(strings.TrimSpace(h.Get("X-Limit-App-Remaining")))
	if err1 != nil || err2 != nil || limit <= 0 || remaining < 0 {
		return quota{}, false
	}
	reset, _ := strconv.ParseInt(strings.TrimSpace(h.Get("X-Limit-App-Reset")), 10, 64)
	return quota{Limit: limit, Remaining: remaining, Reset: reset}, true
}

const maxPushoverConns = 2

func newPushoverClient() *pushoverClient {
	return &pushoverClient{
		http:    &http.Client{Timeout: 20 * time.Second},
		baseURL: "https://api.pushover.net/1",
		slots:   make(chan struct{}, maxPushoverConns),
	}
}

// Send posts one message.
func (c *pushoverClient) Send(ctx context.Context, m pushoverMessage) error {
	fields := map[string]string{
		"token":    m.Token,
		"user":     m.User,
		"message":  truncateRunes(m.Message, maxMessageRunes),
		"priority": strconv.Itoa(m.Priority),
	}
	if fields["message"] == "" {
		fields["message"] = "(no message)"
	}
	if m.Title != "" {
		fields["title"] = truncateRunes(m.Title, maxTitleRunes)
	}
	if m.URL != "" && len(m.URL) <= maxURLBytes {
		fields["url"] = m.URL
		if m.URLTitle != "" {
			fields["url_title"] = truncateRunes(m.URLTitle, maxURLTitleRunes)
		}
	}
	if m.Device != "" {
		fields["device"] = m.Device
	}
	if m.Sound != "" {
		fields["sound"] = m.Sound
	}
	if m.HTML {
		fields["html"] = "1"
	}
	if !m.Timestamp.IsZero() {
		fields["timestamp"] = strconv.FormatInt(m.Timestamp.Unix(), 10)
	}
	if m.Priority == priorityEmergency {
		fields["retry"] = strconv.Itoa(int(emergencyRetry / time.Second))
		fields["expire"] = strconv.Itoa(int(emergencyExpire / time.Second))
	}

	var body io.Reader
	var contentType string
	if len(m.Attachment) > 0 {
		buf := &bytes.Buffer{}
		w := multipart.NewWriter(buf)
		for k, v := range fields {
			_ = w.WriteField(k, v)
		}
		header := textproto.MIMEHeader{}
		header.Set("Content-Disposition", `form-data; name="attachment"; filename="poster"`)
		header.Set("Content-Type", m.AttachmentType)
		part, err := w.CreatePart(header)
		if err != nil {
			return err
		}
		_, _ = part.Write(m.Attachment)
		if err := w.Close(); err != nil {
			return err
		}
		body, contentType = buf, w.FormDataContentType()
	} else {
		form := url.Values{}
		for k, v := range fields {
			form.Set(k, v)
		}
		body, contentType = strings.NewReader(form.Encode()), "application/x-www-form-urlencoded"
	}

	_, _, err := c.do(ctx, http.MethodPost, "/messages.json", body, contentType, m.Token)
	return err
}

// Limits asks Pushover for an application's monthly quota. It is free: it
// doesn't count as a message.
func (c *pushoverClient) Limits(ctx context.Context, token string) (quota, error) {
	raw, _, err := c.do(ctx, http.MethodGet, "/apps/limits.json?token="+url.QueryEscape(token), nil, "", "")
	if err != nil {
		return quota{}, err
	}
	var resp struct {
		Limit     int   `json:"limit"`
		Remaining int   `json:"remaining"`
		Reset     int64 `json:"reset"`
	}
	if json.Unmarshal(raw, &resp) != nil || resp.Limit <= 0 {
		return quota{}, &pushoverError{Errors: []string{"Pushover's answer about the limit wasn't readable"}}
	}
	return quota{Limit: resp.Limit, Remaining: resp.Remaining, Reset: resp.Reset}, nil
}

// Validate checks a user key against an application token and returns the
// user's active device names plus the sounds available to the application.
func (c *pushoverClient) Validate(ctx context.Context, token, user string) (devices []string, sounds []option, err error) {
	form := url.Values{"token": {token}, "user": {user}}
	raw, err := c.call(ctx, http.MethodPost, "/users/validate.json", strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
	if err != nil {
		return nil, nil, err
	}
	var validated struct {
		Devices []string `json:"devices"`
	}
	_ = json.Unmarshal(raw, &validated)

	sounds = builtinSounds
	if raw, err := c.call(ctx, http.MethodGet, "/sounds.json?token="+url.QueryEscape(token), nil, ""); err == nil {
		sounds = mergeSounds(raw)
	}
	return validated.Devices, sounds, nil
}

// mergeSounds appends an account's custom sounds to the stock list.
func mergeSounds(raw []byte) []option {
	var resp struct {
		Sounds map[string]string `json:"sounds"`
	}
	sounds := append([]option(nil), builtinSounds...)
	if json.Unmarshal(raw, &resp) != nil {
		return sounds
	}
	known := map[string]bool{}
	for _, s := range builtinSounds {
		known[s.Value] = true
	}
	var custom []option
	for value, label := range resp.Sounds {
		if !known[value] {
			custom = append(custom, option{value, label + " (custom)"})
		}
	}
	return append(sounds, sortedOptions(custom)...)
}

func (c *pushoverClient) call(ctx context.Context, method, path string, body io.Reader, contentType string) ([]byte, error) {
	raw, _, err := c.do(ctx, method, path, body, contentType, "")
	return raw, err
}

// do makes one Pushover request. token, when given, is the application token
// the request is for: the quota headers on the answer are credited to it.
func (c *pushoverClient) do(ctx context.Context, method, path string, body io.Reader, contentType, token string) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	select {
	case c.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, &pushoverError{Errors: []string{"timed out waiting to contact api.pushover.net"}, Retryable: true}
	}
	defer func() { <-c.slots }()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, &pushoverError{Errors: []string{"could not reach api.pushover.net"}, Retryable: true}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if q, ok := quotaFromHeaders(resp.Header); ok && token != "" && c.onQuota != nil {
		c.onQuota(ctx, token, q)
	}

	var parsed struct {
		Status int      `json:"status"`
		Errors []string `json:"errors"`
	}
	_ = json.Unmarshal(raw, &parsed)
	if resp.StatusCode == http.StatusOK && parsed.Status == 1 {
		return raw, resp.Header, nil
	}
	return nil, resp.Header, &pushoverError{
		HTTPStatus: resp.StatusCode,
		Errors:     parsed.Errors,
		// 4xx means Pushover rejected the input (or the app hit its monthly
		// limit, 429); resending won't help. 5xx is Pushover having trouble.
		Retryable: resp.StatusCode >= 500,
	}
}

func sortedOptions(opts []option) []option {
	slices.SortFunc(opts, func(a, b option) int { return strings.Compare(strings.ToLower(a.Label), strings.ToLower(b.Label)) })
	return opts
}

func truncateRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	runes := []rune(s)
	return string(runes[:limit-1]) + "…"
}
