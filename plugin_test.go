package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/imagevariant"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimehost"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	testUserKey  = "uQiRzpo4DXghDmr9QzzfQu27cmVRsG"
	testAppToken = "azGDORePK8gMaC0QOYAMyEEuzJnyUi"
)

type memoryState struct {
	mu     sync.Mutex
	values map[string][]byte
}

func (m *memoryState) Read(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.values[key]
	return v, ok, nil
}

func (m *memoryState) Write(_ context.Context, key string, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[key] = value
	return nil
}

// fakePushover records sent messages and accepts any key except "bad…".
type fakePushover struct {
	mu   sync.Mutex
	sent []url.Values
	fail atomic.Int32 // HTTP status to answer messages.json with; 0 = succeed
	// hold, when set, makes messages.json wait until it is closed or the
	// request is cancelled.
	hold chan struct{}

	inflight, maxInflight atomic.Int32
}

func (f *fakePushover) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/users/validate.json":
		_ = r.ParseForm()
		if strings.HasPrefix(r.PostForm.Get("user"), "bad") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"status":0,"errors":["user key is invalid"]}`)
			return
		}
		_, _ = io.WriteString(w, `{"status":1,"devices":["iphone","desktop"]}`)
	case "/sounds.json":
		_, _ = io.WriteString(w, `{"status":1,"sounds":{"pushover":"Pushover (default)","mychime":"My Chime"}}`)
	case "/messages.json":
		n := f.inflight.Add(1)
		defer f.inflight.Add(-1)
		for {
			m := f.maxInflight.Load()
			if n <= m || f.maxInflight.CompareAndSwap(m, n) {
				break
			}
		}
		if f.hold != nil {
			select {
			case <-f.hold:
			case <-r.Context().Done():
				return
			}
		}
		if status := int(f.fail.Load()); status != 0 {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"status":0,"errors":["nope"]}`)
			return
		}
		if err := r.ParseMultipartForm(maxAttachment); err != nil {
			_ = r.ParseForm()
		}
		form := r.PostForm
		if r.MultipartForm != nil && len(r.MultipartForm.File["attachment"]) > 0 {
			form.Set("_attachment", "yes")
		}
		f.mu.Lock()
		f.sent = append(f.sent, form)
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{"status":1,"request":"x"}`)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakePushover) messages() []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]url.Values(nil), f.sent...)
}

type harness struct {
	plugin   *Plugin
	pushover *fakePushover
	routes   *routesServer
	events   *eventServer
	state    *memoryState
	siloURL  string
	pushURL  string
}

// testPoster is the image the fake Silo serves at /poster.jpg and /card.jpg.
var testPoster = []byte("\xff\xd8\xff\xe0 fake jpeg")

// newHarness wires a plugin to a fake Silo (user 7 owns profiles p1 "Josh"
// and p2 "Kid") and a fake Pushover.
func newHarness(t *testing.T) *harness {
	t.Helper()
	fp := &fakePushover{}
	pushoverSrv := httptest.NewServer(http.HandlerFunc(fp.handler))
	t.Cleanup(pushoverSrv.Close)

	slowPoster := make(chan struct{})
	silo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/poster.jpg", "/card.jpg":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(testPoster)
			return
		case "/slow.jpg":
			select {
			case <-slowPoster:
			case <-r.Context().Done():
			}
			return
		}
		if r.Header.Get("Authorization") != "Bearer sa_admin" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/api/v2/admin/users/7/profiles" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"items":[{"id":"p1","name":"Josh"},{"id":"p2","name":"Kid"}],"page":{"has_more":false}}`)
	}))
	t.Cleanup(silo.Close)
	t.Cleanup(func() { close(slowPoster) })

	state := &memoryState{values: map[string][]byte{}}
	h := &harness{pushover: fp, state: state, siloURL: silo.URL, pushURL: pushoverSrv.URL}
	h.plugin = h.newPlugin(state)
	h.routes = &routesServer{plugin: h.plugin}
	h.events = &eventServer{plugin: h.plugin}
	return h
}

// newPlugin builds a plugin process wired to the harness's fakes. Several
// processes sharing one state stand in for several Silo API servers.
func (h *harness) newPlugin(state stateBackend) *Plugin {
	p := newPlugin(slog.New(slog.NewTextHandler(io.Discard, nil)), state)
	p.pushover.baseURL = h.pushURL
	p.hostInfoFn = func(context.Context) (*runtimehost.HostInfo, error) {
		return &runtimehost.HostInfo{PublicBaseURL: "https://silo.example.com", InternalBaseURL: h.siloURL}, nil
	}
	p.resolveImagesFn = func(context.Context, []string, string) (map[string]string, error) {
		return nil, errHostUnavailable
	}
	p.setConfig(adminConfig{SiloAPIKey: "sa_admin", DefaultAppToken: testAppToken})
	return p
}

// configure saves enabled Pushover settings for profile p1.
func (h *harness) configure(t *testing.T, edit func(*ProfileSettings)) {
	t.Helper()
	s := &ProfileSettings{ProfileID: "p1", ProfileName: "Josh", Enabled: true, UserKey: testUserKey}
	if edit != nil {
		edit(s)
	}
	if err := h.plugin.store.Put(context.Background(), s); err != nil {
		t.Fatal(err)
	}
}

// episodeEvent is a notification.created event for profile p1.
func episodeEvent(eventID, deliveryID string, extra map[string]any) *pluginv1.HandleEventRequest {
	fields := map[string]any{"id": deliveryID, "type": "episode.available", "profile_id": "p1", "series_title": "Severance"}
	maps.Copy(fields, extra)
	payload, _ := structpb.NewStruct(fields)
	return &pluginv1.HandleEventRequest{EventName: eventNotificationCreated, EventId: eventID, Payload: payload}
}

func (h *harness) request(t *testing.T, method, path, profileName string, primary bool, body any) (int, map[string]any) {
	t.Helper()
	headers := map[string]string{"X-Silo-User-Id": "7", "X-Silo-Profile-Name": profileName}
	if primary {
		headers["X-Silo-Profile-Primary"] = "true"
	}
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
		headers["Content-Type"] = "application/json"
	}
	resp, err := h.routes.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: method, Path: path, Headers: headers, Body: raw})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.Unmarshal(resp.GetBody(), &out)
	return int(resp.GetStatusCode()), out
}

func TestRenderTemplate(t *testing.T) {
	vars := map[string]string{"series": "Severance", "sxe": "S02E01", "episode_title": "", "year": ""}
	cases := []struct{ tmpl, want string }{
		{"{sxe} · {episode_title}", "S02E01"},
		{"New episode: {series}", "New episode: Severance"},
		{"{series} ({year})", "Severance"},
		{"{unknown} stays", "{unknown} stays"},
		{"Line one\n{episode_title}\nLine three", "Line one\nLine three"},
		{"Keep\n\nblank", "Keep\n\nblank"},
	}
	for _, c := range cases {
		if got := renderTemplate(c.tmpl, vars, false); got != c.want {
			t.Errorf("renderTemplate(%q) = %q, want %q", c.tmpl, got, c.want)
		}
	}
	if got := renderTemplate("{series}", map[string]string{"series": "<b>"}, true); got != "&lt;b&gt;" {
		t.Errorf("HTML mode should escape values, got %q", got)
	}
}

func TestDeliveryVars(t *testing.T) {
	season, episode := 2, 1
	d := delivery{Type: "episode.available", SeriesTitle: "Severance", EpisodeTitle: "Hello", SeasonNumber: &season, EpisodeNumber: &episode,
		ReasonFlags: json.RawMessage(`{"favorite":true,"next_up":true}`)}
	vars := deliveryVars(d, "Josh")
	if vars["sxe"] != "S02E01" || vars["reasons"] != "Favorite, Next Up" || vars["profile"] != "Josh" {
		t.Fatalf("unexpected episode vars: %v", vars)
	}

	d = delivery{Type: "request.declined", ReasonFlags: json.RawMessage(`{"request_id":"r1","title":"Dune","year":2021,"media_type":"movie","reason":"Dupe"}`)}
	vars = deliveryVars(d, "")
	if vars["title_year"] != "Dune (2021)" || vars["media_type"] != "Movie" || vars["decline_reason"] != "Dupe" {
		t.Fatalf("unexpected request vars: %v", vars)
	}

	d = delivery{Type: "request.fulfilled", SeriesTitle: "Dune", ReasonFlags: json.RawMessage(`{"request_id":"r1","tmdb_id":1}`)}
	if got := deliveryVars(d, "")["title_year"]; got != "Dune" {
		t.Fatalf("fulfilled title_year = %q", got)
	}
}

func TestStorePersistsAcrossReload(t *testing.T) {
	state := &memoryState{values: map[string][]byte{}}
	s := newProfileStore(state)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		if err := s.Put(ctx, &ProfileSettings{ProfileID: id, UserKey: "k" + id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Delete(ctx, "b"); err != nil {
		t.Fatal(err)
	}

	reloaded := newProfileStore(state)
	got, found, err := reloaded.Get(ctx, "c")
	if err != nil || !found || got.UserKey != "kc" {
		t.Fatalf("Get(c) = %v, %v, %v", got, found, err)
	}
	if _, found, _ := reloaded.Get(ctx, "b"); found {
		t.Fatal("deleted profile came back")
	}
}

func TestParseAdminConfig(t *testing.T) {
	value, _ := structpb.NewStruct(map[string]any{"silo_api_key": " sa_x ", "default_app_token": "tok", "public_url": "https://s.example/"})
	cfg := parseAdminConfig([]*pluginv1.ConfigEntry{{Key: "other"}, {Key: adminConfigKey, Value: value}})
	if cfg.SiloAPIKey != "sa_x" || cfg.DefaultAppToken != "tok" || cfg.PublicURL != "https://s.example" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestEventDeliversToConfiguredProfile(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.plugin.store.Put(ctx, &ProfileSettings{
		ProfileID: "p1", ProfileName: "Josh", Enabled: true, UserKey: testUserKey, IncludeLink: true, Priority: -1,
		Types: map[string]TypeSettings{"request.approved": {Enabled: false}},
	}); err != nil {
		t.Fatal(err)
	}

	payload, _ := structpb.NewStruct(map[string]any{
		"id": "d1", "type": "episode.available", "profile_id": "p1",
		"series_id": "s1", "episode_id": "e1", "series_title": "Severance", "episode_title": "Hello",
		"season_number": 2, "episode_number": 1, "reason_flags": map[string]any{"watchlist": true},
		"created_at": "2026-09-23T12:00:00Z",
	})
	req := &pluginv1.HandleEventRequest{EventName: eventNotificationCreated, EventId: "ev1", Payload: payload}
	approved, _ := structpb.NewStruct(map[string]any{"id": "d2", "type": "request.approved", "profile_id": "p1"})
	other, _ := structpb.NewStruct(map[string]any{"id": "d3", "type": "episode.available", "profile_id": "p2"})
	for _, r := range []*pluginv1.HandleEventRequest{
		req,
		req, // duplicate: skipped
		{EventName: eventNotificationCreated, EventId: "ev2", Payload: approved},
		{EventName: eventNotificationCreated, EventId: "ev3", Payload: other},
		{EventName: "session.started", EventId: "ev4", Payload: other}, // not subscribed
	} {
		if _, err := h.events.HandleEvent(ctx, r); err != nil {
			t.Fatalf("HandleEvent(%s): %v", r.GetEventId(), err)
		}
	}

	sent := h.pushover.messages()
	if len(sent) != 1 {
		t.Fatalf("sent %d messages, want 1 (approved is off, p2 is unconfigured)", len(sent))
	}
	m := sent[0]
	checks := map[string]string{
		"token": testAppToken, "user": testUserKey, "title": "New episode: Severance", "message": "S02E01 · Hello",
		"url": "https://silo.example.com/item/e1", "priority": "-1",
		"timestamp": strconv.FormatInt(time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC).Unix(), 10),
	}
	for k, want := range checks {
		if got := m.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestSendErrorsClassifyRetryable(t *testing.T) {
	h := newHarness(t)
	msg := pushoverMessage{Token: testAppToken, User: testUserKey, Message: "x"}
	var perr *pushoverError
	h.pushover.fail.Store(http.StatusServiceUnavailable)
	if err := h.plugin.pushover.Send(context.Background(), msg); !errors.As(err, &perr) || !perr.Retryable {
		t.Fatalf("503 should be a retryable pushoverError, got %v", err)
	}
	for _, status := range []int{http.StatusBadRequest, http.StatusTooManyRequests} {
		h.pushover.fail.Store(int32(status))
		if err := h.plugin.pushover.Send(context.Background(), msg); !errors.As(err, &perr) || perr.Retryable {
			t.Fatalf("%d should be a non-retryable pushoverError, got %v", status, err)
		}
	}
}

func TestRetryableFailureAsksSiloToRetry(t *testing.T) {
	h := newHarness(t)
	h.configure(t, nil)
	ctx := context.Background()
	req := episodeEvent("ev1", "d1", nil)

	h.pushover.fail.Store(http.StatusServiceUnavailable)
	if _, err := h.events.HandleEvent(ctx, req); err == nil {
		t.Fatal("a Pushover 5xx should return an error so Silo retries")
	}
	if seen, _ := h.plugin.handled.Seen(ctx, "ev1"); seen {
		t.Fatal("a failed send must not be recorded as handled")
	}

	// Silo's retry succeeds and is sent once; a further repeat is skipped.
	h.pushover.fail.Store(0)
	for range 2 {
		if _, err := h.events.HandleEvent(ctx, req); err != nil {
			t.Fatalf("retry: %v", err)
		}
	}
	if n := len(h.pushover.messages()); n != 1 {
		t.Fatalf("sent %d messages, want 1", n)
	}
}

func TestNetworkErrorAsksSiloToRetry(t *testing.T) {
	h := newHarness(t)
	h.configure(t, nil)
	h.plugin.pushover.baseURL = "http://127.0.0.1:1" // nothing listens here
	if _, err := h.events.HandleEvent(context.Background(), episodeEvent("ev1", "d1", nil)); err == nil {
		t.Fatal("an unreachable Pushover should return an error so Silo retries")
	}
}

func TestPermanentFailureIsAcknowledged(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusTooManyRequests} {
		h := newHarness(t)
		h.configure(t, nil)
		ctx := context.Background()
		req := episodeEvent("ev1", "d1", nil)

		h.pushover.fail.Store(int32(status))
		if _, err := h.events.HandleEvent(ctx, req); err != nil {
			t.Fatalf("HTTP %d should be acknowledged, got %v", status, err)
		}
		// Recorded as handled, so a repeat isn't sent even once Pushover
		// would accept it.
		h.pushover.fail.Store(0)
		if _, err := h.events.HandleEvent(ctx, req); err != nil {
			t.Fatal(err)
		}
		if n := len(h.pushover.messages()); n != 0 {
			t.Fatalf("HTTP %d: repeat sent %d messages, want 0", status, n)
		}
	}
}

func TestEventDeadlineReturnsError(t *testing.T) {
	h := newHarness(t)
	h.configure(t, nil)
	h.pushover.hold = make(chan struct{})
	defer close(h.pushover.hold)
	h.plugin.eventTimeout = 100 * time.Millisecond

	start := time.Now()
	_, err := h.events.HandleEvent(context.Background(), episodeEvent("ev1", "d1", nil))
	if err == nil {
		t.Fatal("a send that outlives the deadline should return an error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("HandleEvent took %v; the deadline wasn't applied", elapsed)
	}
	if defaultEventTimeout != 8*time.Second {
		t.Fatalf("default event deadline is %v, want 8s", defaultEventTimeout)
	}
}

func TestDedupeIsSharedAcrossServers(t *testing.T) {
	h := newHarness(t)
	h.configure(t, nil)
	ctx := context.Background()
	if _, err := h.events.HandleEvent(ctx, episodeEvent("ev1", "d1", nil)); err != nil {
		t.Fatal(err)
	}

	// A second process (another Silo API server) with empty memory gets the
	// same event and skips it.
	other := &eventServer{plugin: h.newPlugin(h.state)}
	if _, err := other.HandleEvent(ctx, episodeEvent("ev1", "d1", nil)); err != nil {
		t.Fatal(err)
	}
	if n := len(h.pushover.messages()); n != 1 {
		t.Fatalf("sent %d messages, want 1", n)
	}
	if _, ok := h.state.values[eventLogKey("ev1")]; !ok {
		t.Fatalf("event ID not stored under %s", eventLogKey("ev1"))
	}
}

func TestEmptyEventIDFallsBackToDeliveryID(t *testing.T) {
	h := newHarness(t)
	h.configure(t, nil)
	ctx := context.Background()
	for _, req := range []*pluginv1.HandleEventRequest{
		episodeEvent("", "d1", nil),
		episodeEvent("", "d1", nil), // same delivery: skipped
		episodeEvent("", "d2", nil), // different delivery: sent
	} {
		if _, err := h.events.HandleEvent(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(h.pushover.messages()); n != 2 {
		t.Fatalf("sent %d messages, want 2", n)
	}
	if seen, _ := h.plugin.handled.Seen(ctx, ""); seen {
		t.Fatal("an empty ID must never count as handled")
	}
}

func TestIgnoredEventsAreNotRecorded(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// p1 has no settings yet, so the event is ignored without being stored.
	if _, err := h.events.HandleEvent(ctx, episodeEvent("ev1", "d1", nil)); err != nil {
		t.Fatal(err)
	}
	for key := range h.state.values {
		if strings.HasPrefix(key, "events/") {
			t.Fatalf("ignored event wrote %s", key)
		}
	}
}

func TestEventLogKeepsNewestIDs(t *testing.T) {
	state := &memoryState{values: map[string][]byte{}}
	l := newEventLog(state)
	ctx := context.Background()
	key := eventLogKey("first")
	var ids []string
	for i := 0; len(ids) < eventLogPerBucket+5; i++ {
		if id := "ev" + strconv.Itoa(i); eventLogKey(id) == key {
			ids = append(ids, id)
		}
	}
	if err := l.Record(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if err := l.Record(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	var stored []string
	_ = json.Unmarshal(state.values[key], &stored)
	if len(stored) != eventLogPerBucket || stored[len(stored)-1] != ids[len(ids)-1] {
		t.Fatalf("bucket holds %d IDs ending %q", len(stored), stored[len(stored)-1])
	}

	fresh := newEventLog(state) // no memory of what was recorded
	if seen, _ := fresh.Seen(ctx, "first"); seen {
		t.Fatal("the oldest ID should have been dropped")
	}
	if seen, _ := fresh.Seen(ctx, ids[len(ids)-1]); !seen {
		t.Fatal("the newest ID should be kept")
	}

	long := strings.Repeat("x", 5000)
	if err := l.Record(ctx, long); err != nil {
		t.Fatal(err)
	}
	if seen, _ := fresh.Seen(ctx, long); !seen {
		t.Fatal("a long ID should be found through its hash")
	}
}

func TestPushoverAllowsTwoConcurrentRequests(t *testing.T) {
	h := newHarness(t)
	h.pushover.hold = make(chan struct{})
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			_ = h.plugin.pushover.Send(context.Background(), pushoverMessage{Token: testAppToken, User: testUserKey, Message: "x"})
		})
	}
	deadline := time.Now().Add(2 * time.Second)
	for h.pushover.inflight.Load() < maxPushoverConns && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // give any extra request time to arrive
	close(h.pushover.hold)
	wg.Wait()
	if got := h.pushover.maxInflight.Load(); got != maxPushoverConns {
		t.Fatalf("max concurrent Pushover requests = %d, want %d", got, maxPushoverConns)
	}
	if n := len(h.pushover.messages()); n != 6 {
		t.Fatalf("sent %d messages, want 6", n)
	}
}

func TestPosterPrefersCardVariant(t *testing.T) {
	h := newHarness(t)
	h.configure(t, func(s *ProfileSettings) { s.AttachPoster = true })
	var gotVariant string
	h.plugin.resolveImagesFn = func(_ context.Context, paths []string, variant string) (map[string]string, error) {
		gotVariant = variant
		return map[string]string{paths[0]: h.siloURL + "/card.jpg"}, nil
	}
	req := episodeEvent("ev1", "d1", map[string]any{"poster_path": "/posters/1.jpg", "poster_url": "/slow.jpg"})
	if _, err := h.events.HandleEvent(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	sent := h.pushover.messages()
	if len(sent) != 1 || sent[0].Get("_attachment") != "yes" || gotVariant != imagevariant.Card {
		t.Fatalf("want one message with the card poster; variant %q, sent %v", gotVariant, sent)
	}
}

func TestPosterFallsBackToPosterURL(t *testing.T) {
	h := newHarness(t)
	h.configure(t, func(s *ProfileSettings) { s.AttachPoster = true })
	// resolveImagesFn fails (harness default), so poster_url is used.
	req := episodeEvent("ev1", "d1", map[string]any{"poster_path": "/posters/1.jpg", "poster_url": "/poster.jpg"})
	if _, err := h.events.HandleEvent(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if sent := h.pushover.messages(); len(sent) != 1 || sent[0].Get("_attachment") != "yes" {
		t.Fatalf("want one message with the poster attached, got %v", sent)
	}
}

func TestSlowPosterIsSkipped(t *testing.T) {
	h := newHarness(t)
	h.configure(t, func(s *ProfileSettings) { s.AttachPoster = true })
	h.plugin.posterTimeout = 100 * time.Millisecond
	start := time.Now()
	if _, err := h.events.HandleEvent(context.Background(), episodeEvent("ev1", "d1", map[string]any{"poster_url": "/slow.jpg"})); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("HandleEvent took %v; the poster limit wasn't applied", elapsed)
	}
	sent := h.pushover.messages()
	if len(sent) != 1 || sent[0].Get("_attachment") != "" {
		t.Fatalf("want one message without an attachment, got %v", sent)
	}
	if defaultPosterTimeout != 3*time.Second {
		t.Fatalf("default poster limit is %v, want 3s", defaultPosterTimeout)
	}
}

func TestStoreReloadsAfterTTL(t *testing.T) {
	state := &memoryState{values: map[string][]byte{}}
	ctx := context.Background()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	a := newProfileStore(state)
	a.now = func() time.Time { return now }
	if _, found, err := a.Get(ctx, "p1"); err != nil || found {
		t.Fatalf("Get before any save = %v, %v", found, err)
	}

	// Another server saves p1.
	b := newProfileStore(state)
	if err := b.Put(ctx, &ProfileSettings{ProfileID: "p1", UserKey: "k1"}); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := a.Get(ctx, "p1"); found {
		t.Fatal("cache should still be in use inside the TTL")
	}
	now = now.Add(settingsCacheTTL)
	if got, found, err := a.Get(ctx, "p1"); err != nil || !found || got.UserKey != "k1" {
		t.Fatalf("after TTL Get = %v, %v, %v", got, found, err)
	}
}

func TestStoreKeepsCacheWhenReloadFails(t *testing.T) {
	state := &flakyState{memoryState: memoryState{values: map[string][]byte{}}}
	ctx := context.Background()
	s := newProfileStore(state)
	if err := s.Put(ctx, &ProfileSettings{ProfileID: "p1", UserKey: "k1"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	s.ttl = 0
	state.fail.Store(true)
	if got, found, err := s.Get(ctx, "p1"); err != nil || !found || got.UserKey != "k1" {
		t.Fatalf("Get with failing state = %v, %v, %v; want the cached copy", got, found, err)
	}
}

func TestStoreWriteKeepsOtherServersChanges(t *testing.T) {
	state := &memoryState{values: map[string][]byte{}}
	ctx := context.Background()
	// Two profile IDs that share a bucket.
	first, second := "a0", ""
	for i := 1; second == ""; i++ {
		if id := "a" + strconv.Itoa(i); bucketKey(id) == bucketKey(first) {
			second = id
		}
	}

	a, b := newProfileStore(state), newProfileStore(state)
	if _, _, err := a.Get(ctx, first); err != nil { // load a's cache while empty
		t.Fatal(err)
	}
	if err := b.Put(ctx, &ProfileSettings{ProfileID: first, UserKey: "k1"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Put(ctx, &ProfileSettings{ProfileID: second, UserKey: "k2"}); err != nil {
		t.Fatal(err)
	}

	fresh := newProfileStore(state)
	for id, key := range map[string]string{first: "k1", second: "k2"} {
		if got, found, _ := fresh.Get(ctx, id); !found || got.UserKey != key {
			t.Fatalf("profile %s lost after the other server's write", id)
		}
	}
	if got, found, _ := a.Get(ctx, first); !found || got.UserKey != "k1" {
		t.Fatal("writing a bucket should refresh that bucket in the writer's cache")
	}
}

// flakyState is a memoryState whose reads can be made to fail.
type flakyState struct {
	memoryState
	fail atomic.Bool
}

func (f *flakyState) Read(ctx context.Context, key string) ([]byte, bool, error) {
	if f.fail.Load() {
		return nil, false, errors.New("state unavailable")
	}
	return f.memoryState.Read(ctx, key)
}

func TestNonPrimaryProfileSeesOnlyItself(t *testing.T) {
	h := newHarness(t)
	status, state := h.request(t, http.MethodGet, "/api/state", "Kid", false, nil)
	if status != http.StatusOK {
		t.Fatalf("state status %d: %v", status, state)
	}
	profiles := state["profiles"].([]any)
	if len(profiles) != 1 || profiles[0].(map[string]any)["id"] != "p2" {
		t.Fatalf("Kid should only see p2, got %v", profiles)
	}
	if status, _ := h.request(t, http.MethodGet, "/api/profiles/p1", "Kid", false, nil); status != http.StatusNotFound {
		t.Fatalf("Kid reading p1: status %d, want 404", status)
	}

	status, state = h.request(t, http.MethodGet, "/api/state", "Josh", true, nil)
	if status != http.StatusOK || len(state["profiles"].([]any)) != 2 {
		t.Fatalf("primary should see both profiles: %d %v", status, state)
	}
}

func TestSaveValidatesAndMasksKeys(t *testing.T) {
	h := newHarness(t)
	status, body := h.request(t, http.MethodPut, "/api/profiles/p2", "Kid", false, map[string]any{"enabled": true, "user_key": "bad" + testUserKey[3:]})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("bad key: status %d, want 422 (%v)", status, body)
	}

	status, body = h.request(t, http.MethodPut, "/api/profiles/p2", "Kid", false, map[string]any{"enabled": true, "user_key": testUserKey, "sound": "mychime"})
	if status != http.StatusOK {
		t.Fatalf("save: status %d (%v)", status, body)
	}
	profile := body["profile"].(map[string]any)
	if profile["user_key_hint"] != "uQiR…VRsG" {
		t.Fatalf("unexpected hint %v", profile["user_key_hint"])
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), testUserKey) {
		t.Fatal("response leaked the full user key")
	}

	// Saving again without re-entering the key keeps it.
	status, _ = h.request(t, http.MethodPut, "/api/profiles/p2", "Kid", false, map[string]any{"enabled": false})
	if status != http.StatusOK {
		t.Fatalf("resave: status %d", status)
	}
	stored, _, _ := h.plugin.store.Get(context.Background(), "p2")
	if stored.UserKey != testUserKey || stored.Enabled || stored.UserID != 7 {
		t.Fatalf("unexpected stored settings: %+v", stored)
	}
}

func TestSaveRequiresTokenWithoutServerDefault(t *testing.T) {
	h := newHarness(t)
	h.plugin.setConfig(adminConfig{SiloAPIKey: "sa_admin"})
	status, _ := h.request(t, http.MethodPut, "/api/profiles/p2", "Kid", false, map[string]any{"enabled": true, "user_key": testUserKey})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422", status)
	}
	status, _ = h.request(t, http.MethodPut, "/api/profiles/p2", "Kid", false, map[string]any{"enabled": true, "user_key": testUserKey, "app_token": testAppToken})
	if status != http.StatusOK {
		t.Fatalf("status %d with own token, want 200", status)
	}
}

func TestStateReportsMissingSetup(t *testing.T) {
	h := newHarness(t)
	h.plugin.setConfig(adminConfig{})
	status, state := h.request(t, http.MethodGet, "/api/state", "Josh", true, nil)
	if status != http.StatusOK || state["problem"].(map[string]any)["code"] != "setup_required" {
		t.Fatalf("status %d state %v", status, state)
	}
}

func TestMutationsRequireJSON(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.routes.Handle(context.Background(), &pluginv1.HandleHTTPRequest{
		Method: http.MethodPut, Path: "/api/profiles/p1",
		Headers: map[string]string{"X-Silo-User-Id": "7", "X-Silo-Profile-Name": "Josh", "Content-Type": "application/x-www-form-urlencoded"},
	})
	if resp.GetStatusCode() != http.StatusUnsupportedMediaType {
		t.Fatalf("status %d, want 415", resp.GetStatusCode())
	}
}
