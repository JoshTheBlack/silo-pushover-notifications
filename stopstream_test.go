package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimehost"
)

const (
	testControlKey = "sa_control_secret_key"
	testRevokedKey = "sa_revoked_control_key"
)

var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// sentCommand is one command the fake Silo received.
type sentCommand struct {
	session, action, auth string
	body                  map[string]any
}

// fakeControlSilo serves the routes the stop-stream page uses: the live
// session list and account lookups with the read-only key, and the sequenced
// stop and message commands with the stream-control key, following
// silo-server's admin_playback_commands.go.
type fakeControlSilo struct {
	t  *testing.T
	mu sync.Mutex
	// live maps session IDs to AdminPlaybackSession rows; realtime lists
	// the sessions with a live player connection (messages need one).
	live     map[string]map[string]any
	realtime map[string]bool
	latest   map[string]int64
	commands []sentCommand
	// refusals counts answers other than 2xx to commands.
	refusals int
}

func newFakeControlSilo(t *testing.T) (*fakeControlSilo, *httptest.Server) {
	f := &fakeControlSilo{t: t, live: map[string]map[string]any{}, realtime: map[string]bool{}, latest: map[string]int64{}}
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeControlSilo) addLive(t *testing.T, realtime bool) {
	row := liveRow(t, "session_started", nil)
	id := row["session_id"].(string)
	f.mu.Lock()
	f.live[id], f.realtime[id] = row, realtime
	f.mu.Unlock()
}

func (f *fakeControlSilo) sent() []sentCommand {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentCommand(nil), f.commands...)
}

var commandPath = regexp.MustCompile(`^/api/v2/admin/sessions/([^/]+)/(stop|message)$`)

func (f *fakeControlSilo) handle(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodGet {
		if auth != "Bearer "+testMonitorKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/api/v2/admin/sessions":
			items := []map[string]any{}
			for _, row := range f.live {
				items = append(items, row)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "page": map[string]any{"has_more": false}})
		case r.URL.Path == "/api/v2/admin/users/7":
			row := fixture(f.t, "admin_user")
			row["id"], row["username"], row["role"], row["enabled"] = "7", "josh", "admin", true
			_ = json.NewEncoder(w).Encode(row)
		default:
			http.NotFound(w, r)
		}
		return
	}
	m := commandPath.FindStringSubmatch(r.URL.Path)
	if r.Method != http.MethodPost || m == nil {
		http.NotFound(w, r)
		return
	}
	var body map[string]any
	if r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&body) != nil {
		problem(w, http.StatusUnprocessableEntity, "bad body")
		return
	}
	f.commands = append(f.commands, sentCommand{session: m[1], action: m[2], auth: auth, body: body})
	refuse := func(status int, detail string) {
		f.refusals++
		problem(w, status, detail)
	}
	switch auth {
	case "Bearer " + testControlKey:
	case "Bearer " + testMonitorKey:
		refuse(http.StatusForbidden, "scope")
		return
	default:
		refuse(http.StatusUnauthorized, "unauthorized")
		return
	}
	id, _ := body["command_id"].(string)
	seq, _ := body["sequence"].(float64)
	if !canonicalUUID.MatchString(id) || seq < 1 || seq > maxCommandSequence {
		refuse(http.StatusUnprocessableEntity, "The command identity or content is invalid.")
		return
	}
	if _, ok := f.live[m[1]]; !ok {
		refuse(http.StatusNotFound, "Playback session not found.")
		return
	}
	if int64(seq) <= f.latest[m[1]] {
		w.Header().Set(latestSequenceHeader, strconv.FormatInt(f.latest[m[1]], 10))
		refuse(http.StatusConflict, "The command sequence is behind the session's latest applied command; allocate a newer sequence.")
		return
	}
	delivery := "dispatched"
	switch m[2] {
	case "message":
		if msg, _ := body["message"].(string); msg == "" {
			refuse(http.StatusUnprocessableEntity, "message required")
			return
		}
		if !f.realtime[m[1]] {
			refuse(http.StatusConflict, "The playback session has no realtime connection for this command.")
			return
		}
	case "stop":
		if !f.realtime[m[1]] {
			delivery = "fallback_scheduled"
		}
	}
	f.latest[m[1]] = int64(seq)
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"command_id": id, "sequence": int64(seq), "outcome": "applied", "delivery": delivery})
}

type stopHarness struct {
	*harness
	silo *fakeControlSilo
	logs *syncBuffer
}

func newStopHarness(t *testing.T, controlKey string) *stopHarness {
	t.Helper()
	h := newHarness(t)
	f, srv := newFakeControlSilo(t)
	h.plugin.hostInfoFn = func(context.Context) (*runtimehost.HostInfo, error) {
		return &runtimehost.HostInfo{PublicBaseURL: "https://silo.example.com", InternalBaseURL: srv.URL}, nil
	}
	logs := &syncBuffer{}
	h.plugin.logger = slog.New(slog.NewTextHandler(logs, nil))
	h.plugin.setConfig(adminConfig{MonitorAPIKey: testMonitorKey, ControlAPIKey: controlKey, DefaultAppToken: testAppToken})
	h.plugin.commands.gap = 0
	return &stopHarness{harness: h, silo: f, logs: logs}
}

// stop posts to the stop route.
func (sh *stopHarness) stop(t *testing.T, as viewerAs, id string, body any) (int, map[string]any) {
	t.Helper()
	return sh.request(t, http.MethodPost, "/api/admin/sessions/"+id+"/stop", as, body)
}

// assertNoKeys fails if a key shows up in a response or the log.
func (sh *stopHarness) assertNoKeys(t *testing.T, bodies ...map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(bodies)
	for _, key := range []string{testControlKey, testRevokedKey, testMonitorKey} {
		if strings.Contains(string(raw), key) || strings.Contains(sh.logs.String(), key) {
			t.Fatalf("a key leaked into a response or the log")
		}
	}
}

func TestStopPageIsAdminOnlyAndNeverActs(t *testing.T) {
	sh := newStopHarness(t, testControlKey)
	sh.silo.addLive(t, true)
	for _, path := range []string{"/admin/sessions", "/admin/sessions/", "/admin/sessions/sess-ep1"} {
		resp, err := sh.routes.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: http.MethodGet, Path: path, Headers: asPrimary.headers()})
		if err != nil || resp.GetStatusCode() != http.StatusOK || !strings.Contains(string(resp.GetBody()), "Stop with message") {
			t.Fatalf("%s: status %d, err %v", path, resp.GetStatusCode(), err)
		}
		resp, _ = sh.routes.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: http.MethodGet, Path: path, Headers: viewerAs{profileID: "p1", primary: true}.headers()})
		if resp.GetStatusCode() != http.StatusForbidden || strings.Contains(string(resp.GetBody()), "<script") {
			t.Fatalf("%s for a non-admin: status %d", path, resp.GetStatusCode())
		}
	}
	resp, _ := sh.routes.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: http.MethodPost, Path: "/admin/sessions/sess-ep1", Headers: asPrimary.headers()})
	if resp.GetStatusCode() != http.StatusMethodNotAllowed {
		t.Fatalf("POST to the page: status %d", resp.GetStatusCode())
	}

	status, body := sh.request(t, http.MethodGet, "/api/admin/sessions/sess-ep1", asPrimary, nil)
	if status != http.StatusOK || body["ended"] != false || body["control_key_set"] != true {
		t.Fatalf("session: %d %v", status, body)
	}
	s := body["session"].(map[string]any)
	if s["user"] != "alice (Kids)" || s["title"] != "Severance S02E01" || s["device"] != "Living Room TV" || s["can_message"] != true {
		t.Fatalf("session view: %v", s)
	}
	status, body = sh.request(t, http.MethodGet, "/api/admin/sessions", asPrimary, nil)
	if status != http.StatusOK || len(body["sessions"].([]any)) != 1 {
		t.Fatalf("list: %d %v", status, body)
	}
	status, body = sh.request(t, http.MethodGet, "/api/admin/sessions/sess-gone", asPrimary, nil)
	if status != http.StatusOK || body["ended"] != true {
		t.Fatalf("ended session: %d %v", status, body)
	}
	if got := sh.silo.sent(); len(got) != 0 {
		t.Fatalf("reading sent commands: %v", got)
	}
	// GETs never stop anything, even on the stop route.
	if status, _ := sh.request(t, http.MethodGet, "/api/admin/sessions/sess-ep1/stop", asPrimary, nil); status != http.StatusMethodNotAllowed {
		t.Fatalf("GET stop: status %d", status)
	}
	for _, as := range []viewerAs{{profileID: "p1", primary: true}, asKid} {
		if status, _ := sh.request(t, http.MethodGet, "/api/admin/sessions/sess-ep1", as, nil); status != http.StatusForbidden {
			t.Fatalf("non-admin read: status %d", status)
		}
	}
	if got := sh.silo.sent(); len(got) != 0 {
		t.Fatalf("commands sent without a POST: %v", got)
	}
	sh.assertNoKeys(t, body)
}

func TestStopSendsSequencedStop(t *testing.T) {
	sh := newStopHarness(t, testControlKey)
	sh.silo.addLive(t, true)
	status, body := sh.stop(t, asPrimary, "sess-ep1", map[string]any{})
	if status != http.StatusOK || body["ok"] != true || body["level"] != "ok" {
		t.Fatalf("stop: %d %v", status, body)
	}
	sent := sh.silo.sent()
	if len(sent) != 1 || sent[0].action != "stop" || sent[0].session != "sess-ep1" || sent[0].auth != "Bearer "+testControlKey {
		t.Fatalf("commands: %+v", sent)
	}
	if _, ok := sent[0].body["reason"]; ok {
		t.Fatalf("a plain stop sent a reason: %v", sent[0].body)
	}
	if seq := int64(sent[0].body["sequence"].(float64)); seq < time.Now().Add(-time.Minute).UnixMilli() {
		t.Fatalf("sequence %d isn't clock-based", seq)
	}
	// A second stop gets a new identity above the first.
	sh.stop(t, asPrimary, "sess-ep1", map[string]any{})
	sent = sh.silo.sent()
	if len(sent) != 2 || sent[1].body["command_id"] == sent[0].body["command_id"] || sent[1].body["sequence"].(float64) <= sent[0].body["sequence"].(float64) {
		t.Fatalf("second stop: %+v", sent)
	}
	sh.assertNoKeys(t, body)
}

func TestStopWithMessageShowsItFirst(t *testing.T) {
	sh := newStopHarness(t, testControlKey)
	sh.silo.addLive(t, true)
	status, body := sh.stop(t, asPrimary, "sess-ep1", map[string]any{"message": "  Server restart in 5 minutes.  "})
	if status != http.StatusOK || body["level"] != "ok" {
		t.Fatalf("stop: %d %v", status, body)
	}
	sent := sh.silo.sent()
	if len(sent) != 2 || sent[0].action != "message" || sent[1].action != "stop" {
		t.Fatalf("commands: %+v", sent)
	}
	if sent[0].body["message"] != "Server restart in 5 minutes." || sent[0].body["title"] != stopMessageTitle {
		t.Fatalf("message body: %v", sent[0].body)
	}
	if sent[1].body["reason"] != "Server restart in 5 minutes." || sent[1].body["sequence"].(float64) <= sent[0].body["sequence"].(float64) {
		t.Fatalf("stop body: %v", sent[1].body)
	}
}

func TestStopWithMessageWithoutLiveConnection(t *testing.T) {
	sh := newStopHarness(t, testControlKey)
	sh.silo.addLive(t, false)
	status, body := sh.stop(t, asPrimary, "sess-ep1", map[string]any{"message": "Bye"})
	if status != http.StatusOK || body["level"] != "warn" || body["title"] != "Stopping the stream." {
		t.Fatalf("stop: %d %v", status, body)
	}
	if d := details(body); !strings.Contains(d, "wasn't shown") || !strings.Contains(d, "within a few seconds") {
		t.Fatalf("details: %s", d)
	}
	if sent := sh.silo.sent(); len(sent) != 2 || sent[1].action != "stop" {
		t.Fatalf("commands: %+v", sent)
	}
}

func TestStopRetriesAboveStaleSequence(t *testing.T) {
	sh := newStopHarness(t, testControlKey)
	sh.silo.addLive(t, true)
	ahead := time.Now().Add(time.Hour).UnixMilli()
	sh.silo.mu.Lock()
	sh.silo.latest["sess-ep1"] = ahead
	sh.silo.mu.Unlock()
	status, body := sh.stop(t, asPrimary, "sess-ep1", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("stop: %d %v", status, body)
	}
	sent := sh.silo.sent()
	if len(sent) != 2 || int64(sent[1].body["sequence"].(float64)) != ahead+1 || sent[1].body["command_id"] == sent[0].body["command_id"] {
		t.Fatalf("commands: %+v", sent)
	}
}

func TestStopEndedStream(t *testing.T) {
	sh := newStopHarness(t, testControlKey)
	for _, body := range []map[string]any{{}, {"message": "hi"}} {
		status, out := sh.stop(t, asPrimary, "sess-gone", body)
		if status != http.StatusNotFound || out["code"] != "session_ended" || !strings.Contains(out["message"].(string), "already ended") {
			t.Fatalf("ended: %d %v", status, out)
		}
	}
	if sent := sh.silo.sent(); len(sent) != 2 {
		t.Fatalf("a message to an ended stream shouldn't be followed by a stop: %+v", sent)
	}
	// IDs that could change the request path never reach Silo.
	if status, out := sh.stop(t, asPrimary, "..%2f..", map[string]any{}); status != http.StatusNotFound || out["code"] != "session_ended" {
		t.Fatalf("bad ID: %d %v", status, out)
	}
	if sent := sh.silo.sent(); len(sent) != 2 {
		t.Fatalf("bad ID reached Silo: %+v", sent)
	}
}

func TestStopReportsRefusedControlKey(t *testing.T) {
	for _, tc := range []struct{ key, code string }{
		{testRevokedKey, "control_key_rejected"},
		{testMonitorKey, "control_key_scope"},
	} {
		sh := newStopHarness(t, tc.key)
		sh.silo.addLive(t, true)
		status, body := sh.stop(t, asPrimary, "sess-ep1", map[string]any{"message": "x"})
		if status != http.StatusConflict || body["code"] != tc.code || !strings.Contains(body["message"].(string), "wasn't stopped") {
			t.Fatalf("%s: %d %v", tc.code, status, body)
		}
		if sent := sh.silo.sent(); len(sent) != 1 {
			t.Fatalf("%s: commands %+v", tc.code, sent)
		}
		sh.assertNoKeys(t, body)
	}
}

func TestStopNeedsAdminJSONAndControlKey(t *testing.T) {
	sh := newStopHarness(t, "")
	sh.silo.addLive(t, true)
	status, body := sh.stop(t, asPrimary, "sess-ep1", map[string]any{})
	if status != http.StatusConflict || body["code"] != "control_key_missing" {
		t.Fatalf("no key: %d %v", status, body)
	}
	_, body = sh.request(t, http.MethodGet, "/api/admin/sessions/sess-ep1", asPrimary, nil)
	if body["control_key_set"] != false {
		t.Fatalf("control_key_set: %v", body)
	}

	sh.plugin.setConfig(adminConfig{MonitorAPIKey: testMonitorKey, ControlAPIKey: testControlKey})
	for _, as := range []viewerAs{{profileID: "p1", primary: true}, asKid} {
		if status, _ := sh.stop(t, as, "sess-ep1", map[string]any{}); status != http.StatusForbidden {
			t.Fatalf("non-admin stop: status %d", status)
		}
	}
	resp, _ := sh.routes.Handle(context.Background(), &pluginv1.HandleHTTPRequest{
		Method: http.MethodPost, Path: "/api/admin/sessions/sess-ep1/stop", Headers: asPrimary.headers(), Body: []byte("message=hi"),
	})
	if resp.GetStatusCode() != http.StatusUnsupportedMediaType {
		t.Fatalf("form post: status %d", resp.GetStatusCode())
	}
	if status, body := sh.stop(t, asPrimary, "sess-ep1", map[string]any{"message": strings.Repeat("é", maxStopMessage+1)}); status != http.StatusUnprocessableEntity {
		t.Fatalf("long message: %d %v", status, body)
	}
	if sent := sh.silo.sent(); len(sent) != 0 {
		t.Fatalf("refused requests sent commands: %+v", sent)
	}
}

func TestCommandIdentity(t *testing.T) {
	for range 100 {
		if id := newCommandID(); !canonicalUUID.MatchString(id) {
			t.Fatalf("not a canonical v4 UUID: %s", id)
		}
	}
	c := newSessionCommander()
	fixed := time.UnixMilli(1_000)
	c.now = func() time.Time { return fixed }
	if a, b := c.allocate("s", 0), c.allocate("s", 0); a != 1_000 || b != 1_001 {
		t.Fatalf("allocate: %d %d", a, b)
	}
	if got := c.allocate("s", 5_000); got != 5_001 {
		t.Fatalf("allocate above floor: %d", got)
	}
	if got := c.allocate("other", 0); got != 1_000 {
		t.Fatalf("other session: %d", got)
	}
}

func TestManifestStopStreamRoutes(t *testing.T) {
	var m struct {
		Routes []struct {
			ID, Method, Path, Access string
			Navigable                bool `json:"navigable"`
		} `json:"http_routes"`
	}
	if err := json.Unmarshal(manifestJSON, &m); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"stop-stream-page": "/admin/sessions/*", "stop-stream-api": "/api/admin/sessions/*"}
	for _, r := range m.Routes {
		if path, ok := want[r.ID]; ok {
			if r.Path != path || r.Access != "admin" || r.Navigable {
				t.Fatalf("route %s: %+v", r.ID, r)
			}
			delete(want, r.ID)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing routes: %v", want)
	}
}
