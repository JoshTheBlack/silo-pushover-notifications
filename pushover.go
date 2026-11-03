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

	_, err := c.call(ctx, http.MethodPost, "/messages.json", body, contentType)
	return err
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
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	select {
	case c.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, &pushoverError{Errors: []string{"timed out waiting to contact api.pushover.net"}, Retryable: true}
	}
	defer func() { <-c.slots }()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &pushoverError{Errors: []string{"could not reach api.pushover.net"}, Retryable: true}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var parsed struct {
		Status int      `json:"status"`
		Errors []string `json:"errors"`
	}
	_ = json.Unmarshal(raw, &parsed)
	if resp.StatusCode == http.StatusOK && parsed.Status == 1 {
		return raw, nil
	}
	return nil, &pushoverError{
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
