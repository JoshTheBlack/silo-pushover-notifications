package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
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
	// failUser, when set, makes messages.json answer 503 for that user key.
	failUser atomic.Value
	// hold, when set, makes messages.json wait until it is closed or the
	// request is cancelled.
	hold chan struct{}
	// quotaLimit, when set, makes message answers carry the X-Limit-App-*
	// headers (and /apps/limits.json answer) with these numbers.
	quotaLimit, quotaRemaining, quotaReset atomic.Int64

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
		if strings.HasPrefix(r.PostForm.Get("token"), "bad") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"status":0,"errors":["application token is invalid"]}`)
			return
		}
		_, _ = io.WriteString(w, `{"status":1,"devices":["iphone","desktop"]}`)
	case "/apps/limits.json":
		if strings.HasPrefix(r.URL.Query().Get("token"), "bad") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"status":0,"errors":["application token is invalid, see https://pushover.net/api"]}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"limit":%d,"remaining":%d,"reset":%d,"status":1}`, cmp.Or(f.quotaLimit.Load(), 10000), f.quotaRemaining.Load(), f.quotaReset.Load())
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
		if u, _ := f.failUser.Load().(string); u != "" && form.Get("user") == u {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"status":0,"errors":["try later"]}`)
			return
		}
		if r.MultipartForm != nil && len(r.MultipartForm.File["attachment"]) > 0 {
			form.Set("_attachment", "yes")
		}
		f.mu.Lock()
		f.sent = append(f.sent, form)
		f.mu.Unlock()
		if limit := f.quotaLimit.Load(); limit > 0 {
			w.Header().Set("X-Limit-App-Limit", strconv.FormatInt(limit, 10))
			w.Header().Set("X-Limit-App-Remaining", strconv.FormatInt(f.quotaRemaining.Load(), 10))
			w.Header().Set("X-Limit-App-Reset", strconv.FormatInt(f.quotaReset.Load(), 10))
		}
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
	// siloCalls counts profile list requests; siloAuth records the
	// Authorization headers the fake Silo saw.
	siloCalls atomic.Int32
	siloMu    sync.Mutex
	siloAuth  []string
	siloDown  atomic.Bool
	// accounts are the fake Silo's accounts by ID, served as AdminUser rows
	// built from testdata/admin_user.json. userCalls counts lookups by ID.
	accounts  map[string]fakeAccount
	userCalls map[string]int
	// live is the fake Silo's live session list (AdminPlaybackSession rows);
	// devices and ips are each account's known device IDs and addresses
	// (ips as AdminUserIP rows); catalog maps content IDs to library IDs.
	// calls counts requests by path. failPaths answer 502.
	live      []map[string]any
	devices   map[string][]string
	ips       map[string][]map[string]any
	catalog   map[string][]string
	calls     map[string]int
	failPaths map[string]bool
	// static maps a path to the JSON the fake Silo answers it with (the
	// health and statistics routes).
	static map[string]any
	// lastQuery is the query string of the last request to each path.
	lastQuery map[string]string
}

type fakeAccount struct {
	username, role string
	enabled        bool
}

// testPoster is the image the fake Silo serves at /poster.jpg and /card.jpg.
var testPoster = []byte("\xff\xd8\xff\xe0 fake jpeg")

// Keys the fake Silo knows: the read-only key lists profiles, the
// wrong-scope key gets 403, anything else 401.
const (
	testMonitorKey    = "sa_monitor"
	testWrongScopeKey = "sa_wrong_scope"
	testLegacyKey     = "sa_legacy_full_access_secret"
)

// testNow is the time the fixtures' events happen around.
var testNow = time.Date(2026, 11, 3, 20, 0, 0, 0, time.UTC)

// testProfileNames are user 7's profiles in the fake Silo. p1 is primary. p2
// and p3 share a name, which 0.2.0's name matching mixed up.
var testProfileNames = map[string]string{"p1": "Josh", "p2": "Kid", "p3": "Kid"}

// newHarness wires a plugin to a fake Silo and a fake Pushover.
func newHarness(t *testing.T) *harness {
	t.Helper()
	fp := &fakePushover{}
	pushoverSrv := httptest.NewServer(http.HandlerFunc(fp.handler))
	t.Cleanup(pushoverSrv.Close)

	slowPoster := make(chan struct{})
	h := &harness{pushover: fp, pushURL: pushoverSrv.URL, userCalls: map[string]int{}, devices: map[string][]string{},
		ips: map[string][]map[string]any{}, catalog: map[string][]string{}, calls: map[string]int{}, failPaths: map[string]bool{}, static: map[string]any{}, lastQuery: map[string]string{}, accounts: map[string]fakeAccount{
			"7":  {"josh", "admin", true},
			"9":  {"sam", "admin", true},
			"12": {"alice", "user", true},
			"13": {"bob", "user", true},
		}}
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
		h.siloCalls.Add(1)
		h.siloMu.Lock()
		h.siloAuth = append(h.siloAuth, r.Header.Get("Authorization"))
		h.calls[r.URL.Path]++
		h.lastQuery[r.URL.Path] = r.URL.RawQuery
		failing := h.failPaths[r.URL.Path]
		staticBody, isStatic := h.static[r.URL.Path]
		h.siloMu.Unlock()
		switch {
		case failing:
			w.WriteHeader(http.StatusBadGateway)
			return
		case r.Method != http.MethodGet:
			w.WriteHeader(http.StatusMethodNotAllowed)
		case h.siloDown.Load():
			w.WriteHeader(http.StatusBadGateway)
		case r.Header.Get("Authorization") == "Bearer "+testWrongScopeKey:
			w.WriteHeader(http.StatusForbidden)
		case r.Header.Get("Authorization") != "Bearer "+testMonitorKey:
			w.WriteHeader(http.StatusUnauthorized)
		case isStatic:
			_ = json.NewEncoder(w).Encode(staticBody)
		case r.URL.Path == "/api/v2/admin/users/7/profiles":
			_, _ = io.WriteString(w, `{"items":[{"id":"p1","name":"Josh"},{"id":"p2","name":"Kid"},{"id":"p3","name":"Kid"}]}`)
		case r.URL.Path == "/api/v2/admin/sessions":
			h.siloMu.Lock()
			items := append([]map[string]any{}, h.live...)
			h.siloMu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "page": map[string]any{"has_more": false}})
		case r.URL.Path == "/api/v2/admin/catalog/items":
			id := r.URL.Query().Get("content_ids")
			h.siloMu.Lock()
			libs, ok := h.catalog[id]
			h.siloMu.Unlock()
			items := []map[string]any{}
			if ok {
				items = append(items, map[string]any{"content_id": id, "type": "episode", "title": "x", "library_ids": libs, "added_at": "2026-01-01T00:00:00Z", "versions": []any{}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
		case strings.HasPrefix(r.URL.Path, "/api/v2/admin/users/") && strings.HasSuffix(r.URL.Path, "/devices"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v2/admin/users/"), "/devices")
			h.siloMu.Lock()
			items := []map[string]any{}
			for _, d := range h.devices[id] {
				items = append(items, map[string]any{"device_id": d, "device_name": d, "device_platform": "ios", "last_seen_at": nil, "last_updated": nil, "override_count": 0, "profiles": []any{}})
			}
			h.siloMu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
		case strings.HasPrefix(r.URL.Path, "/api/v2/admin/users/") && strings.HasSuffix(r.URL.Path, "/ips"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v2/admin/users/"), "/ips")
			h.siloMu.Lock()
			items := append([]map[string]any{}, h.ips[id]...)
			h.siloMu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "page": map[string]any{"has_more": false}})
		case r.URL.Path == "/api/v2/admin/users":
			h.siloMu.Lock()
			items := []map[string]any{}
			for id := range h.accounts {
				items = append(items, h.accountRow(t, id))
			}
			h.siloMu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
		case strings.HasPrefix(r.URL.Path, "/api/v2/admin/users/"):
			id := strings.TrimPrefix(r.URL.Path, "/api/v2/admin/users/")
			h.siloMu.Lock()
			h.userCalls[id]++
			_, ok := h.accounts[id]
			var row map[string]any
			if ok {
				row = h.accountRow(t, id)
			}
			h.siloMu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(row)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(silo.Close)
	t.Cleanup(func() { close(slowPoster) })

	state := &memoryState{values: map[string][]byte{}}
	h.state, h.siloURL = state, silo.URL
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
	// Noise control, the tracker and the seen sets prune against the clock;
	// pin it to the fixtures' evening so the tests don't age.
	p.taskNow = func() time.Time { return testNow }
	p.watch.now = func() time.Time { return testNow }
	p.seen.now = func() time.Time { return testNow }
	p.listLibrariesFn = func(context.Context) ([]siloLibrary, error) {
		return []siloLibrary{{ID: "2", Name: "TV Shows"}, {ID: "1", Name: "Movies"}}, nil
	}
	p.setConfig(adminConfig{MonitorAPIKey: testMonitorKey, DefaultAppToken: testAppToken})
	return p
}

// siloAuthHeaders returns the Authorization headers the fake Silo has seen.
func (h *harness) siloAuthHeaders() []string {
	h.siloMu.Lock()
	defer h.siloMu.Unlock()
	return append([]string(nil), h.siloAuth...)
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

// viewerAs describes the headers Silo's proxy sends for one request.
type viewerAs struct {
	profileID string
	primary   bool
	admin     bool
}

var (
	asPrimary = viewerAs{profileID: "p1", primary: true, admin: true}
	asKid     = viewerAs{profileID: "p2"}
)

func (v viewerAs) headers() map[string]string {
	headers := map[string]string{"X-Silo-User-Id": "7", "X-Silo-User-Role": "user"}
	if v.profileID != "" {
		headers["X-Silo-Profile-Id"] = v.profileID
		headers["X-Silo-Profile-Name"] = testProfileNames[v.profileID]
	}
	if v.primary {
		headers["X-Silo-Profile-Primary"] = "true"
	}
	if v.admin {
		headers["X-Silo-User-Role"] = "admin"
	}
	return headers
}

func (h *harness) request(t *testing.T, method, path string, as viewerAs, body any) (int, map[string]any) {
	t.Helper()
	headers := as.headers()
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
	value, _ := structpb.NewStruct(map[string]any{
		"monitor_api_key": " sa_m ", "control_api_key": "sa_c", "default_app_token": "tok", "public_url": "https://s.example/",
	})
	cfg := parseAdminConfig([]*pluginv1.ConfigEntry{{Key: "other"}, {Key: adminConfigKey, Value: value}})
	want := adminConfig{MonitorAPIKey: "sa_m", ControlAPIKey: "sa_c", DefaultAppToken: "tok", PublicURL: "https://s.example"}
	if cfg != want {
		t.Fatalf("config = %+v, want %+v", cfg, want)
	}
}

// configureWith runs Configure the way Silo does after an upgrade from 0.2.0:
// the stored row still holds silo_api_key. Everything the plugin logs from
// then on goes to the returned buffer.
func configureWith(t *testing.T, h *harness, fields map[string]any) *syncBuffer {
	t.Helper()
	logs := &syncBuffer{}
	h.plugin.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	value, _ := structpb.NewStruct(fields)
	if err := h.plugin.configure(context.Background(), []*pluginv1.ConfigEntry{{Key: adminConfigKey, Value: value}}); err != nil {
		t.Fatal(err)
	}
	return logs
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestUpgradeIgnoresLegacyAPIKey(t *testing.T) {
	h := newHarness(t)
	logs := configureWith(t, h, map[string]any{"silo_api_key": testLegacyKey, "default_app_token": testAppToken})
	cfg := h.plugin.config()
	if !cfg.LegacyKeyStored || cfg.MonitorAPIKey != "" || cfg.DefaultAppToken != testAppToken {
		t.Fatalf("unexpected config after upgrade: %+v", cfg)
	}
	if !strings.Contains(logs.String(), "0.2.0") {
		t.Fatalf("expected a note about the stored 0.2.0 key, got %q", logs)
	}

	// The page works without the old key, and Silo never sees it.
	status, state := h.request(t, http.MethodGet, "/api/state", asPrimary, nil)
	if status != http.StatusOK || state["problem"] != nil {
		t.Fatalf("state after upgrade: %d %v", status, state)
	}
	if notice := state["notice"].(map[string]any); notice["code"] != "monitor_key_missing" {
		t.Fatalf("notice = %v, want monitor_key_missing", notice)
	}
	if status, _ := h.request(t, http.MethodPut, "/api/profiles/p1", asPrimary, map[string]any{"enabled": true, "user_key": testUserKey}); status != http.StatusOK {
		t.Fatalf("save after upgrade: status %d", status)
	}
	if n := h.siloCalls.Load(); n != 0 {
		t.Fatalf("Silo was called %d times without a read-only key", n)
	}

	// Nothing the plugin logs or returns contains the key, even with the
	// config itself logged.
	h.plugin.logger.Info("config", "config", h.plugin.config())
	_, body := h.request(t, http.MethodGet, "/api/state", asPrimary, nil)
	raw, _ := json.Marshal(body)
	for _, out := range []string{logs.String(), string(raw)} {
		if strings.Contains(out, testLegacyKey) {
			t.Fatal("the legacy key leaked")
		}
	}
}

func TestConfigNeverLogsKeys(t *testing.T) {
	var logs strings.Builder
	cfg := adminConfig{MonitorAPIKey: testMonitorKey, ControlAPIKey: "sa_control_secret", DefaultAppToken: testAppToken}
	slog.New(slog.NewTextHandler(&logs, nil)).Info("x", "config", cfg)
	for _, secret := range []string{testMonitorKey, "sa_control_secret", testAppToken} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("logged config contains a key: %s", logs.String())
		}
	}
}

// TestManifestUpgradeSchema checks the manifest the way Silo does: it must
// load, put every admin field in a section, no longer declare silo_api_key,
// and accept blank optional keys. Silo validates only the declared
// fields of a save (globalConfigValidationProjection in silo-server
// internal/plugins/service_admin_config.go), so the stored silo_api_key
// doesn't trip additionalProperties: false.
func TestManifestUpgradeSchema(t *testing.T) {
	manifest, err := publicmanifest.LoadWithChecksum(manifestJSON, "")
	if err != nil {
		t.Fatalf("manifest doesn't load: %v", err)
	}
	var schema *pluginv1.ConfigSchema
	for _, s := range manifest.GetGlobalConfigSchema() {
		if s.GetKey() == adminConfigKey {
			schema = s
		}
	}
	if schema == nil {
		t.Fatal("no pushover config schema")
	}
	if strings.Contains(schema.GetJsonSchema(), legacyAPIKeyField) {
		t.Fatal("json_schema still declares silo_api_key")
	}
	inSection := map[string]bool{}
	for _, s := range schema.GetAdminForm().GetSections() {
		for _, k := range s.GetFieldKeys() {
			inSection[k] = true
		}
	}
	for _, f := range schema.GetAdminForm().GetFields() {
		if !inSection[f.GetKey()] {
			t.Errorf("field %s is in no section", f.GetKey())
		}
		if f.GetKey() == legacyAPIKeyField {
			t.Error("admin form still has silo_api_key")
		}
		if strings.HasSuffix(f.GetKey(), "_api_key") && (!f.GetSecret() || f.GetRequired()) {
			t.Errorf("%s should be an optional secret", f.GetKey())
		}
	}
	// Every form field is a plain, unconstrained string in the schema, so a
	// blank optional key saves.
	var doc struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(schema.GetJsonSchema()), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Properties) != len(schema.GetAdminForm().GetFields()) {
		t.Fatalf("schema declares %d properties for %d form fields", len(doc.Properties), len(schema.GetAdminForm().GetFields()))
	}
	for _, f := range schema.GetAdminForm().GetFields() {
		prop, ok := doc.Properties[f.GetKey()]
		if !ok || len(prop) != 1 || prop["type"] != "string" {
			t.Errorf("schema property %s = %v, want {type: string}", f.GetKey(), prop)
		}
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

func TestStoreGetReadsOnlyItsBucket(t *testing.T) {
	state := &countingState{memoryState: memoryState{values: map[string][]byte{}}}
	ctx := context.Background()
	if err := newProfileStore(state).Put(ctx, &ProfileSettings{ProfileID: "p1", UserKey: "k1"}); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	s := newProfileStore(state)
	s.now = func() time.Time { return now }
	state.reads.Store(0)
	if got, found, err := s.Get(ctx, "p1"); err != nil || !found || got.UserKey != "k1" {
		t.Fatalf("cold Get = %v, %v, %v", got, found, err)
	}
	if n := state.reads.Load(); n != 1 {
		t.Fatalf("cold Get made %d state reads, want 1", n)
	}
	if _, _, err := s.Get(ctx, "p1"); err != nil || state.reads.Load() != 1 {
		t.Fatalf("Get inside the TTL should use the cache (reads %d, err %v)", state.reads.Load(), err)
	}
	now = now.Add(settingsCacheTTL)
	if _, _, err := s.Get(ctx, "p1"); err != nil || state.reads.Load() != 2 {
		t.Fatalf("Get after the TTL should re-read one bucket (reads %d, err %v)", state.reads.Load(), err)
	}
}

// countingState is a memoryState that counts reads.
type countingState struct {
	memoryState
	reads atomic.Int32
}

func (c *countingState) Read(ctx context.Context, key string) ([]byte, bool, error) {
	c.reads.Add(1)
	return c.memoryState.Read(ctx, key)
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

// profileIDs lists the IDs in a state response.
func profileIDs(state map[string]any) []string {
	var ids []string
	list, _ := state["profiles"].([]any)
	for _, p := range list {
		ids = append(ids, p.(map[string]any)["id"].(string))
	}
	return ids
}

func TestSecondaryProfileSeesOnlyItself(t *testing.T) {
	h := newHarness(t)
	status, state := h.request(t, http.MethodGet, "/api/state", asKid, nil)
	if status != http.StatusOK {
		t.Fatalf("state status %d: %v", status, state)
	}
	if ids := profileIDs(state); len(ids) != 1 || ids[0] != "p2" {
		t.Fatalf("Kid should only see p2, got %v", ids)
	}
	if state["notice"] != nil {
		t.Fatalf("a secondary profile shouldn't get household notices: %v", state["notice"])
	}
	for _, path := range []string{"/api/profiles/p1", "/api/profiles/p3"} {
		if status, _ := h.request(t, http.MethodGet, path, asKid, nil); status != http.StatusNotFound {
			t.Fatalf("Kid reading %s: status %d, want 404", path, status)
		}
	}
	if n := h.siloCalls.Load(); n != 0 {
		t.Fatalf("a secondary profile needs no Silo call, made %d", n)
	}
}

// Two profiles named "Kid": each is told apart by its ID, so saving as p3
// doesn't touch p2 (0.2.0 matched by name and picked p2 for both).
func TestIdentityComesFromProfileIDHeader(t *testing.T) {
	h := newHarness(t)
	asKid2 := viewerAs{profileID: "p3"}
	status, state := h.request(t, http.MethodGet, "/api/state", asKid2, nil)
	if ids := profileIDs(state); status != http.StatusOK || len(ids) != 1 || ids[0] != "p3" {
		t.Fatalf("second Kid: status %d, profiles %v", status, ids)
	}
	if status, body := h.request(t, http.MethodPut, "/api/profiles/p3", asKid2, map[string]any{"enabled": true, "user_key": testUserKey}); status != http.StatusOK {
		t.Fatalf("save: %d %v", status, body)
	}
	if _, found, _ := h.plugin.store.Get(context.Background(), "p2"); found {
		t.Fatal("saving as p3 wrote p2's settings")
	}
	if s, found, _ := h.plugin.store.Get(context.Background(), "p3"); !found || s.UserID != 7 || s.ProfileName != "Kid" {
		t.Fatalf("p3 settings = %+v", s)
	}

	// No profile ID: the page asks the viewer to pick a profile.
	status, state = h.request(t, http.MethodGet, "/api/state", viewerAs{}, nil)
	if problem, _ := state["problem"].(map[string]any); status != http.StatusOK || problem["code"] != "no_profile" {
		t.Fatalf("no profile: %d %v", status, state)
	}
}

func TestPrimaryManagesHouseholdWithMonitorKey(t *testing.T) {
	h := newHarness(t)
	status, state := h.request(t, http.MethodGet, "/api/state", asPrimary, nil)
	if status != http.StatusOK || state["household"] != "all" || state["notice"] != nil {
		t.Fatalf("state: %d %v", status, state)
	}
	if ids := profileIDs(state); strings.Join(ids, ",") != "p1,p2,p3" {
		t.Fatalf("primary profiles = %v, want p1,p2,p3 (active first)", ids)
	}
	if status, body := h.request(t, http.MethodPut, "/api/profiles/p2", asPrimary, map[string]any{"enabled": true, "user_key": testUserKey}); status != http.StatusOK {
		t.Fatalf("primary saving p2: %d %v", status, body)
	}
	// The profile list is cached, and only the read-only key is sent.
	if n := h.siloCalls.Load(); n != 1 {
		t.Fatalf("Silo profile list called %d times, want 1 (cached)", n)
	}
	for _, auth := range h.siloAuthHeaders() {
		if auth != "Bearer "+testMonitorKey {
			t.Fatalf("Silo got Authorization %q", auth)
		}
	}
}

func TestPrimaryWithoutKeySeesConfiguredProfiles(t *testing.T) {
	h := newHarness(t)
	h.plugin.setConfig(adminConfig{DefaultAppToken: testAppToken})
	ctx := context.Background()
	// p2 set itself up; p9 belongs to another account.
	if status, _ := h.request(t, http.MethodPut, "/api/profiles/p2", asKid, map[string]any{"enabled": true, "user_key": testUserKey}); status != http.StatusOK {
		t.Fatalf("Kid saving p2: %d", status)
	}
	if err := h.plugin.store.Put(ctx, &ProfileSettings{ProfileID: "p9", ProfileName: "Neighbour", UserID: 8, UserKey: testUserKey}); err != nil {
		t.Fatal(err)
	}

	status, state := h.request(t, http.MethodGet, "/api/state", asPrimary, nil)
	if status != http.StatusOK || state["household"] != "configured" {
		t.Fatalf("state: %d %v", status, state)
	}
	if ids := profileIDs(state); strings.Join(ids, ",") != "p1,p2" {
		t.Fatalf("primary profiles = %v, want p1,p2", ids)
	}
	notice := state["notice"].(map[string]any)
	if notice["code"] != "monitor_key_missing" || notice["level"] != "info" || !strings.Contains(notice["message"].(string), "admin:monitor:read") {
		t.Fatalf("notice = %v", notice)
	}
	for path, want := range map[string]int{"/api/profiles/p2": http.StatusOK, "/api/profiles/p3": http.StatusNotFound, "/api/profiles/p9": http.StatusNotFound} {
		if status, _ := h.request(t, http.MethodGet, path, asPrimary, nil); status != want {
			t.Fatalf("primary GET %s: %d, want %d", path, status, want)
		}
	}

	// A primary profile that isn't an admin isn't told to change admin settings.
	_, state = h.request(t, http.MethodGet, "/api/state", viewerAs{profileID: "p1", primary: true}, nil)
	if msg := state["notice"].(map[string]any)["message"].(string); strings.Contains(msg, adminFormPath) {
		t.Fatalf("non-admin notice mentions the admin form: %q", msg)
	}
	if n := h.siloCalls.Load(); n != 0 {
		t.Fatalf("Silo called %d times without a key", n)
	}
}

func TestHouseholdKeyProblemsShowNotices(t *testing.T) {
	cases := []struct {
		name, key, code string
		down            bool
	}{
		{"rejected", "sa_revoked", "monitor_key_rejected", false},
		{"wrong scope", testWrongScopeKey, "monitor_key_scope", false},
		{"Silo error", testMonitorKey, "silo_unavailable", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.plugin.setConfig(adminConfig{MonitorAPIKey: c.key, DefaultAppToken: testAppToken})
			h.siloDown.Store(c.down)
			h.configure(t, func(s *ProfileSettings) { s.ProfileID, s.ProfileName, s.UserID = "p2", "Kid", 7 })

			for _, as := range []viewerAs{asPrimary, {profileID: "p1", primary: true}} {
				status, state := h.request(t, http.MethodGet, "/api/state", as, nil)
				if status != http.StatusOK || state["household"] != "configured" {
					t.Fatalf("state: %d %v", status, state)
				}
				if ids := profileIDs(state); strings.Join(ids, ",") != "p1,p2" {
					t.Fatalf("fallback profiles = %v, want p1,p2", ids)
				}
				notice := state["notice"].(map[string]any)
				if notice["code"] != c.code || notice["level"] != "warn" {
					t.Fatalf("notice = %v, want %s", notice, c.code)
				}
				mentionsForm := strings.Contains(notice["message"].(string), adminFormPath)
				if c.code != "silo_unavailable" && mentionsForm != as.admin {
					t.Fatalf("admin=%v but message mentioning the admin form = %v: %q", as.admin, mentionsForm, notice["message"])
				}
			}
		})
	}
}

func TestSiloClientCachesKeyErrors(t *testing.T) {
	h := newHarness(t)
	c := h.plugin.silo
	ctx := context.Background()
	for range 2 {
		if _, err := c.AccountProfiles(ctx, h.siloURL, "sa_revoked", 7); !errors.Is(err, errSiloKeyRejected) {
			t.Fatalf("err = %v, want errSiloKeyRejected", err)
		}
	}
	if n := h.siloCalls.Load(); n != 1 {
		t.Fatalf("rejected key called Silo %d times, want 1 (cached)", n)
	}
	// A key change clears the cache.
	c.Invalidate()
	if _, err := c.AccountProfiles(ctx, h.siloURL, testWrongScopeKey, 7); !errors.Is(err, errSiloKeyScope) {
		t.Fatalf("err = %v, want errSiloKeyScope", err)
	}
	if _, err := c.AccountProfiles(ctx, h.siloURL, "", 7); !errors.Is(err, errSiloKeyMissing) {
		t.Fatalf("err = %v, want errSiloKeyMissing", err)
	}
	// Other failures (Silo down, network) aren't cached.
	h.siloDown.Store(true)
	c.Invalidate()
	for range 2 {
		if _, err := c.AccountProfiles(ctx, h.siloURL, testMonitorKey, 7); err == nil {
			t.Fatal("want an error while Silo is down")
		}
	}
	if n := h.siloCalls.Load(); n != 4 {
		t.Fatalf("Silo calls = %d, want 4", n)
	}
}

func TestCheckKeyHonorsRemovedToken(t *testing.T) {
	h := newHarness(t)
	badToken := "bad" + testAppToken[3:]
	h.configure(t, func(s *ProfileSettings) { s.ProfileID, s.ProfileName, s.UserID, s.AppToken = "p2", "Kid", 7, badToken })

	if status, _ := h.request(t, http.MethodPost, "/api/profiles/p2/validate", asKid, map[string]any{}); status != http.StatusUnprocessableEntity {
		t.Fatalf("check with the saved bad token: %d, want 422", status)
	}
	// "Remove my saved token" then "Check key" checks the shared token.
	if status, body := h.request(t, http.MethodPost, "/api/profiles/p2/validate", asKid, map[string]any{"clear_app_token": true}); status != http.StatusOK {
		t.Fatalf("check with the token removed: %d %v", status, body)
	}
}

func TestSaveValidatesAndMasksKeys(t *testing.T) {
	h := newHarness(t)
	status, body := h.request(t, http.MethodPut, "/api/profiles/p2", asKid, map[string]any{"enabled": true, "user_key": "bad" + testUserKey[3:]})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("bad key: status %d, want 422 (%v)", status, body)
	}

	status, body = h.request(t, http.MethodPut, "/api/profiles/p2", asKid, map[string]any{"enabled": true, "user_key": testUserKey, "sound": "mychime"})
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
	status, _ = h.request(t, http.MethodPut, "/api/profiles/p2", asKid, map[string]any{"enabled": false})
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
	h.plugin.setConfig(adminConfig{})
	status, _ := h.request(t, http.MethodPut, "/api/profiles/p2", asKid, map[string]any{"enabled": true, "user_key": testUserKey})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422", status)
	}
	status, _ = h.request(t, http.MethodPut, "/api/profiles/p2", asKid, map[string]any{"enabled": true, "user_key": testUserKey, "app_token": testAppToken})
	if status != http.StatusOK {
		t.Fatalf("status %d with own token, want 200", status)
	}
}

func TestMutationsRequireJSON(t *testing.T) {
	h := newHarness(t)
	headers := asPrimary.headers()
	headers["Content-Type"] = "application/x-www-form-urlencoded"
	resp, _ := h.routes.Handle(context.Background(), &pluginv1.HandleHTTPRequest{
		Method: http.MethodPut, Path: "/api/profiles/p1", Headers: headers,
	})
	if resp.GetStatusCode() != http.StatusUnsupportedMediaType {
		t.Fatalf("status %d, want 415", resp.GetStatusCode())
	}
}
