package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// liveRow is a fixture's session row as Silo's live list shows it, with
// fields changed.
func liveRow(t *testing.T, file string, fields map[string]any) map[string]any {
	t.Helper()
	row := maps.Clone(fixture(t, file)["session"].(map[string]any))
	maps.Copy(row, fields)
	return row
}

func (h *harness) setLive(rows ...map[string]any) {
	h.siloMu.Lock()
	h.live = rows
	h.siloMu.Unlock()
}

func (h *harness) callsTo(path string) int {
	h.siloMu.Lock()
	defer h.siloMu.Unlock()
	return h.calls[path]
}

func (h *harness) failPath(path string, fail bool) {
	h.siloMu.Lock()
	h.failPaths[path] = fail
	h.siloMu.Unlock()
}

// onlyAlerts switches on just the named alerts, with noise mode "every".
func onlyAlerts(keys ...string) func(*AdminSettings) {
	return func(a *AdminSettings) {
		for k, s := range a.Alerts {
			s.Enabled = false
			a.Alerts[k] = s
		}
		for _, k := range keys {
			s := a.alert(k)
			s.Enabled = true
			a.Alerts[k] = s
		}
	}
}

func and(fns ...func(*AdminSettings)) func(*AdminSettings) {
	return func(a *AdminSettings) {
		for _, fn := range fns {
			fn(a)
		}
	}
}

// startOf is alice's episode start for another session ID and device.
func startOf(t *testing.T, eventID, session string, minutes int, fields map[string]any) *pluginv1.HandleEventRequest {
	t.Helper()
	return sessionEvent(t, eventSessionStarted, eventID, "session_started", edits(asSession(session), at(minutes), withRow(fields)))
}

func TestServerStreamLimitHasHysteresis(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, and(onlyAlerts(alertStreamsServer), func(a *AdminSettings) { a.Limits.ServerStreams = 1 }))
	ep := liveRow(t, "session_started", nil)
	movie := liveRow(t, "session_started_local_movie", nil)
	third := liveRow(t, "session_started", map[string]any{"session_id": "s3"})

	h.setLive(ep)
	h.mustHandle(t, sessionEvent(t, eventSessionStarted, "e1", "session_started", nil))
	if n := len(h.pushover.messages()); n != 0 {
		t.Fatalf("one stream with a limit of one sent %d alerts", n)
	}

	h.setLive(ep, movie)
	h.mustHandle(t, sessionEvent(t, eventSessionStarted, "e2", "session_started_local_movie", nil))
	sent := h.sentTo(testUserKey)
	if len(sent) != 1 || sent[0].Get("title") != "2 streams playing" {
		t.Fatalf("crossing the limit sent %q", titles(sent))
	}
	msg := sent[0].Get("message")
	if !strings.HasPrefix(msg, "The limit is 1.\n") || !strings.Contains(msg, "alice (Kids) · Severance S02E01 · Living Room TV · 8.0 Mbps") || !strings.Contains(msg, "bob · Dune: Part Two") {
		t.Fatalf("message = %q", msg)
	}

	// Still over: no second alert.
	h.setLive(ep, movie, third)
	h.mustHandle(t, startOf(t, "e3", "s3", 70, nil))
	if n := len(h.pushover.messages()); n != 1 {
		t.Fatalf("staying over the limit sent %d alerts, want 1", n)
	}

	// Back to one stream, then over again: a new alert.
	h.setLive(ep)
	h.mustHandle(t,
		sessionEvent(t, eventSessionStopped, "e4", "session_stopped", edits(asSession("s3"), at(80))),
		sessionEvent(t, eventSessionStopped, "e5", "session_stopped", edits(asSession("sess-movie"), at(81))),
	)
	h.setLive(ep, third)
	h.mustHandle(t, startOf(t, "e6", "s4", 90, nil))
	if got := titles(h.sentTo(testUserKey)); len(got) != 2 {
		t.Fatalf("crossing again after dropping back sent %q, want two alerts", got)
	}
}

// A stop that never arrives must not leave a phantom stream: the recount
// trusts Silo's live list.
func TestRecountIgnoresMissedStop(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, and(onlyAlerts(alertStreamsServer), func(a *AdminSettings) { a.Limits.ServerStreams = 1 }))
	ep := liveRow(t, "session_started", nil)
	h.setLive(ep)
	h.mustHandle(t, sessionEvent(t, eventSessionStarted, "e1", "session_started", nil))
	// The episode ends but its stop is lost; the movie starts.
	movie := liveRow(t, "session_started_local_movie", nil)
	h.setLive(movie)
	h.mustHandle(t, sessionEvent(t, eventSessionStarted, "e2", "session_started_local_movie", nil))
	if n := len(h.pushover.messages()); n != 0 {
		t.Fatalf("a phantom stream tripped the limit (%d alerts)", n)
	}
	sessions, _, _ := h.plugin.watch.readSessions(context.Background())
	if m := sessions["sess-ep1"]; m == nil || m.End.IsZero() {
		t.Fatalf("the missed stop wasn't recorded: %+v", m)
	}
	if n := h.callsTo("/api/v2/admin/sessions"); n != 2 {
		t.Fatalf("recounted %d times, want once per start", n)
	}
}

func TestUserStreamLimit(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, and(onlyAlerts(alertStreamsUser), func(a *AdminSettings) { a.Limits.UserStreams = 1 }))
	ep := liveRow(t, "session_started", nil)
	movie := liveRow(t, "session_started_local_movie", nil)
	second := liveRow(t, "session_started", map[string]any{"session_id": "s2", "client_label": "Phone"})

	// Two accounts, one stream each: fine.
	h.setLive(ep, movie)
	h.mustHandle(t, sessionEvent(t, eventSessionStarted, "e1", "session_started_local_movie", nil))
	if n := len(h.pushover.messages()); n != 0 {
		t.Fatalf("one stream each sent %d alerts", n)
	}
	// alice starts a second.
	h.setLive(ep, movie, second)
	h.mustHandle(t, startOf(t, "e2", "s2", 5, map[string]any{"client_label": "Phone"}))
	sent := h.sentTo(testUserKey)
	if len(sent) != 1 || sent[0].Get("title") != "alice is playing 2 streams" {
		t.Fatalf("sent %q", titles(sent))
	}
	if msg := sent[0].Get("message"); strings.Contains(msg, "bob") || !strings.Contains(msg, "Phone") {
		t.Fatalf("the list should hold alice's streams only: %q", msg)
	}

	// An ignored account's streams aren't alerted.
	h2 := newHarness(t)
	h2.optInAdmin(t, 7, "p1", testUserKey, and(onlyAlerts(alertStreamsUser), func(a *AdminSettings) {
		a.Limits.UserStreams = 1
		a.Filters.IgnoreUsers = []string{"12"}
	}))
	h2.setLive(ep, second)
	h2.mustHandle(t, startOf(t, "e2", "s2", 5, nil))
	if n := len(h2.pushover.messages()); n != 0 {
		t.Fatalf("an ignored account's streams sent %d alerts", n)
	}
	if n := h2.callsTo("/api/v2/admin/sessions"); n != 0 {
		t.Fatalf("recounted %d times with no limit alert to check", n)
	}
}

func TestBandwidthLimit(t *testing.T) {
	ep := func(t *testing.T) map[string]any { return liveRow(t, "session_started", nil) }                // 8 Mbps remote
	movie := func(t *testing.T) map[string]any { return liveRow(t, "session_started_local_movie", nil) } // 12 Mbps local
	for _, c := range []struct {
		name       string
		remoteOnly bool
		want       string
	}{
		{"all streams", false, "Streaming 20.0 Mbps"},
		{"remote only", true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.optInAdmin(t, 7, "p1", testUserKey, and(onlyAlerts(alertBandwidth), func(a *AdminSettings) {
				a.Limits.BandwidthMbps, a.Limits.BandwidthRemoteOnly = 15, c.remoteOnly
			}))
			h.setLive(ep(t), movie(t))
			h.mustHandle(t, sessionEvent(t, eventSessionStarted, "e1", "session_started_local_movie", nil))
			got := strings.Join(titles(h.sentTo(testUserKey)), "|")
			if got != c.want {
				t.Fatalf("sent %q, want %q", got, c.want)
			}
			if c.want != "" {
				msg := h.sentTo(testUserKey)[0].Get("message")
				// Biggest stream first.
				if !strings.HasPrefix(msg, "Over the limit of 15 Mbps, across 2 streams.\nbob · Dune: Part Two") {
					t.Fatalf("message = %q", msg)
				}
			}
		})
	}
}

// When the recount fails, the limit alert waits for Silo's retry; the
// stream-started alert goes out once.
func TestRecountFailureRetriesLimitAlertOnly(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, and(onlyAlerts(alertStreamStarted, alertStreamsServer), func(a *AdminSettings) { a.Limits.ServerStreams = 1 }))
	h.setLive(liveRow(t, "session_started", nil), liveRow(t, "session_started_local_movie", nil))
	req := sessionEvent(t, eventSessionStarted, "e1", "session_started", nil)

	h.failPath("/api/v2/admin/sessions", true)
	if err := h.handle(t, req); err == nil {
		t.Fatal("a failed recount should ask Silo to retry")
	}
	if got := titles(h.sentTo(testUserKey)); len(got) != 1 || !strings.Contains(got[0], "started") {
		t.Fatalf("first try sent %q", got)
	}
	h.failPath("/api/v2/admin/sessions", false)
	h.mustHandle(t, req)
	if got := titles(h.sentTo(testUserKey)); len(got) != 2 || got[1] != "2 streams playing" {
		t.Fatalf("after retry: %q", got)
	}
}

func TestNewDeviceAndAddress(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertNewDevice, alertNewIP))
	h.siloMu.Lock()
	h.devices["12"] = []string{"dev-tablet"}
	h.ips["12"] = []map[string]any{
		{"client_ip": "198.51.100.9", "location": "remote", "first_seen": "2026-10-01T00:00:00Z", "last_seen": "2026-11-01T00:00:00Z", "request_count": 4},
		{"client_ip": "192.168.1.5", "location": "local", "first_seen": "2026-10-01T00:00:00Z", "last_seen": "2026-11-01T00:00:00Z", "request_count": 9},
	}
	h.siloMu.Unlock()

	// Day one: the account's devices and addresses are learned, including
	// the ones this stream uses, and nothing is new.
	h.mustHandle(t, sessionEvent(t, eventSessionStarted, "e1", "session_started", nil))
	if n := len(h.pushover.messages()); n != 0 {
		t.Fatalf("the first stream of an account sent %d alerts", n)
	}
	// Known from Silo: no alerts.
	h.mustHandle(t, startOf(t, "e2", "s2", 5, map[string]any{"device_id": "dev-tablet", "client_ip": "198.51.100.9"}))
	if n := len(h.pushover.messages()); n != 0 {
		t.Fatalf("a device and address Silo already knew sent %d alerts", n)
	}
	if n, m := h.callsTo("/api/v2/admin/users/12/devices"), h.callsTo("/api/v2/admin/users/12/ips"); n != 1 || m != 1 {
		t.Fatalf("seeded devices %d and addresses %d times, want once each", n, m)
	}

	// A new device and a new remote address.
	h.mustHandle(t, startOf(t, "e3", "s3", 10, map[string]any{"device_id": "dev-phone", "client_label": "Pixel", "client_ip": "2001:db8:7:1::10"}))
	got := titles(h.sentTo(testUserKey))
	if strings.Join(got, "|") != "New device for alice (Kids)|New IP address for alice (Kids)" &&
		strings.Join(got, "|") != "New IP address for alice (Kids)|New device for alice (Kids)" {
		t.Fatalf("sent %q", got)
	}
	for _, m := range h.sentTo(testUserKey) {
		if strings.HasPrefix(m.Get("title"), "New IP") && !strings.HasPrefix(m.Get("message"), "2001:db8:7:1::10 · Remote") {
			t.Fatalf("IP message = %q", m.Get("message"))
		}
	}

	// The same /64, a local address and the same device: nothing new.
	h.mustHandle(t,
		startOf(t, "e4", "s4", 20, map[string]any{"device_id": "dev-phone", "client_ip": "2001:db8:7:1::99"}),
		startOf(t, "e5", "s5", 30, map[string]any{"device_id": "dev-phone", "client_ip": "192.168.1.77", "stream_location": "local"}),
	)
	if n := len(h.pushover.messages()); n != 2 {
		t.Fatalf("known device, same network and a local address sent %d more alerts", n-2)
	}
}

// A new device whose alert fails is still new when Silo retries the event.
func TestNewDeviceSurvivesRetry(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertNewDevice))
	h.mustHandle(t, sessionEvent(t, eventSessionStarted, "e1", "session_started", nil)) // learns the account
	req := startOf(t, "e2", "s2", 5, map[string]any{"device_id": "dev-phone"})
	h.pushover.fail.Store(http.StatusServiceUnavailable)
	if err := h.handle(t, req); err == nil {
		t.Fatal("a failed send should ask Silo to retry")
	}
	h.pushover.fail.Store(0)
	other := &eventServer{plugin: h.newPlugin(h.state)} // the retry may reach another server
	if _, err := other.HandleEvent(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := titles(h.sentTo(testUserKey)); len(got) != 1 || got[0] != "New device for alice (Kids)" {
		t.Fatalf("after retry: %q", got)
	}
	if _, err := other.HandleEvent(context.Background(), req); err != nil || len(h.pushover.messages()) != 1 {
		t.Fatalf("a repeat sent again (%d messages, %v)", len(h.pushover.messages()), err)
	}
}

// Seeding that can't reach Silo is retried rather than treating everything
// as known or as new.
func TestSeedFailureRetries(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertNewDevice))
	h.failPath("/api/v2/admin/users/12/devices", true)
	req := sessionEvent(t, eventSessionStarted, "e1", "session_started", nil)
	if err := h.handle(t, req); err == nil {
		t.Fatal("failed seeding should ask Silo to retry")
	}
	if _, found := h.state.values[seenBucketKey("12")]; found {
		t.Fatal("the account was recorded without its seed")
	}
	h.failPath("/api/v2/admin/users/12/devices", false)
	h.mustHandle(t, req)
	if n := len(h.pushover.messages()); n != 0 {
		t.Fatalf("day one sent %d alerts after a retry", n)
	}
}

func TestLibraryFilter(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, and(onlyAlerts(alertStreamStarted, alertStreamStopped), func(a *AdminSettings) {
		a.Filters.Libraries = []string{"1"}
	}))
	h.siloMu.Lock()
	h.catalog["ep-s2e1"] = []string{"2"}
	h.catalog["movie-dune2"] = []string{"1", "3"}
	h.siloMu.Unlock()
	h.mustHandle(t,
		sessionEvent(t, eventSessionStarted, "e1", "session_started", nil),
		sessionEvent(t, eventSessionStarted, "e2", "session_started_local_movie", nil),
		sessionEvent(t, eventSessionStopped, "e3", "session_stopped", nil), // the episode, via media_item_id
	)
	if got := titles(h.sentTo(testUserKey)); len(got) != 1 || got[0] != "bob started Dune: Part Two" {
		t.Fatalf("sent %q", got)
	}
	if n := h.callsTo("/api/v2/admin/catalog/items"); n != 2 {
		t.Fatalf("looked up the catalog %d times, want 2 (cached)", n)
	}

	// An item the catalog doesn't place matches no library.
	h.mustHandle(t, startOf(t, "e4", "s4", 5, map[string]any{"content_id": "audiobook-1"}))
	if n := len(h.pushover.messages()); n != 1 {
		t.Fatalf("an unplaced item got through the library filter (%d messages)", n)
	}

	// A failed lookup is retried.
	h.failPath("/api/v2/admin/catalog/items", true)
	req := sessionEvent(t, eventSessionStarted, "e5", "session_started_local_movie", edits(asSession("m2"), withRow(map[string]any{"content_id": "movie-2"})))
	if err := h.handle(t, req); err == nil {
		t.Fatal("a failed library lookup should ask Silo to retry")
	}
	h.failPath("/api/v2/admin/catalog/items", false)
	h.siloMu.Lock()
	h.catalog["movie-2"] = []string{"1"}
	h.siloMu.Unlock()
	h.mustHandle(t, req)
	if n := len(h.pushover.messages()); n != 2 {
		t.Fatalf("after retry: %d messages, want 2", n)
	}
}

func serverEvent(t *testing.T, name, eventID string, payload map[string]any) *pluginv1.HandleEventRequest {
	t.Helper()
	s, err := structpb.NewStruct(payload)
	if err != nil {
		t.Fatal(err)
	}
	return &pluginv1.HandleEventRequest{EventName: name, EventId: eventID, Payload: s}
}

// Shapes from silo-server d10d9c93: internal/events ScanRun and
// models.AdminJob.
func scanFailedPayload() map[string]any {
	return map[string]any{
		"id": "scan-77", "library_id": 1, "mode": "full", "path": "/media/movies", "trigger": "scheduled",
		"status": "failed", "started_at": "2026-11-03T19:58:00Z", "completed_at": "2026-11-03T20:01:00Z",
		"error_message": "open /media/movies:\n  input/output error",
	}
}

func jobFailedPayload() map[string]any {
	return map[string]any{
		"id": "job-5", "job_type": "library_refresh", "status": "failed", "created_by_user_id": 7,
		"request_payload": map[string]any{"library_id": 2}, "result_payload": nil,
		"message": "Refreshed 120 of 480 items", "error_message": "metadata provider timed out",
		"progress_current": 120, "progress_total": 480, "artifact_size_bytes": 0,
		"requested_at": "2026-11-03T19:00:00Z", "completed_at": "2026-11-03T19:30:00Z", "updated_at": "2026-11-03T19:30:00Z",
	}
}

func TestScanAndJobFailedAlerts(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertScanFailed, alertJobFailed))
	h.optInAdmin(t, 9, "p9", testAdmin2Key, onlyAlerts(alertScanFailed))
	h.setAccount("9", fakeAccount{"sam", "user", true}) // demoted

	h.mustHandle(t, serverEvent(t, eventScanFailed, "01JSCAN", scanFailedPayload()))
	sent := h.sentTo(testUserKey)
	if len(sent) != 1 || sent[0].Get("title") != "Scan failed: Movies" || sent[0].Get("message") != "open /media/movies: input/output error\nFull scan · /media/movies" {
		t.Fatalf("scan alert = %v", sent)
	}
	if sent[0].Get("timestamp") != fmt.Sprint(time.Date(2026, 11, 3, 20, 1, 0, 0, time.UTC).Unix()) {
		t.Fatalf("timestamp = %q, want the scan's end", sent[0].Get("timestamp"))
	}
	if n := len(h.sentTo(testAdmin2Key)); n != 0 {
		t.Fatalf("a demoted admin got %d scan alerts", n)
	}
	// Silo publishes a failed scan from more than one place: same scan,
	// another event ID, no second alert.
	h.mustHandle(t, serverEvent(t, eventScanFailed, "01JSCAN2", scanFailedPayload()))
	if n := len(h.sentTo(testUserKey)); n != 1 {
		t.Fatalf("the same scan alerted %d times", n)
	}

	h.mustHandle(t, serverEvent(t, eventJobFailed, "01JJOB", jobFailedPayload()))
	sent = h.sentTo(testUserKey)
	if len(sent) != 2 || sent[1].Get("title") != "Job failed: Library refresh" || sent[1].Get("message") != "metadata provider timed out\nRefreshed 120 of 480 items" {
		t.Fatalf("job alert = %v", sent)
	}

	// Without the key, nothing.
	h.plugin.setConfig(adminConfig{DefaultAppToken: testAppToken})
	h.mustHandle(t, serverEvent(t, eventJobFailed, "01JJOB2", map[string]any{"id": "job-6", "job_type": "item_refresh", "status": "failed"}))
	if n := len(h.pushover.messages()); n != 2 {
		t.Fatalf("sent a job alert without the key (%d messages)", n)
	}
}

func TestServerEventPushoverFailureRetries(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertScanFailed))
	h.pushover.fail.Store(http.StatusInternalServerError)
	req := serverEvent(t, eventScanFailed, "01JSCAN", scanFailedPayload())
	if err := h.handle(t, req); err == nil {
		t.Fatal("a failed send should ask Silo to retry")
	}
	h.pushover.fail.Store(0)
	h.mustHandle(t, req, req)
	if n := len(h.pushover.messages()); n != 1 {
		t.Fatalf("sent %d, want 1", n)
	}
}

// The tracker survives events in any order, repeats included.
func TestTrackerHandlesOrderAndRepeats(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertStreamStopped))
	stop := sessionEvent(t, eventSessionStopped, "e2", "session_stopped", nil)
	start := sessionEvent(t, eventSessionStarted, "e1", "session_started", nil)
	h.mustHandle(t, stop, start, start, stop)
	sessions, _, _ := h.plugin.watch.readSessions(context.Background())
	m := sessions["sess-ep1"]
	if m == nil || m.End.IsZero() || !m.Started || m.live(testNow.Add(time.Hour)) {
		t.Fatalf("a stop that came before its start should leave the session stopped: %+v", m)
	}
	if m.Location != "remote" || m.Device != "Living Room TV" || m.Kbps != 8000 {
		t.Fatalf("the start's stream details weren't kept: %+v", m)
	}
	if n := len(h.pushover.messages()); n != 1 {
		t.Fatalf("a repeated stop sent %d alerts", n)
	}
}

func TestReplacedStopIsTrackedQuietly(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, nil)
	h.mustHandle(t, sessionEvent(t, eventSessionStarted, "e1", "session_started", nil))
	h.mustHandle(t, sessionEvent(t, eventSessionStopped, "e2", "session_stopped", func(p map[string]any) { p["end_reason"] = endReasonReplaced }))
	if n := len(h.pushover.messages()); n != 1 {
		t.Fatalf("a replaced stop sent an alert (%d messages)", n)
	}
	sessions, _, _ := h.plugin.watch.readSessions(context.Background())
	if m := sessions["sess-ep1"]; m == nil || m.End.IsZero() {
		t.Fatalf("a replaced session should count as stopped: %+v", m)
	}
}

// sizedState is a state store that refuses values over the host's limit,
// as the host does.
type sizedState struct {
	memoryState
	mu      sync.Mutex
	refused []string
}

func (s *sizedState) Write(ctx context.Context, key string, value []byte) error {
	if len(value) > maxStateValue {
		s.mu.Lock()
		s.refused = append(s.refused, key)
		s.mu.Unlock()
		return fmt.Errorf("value for %q is %d bytes, over the limit", key, len(value))
	}
	return s.memoryState.Write(ctx, key, value)
}

// Every key the watchdog writes stays under the host's value limit, however
// full it gets, by dropping its oldest entries.
func TestFullStateStaysUnderSizeLimit(t *testing.T) {
	ctx := context.Background()
	state := &sizedState{memoryState: memoryState{values: map[string][]byte{}}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := testNow

	// A tracker and throttle far past 256 KiB, as Chat 3's count caps
	// allowed: thousands of sessions and cooldowns with long IDs.
	sessions := sessionTable{}
	for i := range 6000 {
		sessions[fmt.Sprintf("session-%06d-%s", i, strings.Repeat("x", 40))] = &trackedSession{
			UserID: fmt.Sprint(i % 300), Show: "series:" + strings.Repeat("a long show name ", 3), Started: true,
			Start: now.Add(-time.Duration(6000-i) * time.Second), End: now.Add(-time.Duration(6000-i)*time.Second + time.Minute),
			Location: "remote", Method: "transcode", Device: "Living Room TV", Kbps: 8000,
		}
	}
	sessions["live-oldest"] = &trackedSession{UserID: "1", Start: now.Add(-11 * time.Hour)}
	th := &throttleState{Cooldowns: map[string]cooldownMark{}, Over: map[string]cooldownMark{}}
	for i := range 6000 {
		th.Cooldowns[fmt.Sprintf("%d|%d|stream.started", i%8, i)] = cooldownMark{At: now.Add(-time.Duration(6000-i) * time.Second), Event: "playback-lifecycle:" + strings.Repeat("9", 20) + fmt.Sprint(i)}
	}
	th.Over["7|streams.server|"] = cooldownMark{At: now.Add(-time.Hour), Event: "e"}
	for key, v := range map[string]any{sessionsKey: sessions, throttleKey: th} {
		raw, _ := json.Marshal(v)
		if len(raw) <= maxStateValue {
			t.Fatalf("%s test state is only %d bytes; make it bigger", key, len(raw))
		}
		state.values[key] = raw
	}

	w := newWatchStore(state)
	w.now = func() time.Time { return now }
	a := defaultAdminSettings()
	a.UserID, a.ProfileID, a.Noise.Mode = 7, "p1", noiseCooldown
	a.Alerts[alertStreamStarted] = AlertSettings{Enabled: true}
	a.Alerts[alertStreamsServer] = AlertSettings{Enabled: true}
	a.Limits.ServerStreams = 1
	pb := playback{Event: eventSessionStarted, SessionID: "newest", UserID: "12", At: now, FromRow: true, Location: "remote"}
	r := newRecipient(a, alertStreamStarted, "e-new")
	if _, err := w.decide(ctx, log, &watchEvent{pb: &pb, admins: []*AdminSettings{a}}, []*recipient{r}); err != nil {
		t.Fatal(err)
	}
	if len(state.refused) > 0 {
		t.Fatalf("writes refused for %v", state.refused)
	}
	got, _, _ := w.readSessions(ctx)
	if got["newest"] == nil || got["live-oldest"] == nil {
		t.Fatal("the size cap dropped the newest or a live session")
	}
	if _, ok := got["session-000000-"+strings.Repeat("x", 40)]; ok {
		t.Fatal("the size cap kept the oldest stopped session")
	}
	gotTh, _, _ := w.readThrottle(ctx)
	if _, ok := gotTh.Cooldowns[cooldownKey(7, "12", alertStreamStarted)]; !ok {
		t.Fatal("the newest cooldown was dropped")
	}
	if _, ok := gotTh.Over["7|streams.server|"]; !ok {
		t.Fatal("a limit mark was dropped before older cooldowns")
	}
	for _, key := range []string{sessionsKey, throttleKey} {
		if n := len(state.values[key]); n > maxStateValue || n < maxStateValue*8/10 {
			t.Fatalf("%s is %d bytes; want just under %d", key, n, maxStateValue)
		}
	}

	// A seen bucket full of accounts.
	seen := newSeenStore(state)
	seen.now = func() time.Time { return now }
	key := seenBucketKey("12")
	users := map[string]*seenUser{}
	for i := range 2000 {
		u := &seenUser{Devices: map[string]seenItem{}, IPs: map[string]seenItem{}, Updated: now.Unix() - int64(2000-i)}
		for d := range maxSeenPerUser {
			u.Devices[fmt.Sprintf("device-%d-%s", d, strings.Repeat("d", 20))] = seenItem{At: u.Updated}
			u.IPs[fmt.Sprintf("2001:db8:%x:%x::/64", i, d)] = seenItem{At: u.Updated}
		}
		users[fmt.Sprintf("acct-%d", i)] = u
	}
	raw, _ := json.Marshal(users)
	state.values[key] = raw
	noSeed := func(context.Context) ([]string, []string, error) { return []string{"dev-a"}, nil, nil }
	if _, _, err := seen.check(ctx, "12", "dev-a", "203.0.113.24", "e1", noSeed); err != nil {
		t.Fatal(err)
	}
	if isNew, _, _ := seen.check(ctx, "12", "dev-b", "203.0.113.24", "e2", noSeed); !isNew || len(state.refused) > 0 {
		t.Fatalf("new device = %v, refused writes %v", isNew, state.refused)
	}
	gotUsers, _, _ := seen.read(ctx, key)
	if gotUsers["12"] == nil || len(state.values[key]) > maxStateValue {
		t.Fatalf("the account being updated was dropped, or the bucket is %d bytes", len(state.values[key]))
	}

	// A handled-events bucket of IDs that encode long (HTML characters are
	// escaped six times their size).
	log2 := newEventLog(state)
	var ids []string
	for i := range eventLogPerBucket + 10 {
		ids = append(ids, fmt.Sprintf("%04d%s", i, strings.Repeat("<", 190)))
	}
	if err := log2.RecordMany(ctx, ids); err != nil || len(state.refused) > 0 {
		t.Fatalf("event log write: %v, refused %v", err, state.refused)
	}
	if seenID, _ := newEventLog(state).Seen(ctx, ids[len(ids)-1]); !seenID {
		t.Fatal("the newest handled ID was dropped")
	}
}

func TestSeenKeepsNewestFifty(t *testing.T) {
	m := map[string]seenItem{}
	for i := range 60 {
		m[fmt.Sprint(i)] = seenItem{At: int64(i)}
	}
	capSeen(m)
	if _, ok := m["9"]; len(m) != maxSeenPerUser || ok {
		t.Fatalf("kept %d, oldest kept = %v", len(m), ok)
	}
}

func TestIPKey(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.24":     "203.0.113.24",
		"::ffff:1.2.3.4":   "1.2.3.4",
		"2001:db8:7:1::10": "2001:db8:7:1::/64",
		"2001:db8:7:1:a::": "2001:db8:7:1::/64",
		" junk ":           "junk",
	} {
		if got := ipKey(in); got != want {
			t.Errorf("ipKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMultiLineValuesKeepTheirLines(t *testing.T) {
	got := renderTemplate("The limit is {limit}.\n{streams}", map[string]string{"limit": "2", "streams": "a · b · \nc  ·  d"}, false)
	if got != "The limit is 2.\na · b\nc · d" {
		t.Fatalf("got %q", got)
	}
}

func TestWatchdogSettingsEndpoints(t *testing.T) {
	h := newHarness(t)
	h.configure(t, func(s *ProfileSettings) { s.UserID = 7 })

	status, out := h.request(t, http.MethodGet, "/api/admin/alerts", asPrimary, nil)
	if status != http.StatusOK {
		t.Fatalf("GET: %d %v", status, out)
	}
	libs, _ := json.Marshal(out["libraries"])
	if string(libs) != `[{"id":"1","name":"Movies"},{"id":"2","name":"TV Shows"}]` {
		t.Fatalf("libraries = %s", libs)
	}
	if settings := out["settings"].(map[string]any); settings["limits"] == nil || settings["filters"].(map[string]any)["libraries"] == nil {
		t.Fatalf("settings lack limits or libraries: %v", settings)
	}

	save := map[string]any{
		"alerts": map[string]any{
			alertStreamsServer: map[string]any{"enabled": true},
			alertBandwidth:     map[string]any{"enabled": true},
			alertScanFailed:    map[string]any{"enabled": true},
		},
		"filters": map[string]any{"libraries": []string{"2", "2"}},
		"limits":  map[string]any{"server_streams": 4, "bandwidth_mbps": 50, "bandwidth_remote_only": true, "user_streams": 3},
	}
	if status, out := h.request(t, http.MethodPut, "/api/admin/alerts", asPrimary, save); status != http.StatusOK {
		t.Fatalf("PUT: %d %v", status, out)
	}
	stored, _, _ := h.plugin.admins.Get(context.Background(), 7)
	if stored.Limits != (AdminLimits{UserStreams: 3, ServerStreams: 4, BandwidthMbps: 50, BandwidthRemoteOnly: true}) ||
		strings.Join(stored.Filters.Libraries, ",") != "2" || !stored.alert(alertScanFailed).Enabled {
		t.Fatalf("stored = %+v", stored)
	}

	for name, bad := range map[string]map[string]any{
		"on without a limit": {"alerts": map[string]any{alertStreamsUser: map[string]any{"enabled": true}}},
		"negative limit":     {"limits": map[string]any{"server_streams": -1}},
		"huge limit":         {"limits": map[string]any{"user_streams": maxUserStreams + 1}},
		"library id":         {"filters": map[string]any{"libraries": []string{"a/b"}}},
	} {
		if status, out := h.request(t, http.MethodPut, "/api/admin/alerts", asPrimary, bad); status != http.StatusUnprocessableEntity {
			t.Fatalf("%s: %d %v", name, status, out)
		}
	}

	// Test sends work for the new alerts.
	for _, kind := range []string{alertStreamsServer, alertNewIP, alertScanFailed, alertJobFailed} {
		body := map[string]any{"alert": kind, "settings": map[string]any{}}
		if status, out := h.request(t, http.MethodPost, "/api/admin/alerts/test", asPrimary, body); status != http.StatusOK {
			t.Fatalf("test %s: %d %v", kind, status, out)
		}
	}
	sent := h.sentTo(testUserKey)
	if len(sent) != 4 || sent[0].Get("title") != "[Test] 3 streams playing" || !strings.Contains(sent[0].Get("message"), "\nbob · The Bear") {
		t.Fatalf("test sends = %q", titles(sent))
	}
}
