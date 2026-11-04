package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// A second admin's Pushover user key.
const testAdmin2Key = "sAmKeY0123456789abcdefghijklmn"

// fixture loads testdata/<name>.json as a JSON object.
func fixture(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return m
}

// accountRow is the fake Silo's AdminUser for an account. The caller holds
// h.siloMu.
func (h *harness) accountRow(t *testing.T, id string) map[string]any {
	row := fixture(t, "admin_user")
	a := h.accounts[id]
	row["id"], row["username"], row["role"], row["enabled"] = id, a.username, a.role, a.enabled
	return row
}

func (h *harness) setAccount(id string, a fakeAccount) {
	h.siloMu.Lock()
	h.accounts[id] = a
	h.siloMu.Unlock()
}

func (h *harness) deleteAccount(id string) {
	h.siloMu.Lock()
	delete(h.accounts, id)
	h.siloMu.Unlock()
}

func (h *harness) userLookups(id string) int {
	h.siloMu.Lock()
	defer h.siloMu.Unlock()
	return h.userCalls[id]
}

// sessionEvent builds a session.* event from a fixture. edit changes the
// payload; "at" sets the event time.
func sessionEvent(t *testing.T, name, eventID, file string, edit func(map[string]any)) *pluginv1.HandleEventRequest {
	t.Helper()
	payload := fixture(t, file)
	if edit != nil {
		edit(payload)
	}
	s, err := structpb.NewStruct(payload)
	if err != nil {
		t.Fatal(err)
	}
	return &pluginv1.HandleEventRequest{EventName: name, EventId: eventID, Payload: s}
}

// at returns an edit that sets the event time to base plus minutes.
func at(minutes int) func(map[string]any) {
	return func(p map[string]any) {
		p["at"] = time.Date(2026, 11, 3, 20, 0, 0, 0, time.UTC).Add(time.Duration(minutes) * time.Minute).Format(time.RFC3339Nano)
	}
}

// edits combines payload edits.
func edits(fns ...func(map[string]any)) func(map[string]any) {
	return func(p map[string]any) {
		for _, fn := range fns {
			fn(p)
		}
	}
}

// asSession sets the session ID on the event and its row.
func asSession(id string) func(map[string]any) {
	return func(p map[string]any) {
		p["session_id"] = id
		for _, row := range []string{"session", "history"} {
			if r, ok := p[row].(map[string]any); ok {
				r["session_id"] = id
			}
		}
	}
}

// withRow edits the event's session or history row.
func withRow(fields map[string]any) func(map[string]any) {
	return func(p map[string]any) {
		for _, row := range []string{"session", "history"} {
			if r, ok := p[row].(map[string]any); ok {
				for k, v := range fields {
					r[k] = v
				}
			}
		}
	}
}

// optInAdmin gives an admin account a profile with Pushover set up and saves
// their admin alert settings, with every alert on and noise mode "every"
// unless edit says otherwise.
func (h *harness) optInAdmin(t *testing.T, userID int, profileID, userKey string, edit func(*AdminSettings)) {
	t.Helper()
	ctx := context.Background()
	if err := h.plugin.store.Put(ctx, &ProfileSettings{ProfileID: profileID, ProfileName: "Admin", UserID: userID, Enabled: true, UserKey: userKey}); err != nil {
		t.Fatal(err)
	}
	a := defaultAdminSettings()
	a.UserID, a.ProfileID = userID, profileID
	a.Noise.Mode = noiseEvery
	// Due at testNow (Tuesday 20:00 UTC), so the digest tests don't depend on
	// the default schedule.
	a.Digest = &DigestSchedule{Day: int(testNow.Weekday()), Hour: testNow.Hour()}
	for _, at := range adminAlertTypes {
		s := at.defaults()
		s.Enabled = true
		a.Alerts[at.Key] = s
	}
	if edit != nil {
		edit(a)
	}
	if err := h.plugin.admins.Put(ctx, a); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) handle(t *testing.T, req *pluginv1.HandleEventRequest) error {
	t.Helper()
	_, err := h.events.HandleEvent(context.Background(), req)
	return err
}

func (h *harness) mustHandle(t *testing.T, reqs ...*pluginv1.HandleEventRequest) {
	t.Helper()
	for _, req := range reqs {
		if err := h.handle(t, req); err != nil {
			t.Fatalf("HandleEvent(%s %s): %v", req.GetEventName(), req.GetEventId(), err)
		}
	}
}

// sentTo returns the messages sent to one Pushover user key.
func (h *harness) sentTo(userKey string) []url.Values {
	var out []url.Values
	for _, m := range h.pushover.messages() {
		if m.Get("user") == userKey {
			out = append(out, m)
		}
	}
	return out
}

func titles(msgs []url.Values) []string {
	var out []string
	for _, m := range msgs {
		out = append(out, m.Get("title"))
	}
	return out
}

func TestSessionFixturesParse(t *testing.T) {
	now := time.Now()
	raw, _ := json.Marshal(fixture(t, "session_started"))
	pb, ok := parseLifecycle(eventSessionStarted, raw, now)
	if !ok || pb.UserID != "12" || pb.Location != "remote" || !pb.isTranscode() || pb.BitrateKbps != 8000 || !pb.FromRow {
		t.Fatalf("started = %+v, %v", pb, ok)
	}
	v := pb.vars()
	for k, want := range map[string]string{
		"user_profile": "alice (Kids)", "title": "Severance S02E01", "episode_title": "Hello, Ms. Cobel",
		"device": "Living Room TV", "client": "Silo for Android TV 2.4.1", "video": "4K → 1080p",
		"bitrate": "8.0 Mbps", "method": "Transcode", "location": "Remote", "media_type": "Episode",
	} {
		if v[k] != want {
			t.Errorf("started %s = %q, want %q", k, v[k], want)
		}
	}

	raw, _ = json.Marshal(fixture(t, "session_stopped"))
	pb, ok = parseLifecycle(eventSessionStopped, raw, now)
	if !ok || pb.alertKind() != alertStreamStopped || pb.Location != "remote" || pb.Method != "transcode" || pb.FromRow {
		t.Fatalf("stopped = %+v, %v", pb, ok)
	}
	v = pb.vars()
	for k, want := range map[string]string{"watched": "42m", "progress": "78%", "finished": "not finished", "position": "45:00", "end_reason": "stopped by the client"} {
		if v[k] != want {
			t.Errorf("stopped %s = %q, want %q", k, v[k], want)
		}
	}

	raw, _ = json.Marshal(fixture(t, "session_started_local_movie"))
	pb, ok = parseLifecycle(eventSessionStarted, raw, now)
	if v := pb.vars(); !ok || pb.Location != "local" || v["title"] != "Dune: Part Two" || v["user_profile"] != "bob" || v["method"] != "Direct play" {
		t.Fatalf("movie = %+v, %v", v, ok)
	}

	// A start with no live row says nothing about who is playing: unusable.
	raw, _ = json.Marshal(map[string]any{"session_id": "s", "seq": 1, "at": "2026-11-03T20:00:00Z", "state": "playing"})
	if _, ok := parseLifecycle(eventSessionStarted, raw, now); ok {
		t.Fatal("an event without a row should be unusable")
	}
}

func TestLocationFromIP(t *testing.T) {
	for ip, want := range map[string]string{
		"192.168.1.4": "local", "10.0.0.1": "local", "127.0.0.1": "local", "100.101.102.103": "local",
		"fd00::1": "local", "::ffff:192.168.1.4": "local", "203.0.113.24": "remote", "2001:db8::1": "remote", "": "",
	} {
		if got := locationFromIP(ip); got != want {
			t.Errorf("locationFromIP(%q) = %q, want %q", ip, got, want)
		}
	}
}

func TestPlaybackAlertsReachOptedInAdmin(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, nil)
	h.mustHandle(t,
		sessionEvent(t, eventSessionStarted, "playback-lifecycle:101", "session_started", nil),
		sessionEvent(t, eventSessionPaused, "playback-lifecycle:102", "session_paused", nil),
		sessionEvent(t, eventSessionResumed, "playback-lifecycle:103", "session_paused", edits(at(14), func(p map[string]any) { p["state"] = "playing" })),
		sessionEvent(t, eventSessionStopped, "playback-lifecycle:104", "session_stopped", nil),
	)
	sent := h.sentTo(testUserKey)
	want := []string{
		"alice (Kids) started Severance S02E01",
		"alice (Kids) paused Severance S02E01",
		"alice (Kids) resumed Severance S02E01",
		"alice (Kids) stopped Severance S02E01",
	}
	if got := titles(sent); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("titles = %q, want %q", got, want)
	}
	if msg := sent[0].Get("message"); msg != "Hello, Ms. Cobel\nLiving Room TV · Silo for Android TV 2.4.1\nRemote · Transcode · 4K → 1080p · 8.0 Mbps" {
		t.Fatalf("start message = %q", msg)
	}
	if msg := sent[1].Get("message"); msg != "Hello, Ms. Cobel\nAt 12:34 (21%)\nLiving Room TV" {
		t.Fatalf("pause message = %q", msg)
	}
	// The history entry has no device label; the start's is remembered.
	if msg := sent[3].Get("message"); msg != "Hello, Ms. Cobel\nWatched 42m\n78% · not finished\nLiving Room TV" {
		t.Fatalf("stop message = %q", msg)
	}
	if ts := sent[0].Get("token"); ts != testAppToken {
		t.Fatalf("token = %q, want the shared token", ts)
	}

	// A repeat of any of them is skipped.
	h.mustHandle(t, sessionEvent(t, eventSessionStarted, "playback-lifecycle:101", "session_started", nil))
	if n := len(h.sentTo(testUserKey)); n != 4 {
		t.Fatalf("repeat sent again: %d messages", n)
	}
}

func TestStopWithoutWatchTimeDropsThatLine(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, nil)
	h.mustHandle(t, sessionEvent(t, eventSessionStopped, "e1", "session_stopped", withRow(map[string]any{"played_seconds": nil, "completed": true})))
	// No start seen, so the device is the platform the history entry has.
	if msg := h.sentTo(testUserKey)[0].Get("message"); msg != "Hello, Ms. Cobel\n78% · finished\nandroid_tv" {
		t.Fatalf("message = %q", msg)
	}
}

func TestPlaybackFailedAlert(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, func(a *AdminSettings) {
		a.Alerts[alertStreamStopped] = AlertSettings{Enabled: false, Title: "x", Message: "x"}
	})
	h.optInAdmin(t, 9, "p9", testAdmin2Key, func(a *AdminSettings) {
		a.Alerts[alertPlaybackFailed] = AlertSettings{Enabled: false}
	})
	aborted := func(p map[string]any) {
		p["end_reason"] = endReasonAborted
		p["history"].(map[string]any)["end_reason"] = endReasonAborted
	}
	h.mustHandle(t, sessionEvent(t, eventSessionStopped, "e1", "session_stopped", aborted))

	if got := titles(h.sentTo(testUserKey)); len(got) != 1 || got[0] != "Playback failed for alice (Kids)" {
		t.Fatalf("admin with failures on got %q", got)
	}
	// Failures off but stops on: told as a stop.
	if got := titles(h.sentTo(testAdmin2Key)); len(got) != 1 || got[0] != "alice (Kids) stopped Severance S02E01" {
		t.Fatalf("admin with only stops on got %q", got)
	}

	// A replaced session goes on in a new one: no stop alert.
	h.mustHandle(t, sessionEvent(t, eventSessionStopped, "e2", "session_stopped", func(p map[string]any) { p["end_reason"] = endReasonReplaced }))
	if n := len(h.pushover.messages()); n != 2 {
		t.Fatalf("replaced stop sent alerts: %d messages", n)
	}
}

func TestAdminFilters(t *testing.T) {
	cases := []struct {
		name   string
		filter AdminFilters
		// want is which fixture starts get through: ep (remote transcode
		// episode by alice), movie (local direct movie by bob).
		want string
	}{
		{"none", AdminFilters{}, "ep,movie"},
		{"ignore alice", AdminFilters{IgnoreUsers: []string{"12"}}, "movie"},
		{"only remote", AdminFilters{OnlyRemote: true}, "ep"},
		{"only transcodes", AdminFilters{OnlyTranscodes: true}, "ep"},
		{"movies only", AdminFilters{MediaTypes: []string{"movie"}}, "movie"},
		{"episodes and audiobooks", AdminFilters{MediaTypes: []string{"episode", "audiobook"}}, "ep"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.optInAdmin(t, 7, "p1", testUserKey, func(a *AdminSettings) { a.Filters = c.filter })
			h.mustHandle(t,
				sessionEvent(t, eventSessionStarted, "e1", "session_started", nil),
				sessionEvent(t, eventSessionStarted, "e2", "session_started_local_movie", nil),
			)
			var got []string
			for _, title := range titles(h.sentTo(testUserKey)) {
				if strings.Contains(title, "Severance") {
					got = append(got, "ep")
				} else {
					got = append(got, "movie")
				}
			}
			if strings.Join(got, ",") != c.want {
				t.Fatalf("sent %v, want %s", got, c.want)
			}
		})
	}
}

// A stop's history entry has no stream_location. The plugin remembers what
// the start said, so a remote stream from a private address (a VPN, say)
// still counts as remote when it stops.
func TestStopRemembersStartLocation(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, func(a *AdminSettings) { a.Filters.OnlyRemote = true })
	vpn := withRow(map[string]any{"client_ip": "10.8.0.6"})
	h.mustHandle(t,
		sessionEvent(t, eventSessionStarted, "e1", "session_started", vpn),
		sessionEvent(t, eventSessionStopped, "e2", "session_stopped", vpn),
	)
	if got := titles(h.sentTo(testUserKey)); len(got) != 2 {
		t.Fatalf("want start and stop of the remote stream, got %q", got)
	}

	// Without its start, the address decides: private means local.
	h.mustHandle(t, sessionEvent(t, eventSessionStopped, "e3", "session_stopped", edits(asSession("other"), vpn)))
	if n := len(h.sentTo(testUserKey)); n != 2 {
		t.Fatalf("a stop from a private address passed only-remote (%d messages)", n)
	}
}

// Silo delivers events out of order: a pause that happened before the stop
// but arrives after it is dropped; a later one (a new pause) still goes out.
func TestLatePauseAfterStopIsDropped(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, nil)
	h.mustHandle(t,
		sessionEvent(t, eventSessionStarted, "e1", "session_started", at(0)),
		sessionEvent(t, eventSessionStopped, "e3", "session_stopped", at(10)),
		sessionEvent(t, eventSessionPaused, "e2", "session_paused", at(9)),
	)
	got := titles(h.sentTo(testUserKey))
	if len(got) != 2 || strings.Contains(strings.Join(got, "|"), "paused") {
		t.Fatalf("titles = %q, want only the start and the stop", got)
	}

	// A pause stamped after the recorded stop isn't stale.
	h.mustHandle(t, sessionEvent(t, eventSessionPaused, "e4", "session_paused", at(11)))
	if got := titles(h.sentTo(testUserKey)); len(got) != 3 {
		t.Fatalf("a pause after the stop time was dropped: %q", got)
	}
}

func TestNoiseCooldown(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, func(a *AdminSettings) { a.Noise = NoiseSettings{Mode: noiseCooldown, WindowMinutes: 30} })
	h.mustHandle(t,
		sessionEvent(t, eventSessionStarted, "e1", "session_started", at(0)),
		sessionEvent(t, eventSessionStarted, "e2", "session_started", edits(at(5), asSession("s2"))),  // held back
		sessionEvent(t, eventSessionPaused, "e3", "session_paused", at(6)),                            // another type: sent
		sessionEvent(t, eventSessionStarted, "e4", "session_started_local_movie", at(7)),              // another user: sent
		sessionEvent(t, eventSessionStarted, "e5", "session_started", edits(at(31), asSession("s3"))), // window over: sent
		sessionEvent(t, eventSessionStarted, "e6", "session_started", edits(at(45), asSession("s4"))), // held back
	)
	got := titles(h.sentTo(testUserKey))
	want := []string{"alice (Kids) started Severance S02E01", "alice (Kids) paused Severance S02E01", "bob started Dune: Part Two", "alice (Kids) started Severance S02E01"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("titles = %q, want %q", got, want)
	}
}

func TestNoiseCollapseBinges(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, func(a *AdminSettings) { a.Noise = NoiseSettings{Mode: noiseBinge, WindowMinutes: 30} })
	ep := func(n int) func(map[string]any) {
		return edits(asSession("ep"+string(rune('0'+n))), withRow(map[string]any{"episode_number": n}))
	}
	h.mustHandle(t,
		sessionEvent(t, eventSessionStarted, "e1", "session_started", edits(at(0), ep(1))),   // sent: the binge starts
		sessionEvent(t, eventSessionPaused, "e2", "session_paused", edits(at(10), ep(1))),    // held back
		sessionEvent(t, eventSessionStopped, "e3", "session_stopped", edits(at(50), ep(1))),  // held back
		sessionEvent(t, eventSessionStarted, "e4", "session_started", edits(at(60), ep(2))),  // 10 min after the stop: held back
		sessionEvent(t, eventSessionStopped, "e5", "session_stopped", edits(at(110), ep(2))), // held back
		sessionEvent(t, eventSessionStarted, "e6", "session_started_local_movie", at(115)),   // another show: sent
		sessionEvent(t, eventSessionStarted, "e7", "session_started", edits(at(141), ep(3))), // 31 min after the last stop: sent
	)
	// Failures aren't noise controlled.
	h.mustHandle(t, sessionEvent(t, eventSessionStopped, "e8", "session_stopped", edits(at(150), ep(3), func(p map[string]any) { p["end_reason"] = endReasonAborted })))

	got := titles(h.sentTo(testUserKey))
	want := []string{"alice (Kids) started Severance S02E01", "bob started Dune: Part Two", "alice (Kids) started Severance S02E03", "Playback failed for alice (Kids)"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("titles = %q, want %q", got, want)
	}

	// Out of order: a stop whose start was never seen still says something.
	h.mustHandle(t, sessionEvent(t, eventSessionStopped, "e9", "session_stopped", edits(at(400), asSession("lonely"))))
	if n := len(h.sentTo(testUserKey)); n != 5 {
		t.Fatalf("a stop with no binge before it was held back (%d messages)", n)
	}
}

// One event goes to two admins and Pushover fails for one of them. The retry
// sends only to that one, and noise control doesn't hold back the retry.
func TestFanOutRetriesOnlyFailedRecipients(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, func(a *AdminSettings) { a.Noise.Mode = noiseBinge })
	h.optInAdmin(t, 9, "p9", testAdmin2Key, func(a *AdminSettings) { a.Noise.Mode = noiseCooldown })
	req := sessionEvent(t, eventSessionStarted, "playback-lifecycle:101", "session_started", nil)

	h.pushover.failUser.Store(testAdmin2Key)
	if err := h.handle(t, req); err == nil {
		t.Fatal("a failed recipient should ask Silo to retry")
	}
	if n, m := len(h.sentTo(testUserKey)), len(h.sentTo(testAdmin2Key)); n != 1 || m != 0 {
		t.Fatalf("first try: admin 7 got %d, admin 9 got %d", n, m)
	}

	// The retry may reach another API server.
	h.pushover.failUser.Store("")
	other := &eventServer{plugin: h.newPlugin(h.state)}
	for range 2 {
		if _, err := other.HandleEvent(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	if n, m := len(h.sentTo(testUserKey)), len(h.sentTo(testAdmin2Key)); n != 1 || m != 1 {
		t.Fatalf("after retry: admin 7 got %d (want 1), admin 9 got %d (want 1)", n, m)
	}
	for _, key := range []string{"playback-lifecycle:101|admin:7", "playback-lifecycle:101|admin:9"} {
		if seen, _ := h.plugin.handled.Seen(context.Background(), key); !seen {
			t.Fatalf("%s not recorded", key)
		}
	}
}

func TestFanOutFitsEventDeadline(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, nil)
	h.optInAdmin(t, 9, "p9", testAdmin2Key, nil)
	h.pushover.hold = make(chan struct{})
	defer close(h.pushover.hold)
	h.plugin.eventTimeout = 200 * time.Millisecond

	start := time.Now()
	if err := h.handle(t, sessionEvent(t, eventSessionStarted, "e1", "session_started", nil)); err == nil {
		t.Fatal("sends that outlive the deadline should ask Silo to retry")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("HandleEvent took %v; the deadline wasn't applied to the fan-out", elapsed)
	}
}

func TestAdminAlertsNeedMonitorKey(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, nil)
	h.plugin.setConfig(adminConfig{DefaultAppToken: testAppToken})
	h.mustHandle(t, sessionEvent(t, eventSessionStarted, "e1", "session_started", nil))
	if n := len(h.pushover.messages()); n != 0 {
		t.Fatalf("sent %d admin alerts without the read-only key", n)
	}
	if n := h.siloCalls.Load(); n != 0 {
		t.Fatalf("called Silo %d times without a key", n)
	}
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/api/admin/alerts"}, {http.MethodPut, "/api/admin/alerts"},
		{http.MethodPost, "/api/admin/alerts/test"}, {http.MethodGet, "/api/admin/users"},
	} {
		var body any
		if r.method != http.MethodGet {
			body = map[string]any{}
		}
		status, out := h.request(t, r.method, r.path, asPrimary, body)
		if status != http.StatusConflict || out["code"] != "monitor_key_missing" || !strings.Contains(out["message"].(string), "Set up the read-only key") {
			t.Fatalf("%s %s: %d %v", r.method, r.path, status, out)
		}
	}
}

func TestRecipientRoleIsChecked(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, nil)
	h.optInAdmin(t, 9, "p9", testAdmin2Key, nil)
	h.optInAdmin(t, 10, "p10", "gOnEkEy0123456789abcdefghijklm", nil) // deleted account
	h.setAccount("9", fakeAccount{"sam", "user", true})               // demoted

	h.mustHandle(t, sessionEvent(t, eventSessionStarted, "e1", "session_started", nil))
	if n, m := len(h.sentTo(testUserKey)), len(h.sentTo(testAdmin2Key)); n != 1 || m != 0 {
		t.Fatalf("admin 7 got %d (want 1), demoted admin 9 got %d (want 0)", n, m)
	}
	if n := len(h.pushover.messages()); n != 1 {
		t.Fatalf("sent %d messages, want 1", n)
	}

	// Roles are cached for about 10 minutes.
	h.mustHandle(t, sessionEvent(t, eventSessionPaused, "e2", "session_paused", nil))
	if n := h.userLookups("7"); n != 1 {
		t.Fatalf("looked up account 7 %d times, want 1 (cached)", n)
	}
	h.plugin.silo.now = func() time.Time { return time.Now().Add(userCacheTTL) }
	h.setAccount("7", fakeAccount{"josh", "admin", false}) // disabled
	h.mustHandle(t, sessionEvent(t, eventSessionResumed, "e3", "session_paused", nil))
	if n := len(h.pushover.messages()); n != 2 {
		t.Fatalf("a disabled admin still got alerts (%d messages)", n)
	}
}

func TestRoleCheckNetworkErrorRetries(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, nil)
	req := sessionEvent(t, eventSessionStarted, "e1", "session_started", nil)
	h.siloDown.Store(true)
	if err := h.handle(t, req); err == nil {
		t.Fatal("a failed role check should ask Silo to retry")
	}
	if n := len(h.pushover.messages()); n != 0 {
		t.Fatalf("sent %d messages without a role check", n)
	}
	h.siloDown.Store(false)
	h.mustHandle(t, req)
	if n := len(h.pushover.messages()); n != 1 {
		t.Fatalf("retry sent %d messages, want 1", n)
	}
}

func TestRejectedKeyStopsAdminAlertsAndLogsOnce(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, nil)
	logs := &syncBuffer{}
	h.plugin.logger = slog.New(slog.NewTextHandler(logs, nil))
	h.plugin.setConfig(adminConfig{MonitorAPIKey: "sa_revoked", DefaultAppToken: testAppToken})

	for i, name := range []string{eventSessionStarted, eventSessionPaused} {
		h.plugin.silo.Invalidate() // past the 30 s key-error cache
		if err := h.handle(t, sessionEvent(t, name, "e"+string(rune('1'+i)), "session_started", nil)); err != nil {
			t.Fatalf("a rejected key should acknowledge the event, got %v", err)
		}
	}
	if n := len(h.pushover.messages()); n != 0 {
		t.Fatalf("sent %d alerts with a rejected key", n)
	}
	if n := strings.Count(logs.String(), "admin alerts are paused"); n != 1 {
		t.Fatalf("logged the rejected key %d times, want 1:\n%s", n, logs)
	}
	if strings.Contains(logs.String(), "sa_revoked") {
		t.Fatal("logged the key")
	}
	status, out := h.request(t, http.MethodGet, "/api/admin/alerts", asPrimary, nil)
	if status != http.StatusConflict || out["code"] != "monitor_key_rejected" {
		t.Fatalf("GET alerts with a rejected key: %d %v", status, out)
	}
}

func TestAdminWithoutPushoverIsSkipped(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, nil)
	// Admin 9 turned alerts on but their profile has no Pushover key.
	a := defaultAdminSettings()
	a.UserID, a.ProfileID = 9, "p9"
	a.Alerts[alertStreamStarted] = AlertSettings{Enabled: true, Title: "t", Message: "m"}
	if err := h.plugin.admins.Put(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	h.mustHandle(t, sessionEvent(t, eventSessionStarted, "e1", "session_started", nil))
	if n := len(h.pushover.messages()); n != 1 {
		t.Fatalf("sent %d messages, want 1", n)
	}
}

func TestAdminAlertsEndpoints(t *testing.T) {
	h := newHarness(t)
	h.configure(t, func(s *ProfileSettings) { s.UserID = 7 })

	// Only an acting admin.
	for _, as := range []viewerAs{asKid, {profileID: "p1", primary: true}} {
		if status, _ := h.request(t, http.MethodGet, "/api/admin/alerts", as, nil); status != http.StatusForbidden {
			t.Fatalf("non-admin GET: %d, want 403", status)
		}
	}

	status, out := h.request(t, http.MethodGet, "/api/admin/alerts", asPrimary, nil)
	if status != http.StatusOK || out["configured"] != false || out["pushover_ready"] != true {
		t.Fatalf("GET: %d %v", status, out)
	}
	settings := out["settings"].(map[string]any)
	if noise := settings["noise"].(map[string]any); noise["mode"] != noiseBinge || noise["window_minutes"] != float64(30) {
		t.Fatalf("default noise = %v", noise)
	}
	alerts := settings["alerts"].(map[string]any)
	if len(alerts) != len(adminAlertTypes) {
		t.Fatalf("alerts = %v", alerts)
	}
	for key, a := range alerts {
		if a.(map[string]any)["enabled"] != false {
			t.Fatalf("%s is on by default", key)
		}
	}
	if types := out["alert_types"].([]any); len(types) != len(adminAlertTypes) {
		t.Fatalf("alert_types = %v", types)
	}

	priority := -1
	save := map[string]any{
		"alerts": map[string]any{
			alertStreamStarted: map[string]any{"enabled": true, "title": "{user} is watching {title}", "message": "{device}", "sound": "bike", "priority": priority},
		},
		"filters": map[string]any{"ignore_users": []string{"13", "13"}, "only_remote": true, "media_types": []string{"Episode"}},
		"noise":   map[string]any{"mode": noiseCooldown, "window_minutes": 15},
	}
	status, out = h.request(t, http.MethodPut, "/api/admin/alerts", asPrimary, save)
	if status != http.StatusOK || out["warning"] != "" {
		t.Fatalf("PUT: %d %v", status, out)
	}
	stored, found, err := h.plugin.admins.Get(context.Background(), 7)
	if err != nil || !found {
		t.Fatalf("not stored: %v", err)
	}
	if stored.ProfileID != "p1" || !stored.alert(alertStreamStarted).Enabled || stored.alert(alertStreamStopped).Enabled ||
		strings.Join(stored.Filters.IgnoreUsers, ",") != "13" || strings.Join(stored.Filters.MediaTypes, ",") != "episode" ||
		stored.Noise != (NoiseSettings{Mode: noiseCooldown, WindowMinutes: 15}) {
		t.Fatalf("stored = %+v", stored)
	}
	if _, ok := h.state.values[adminBucketKey(7)]; !ok {
		t.Fatalf("settings not under %s", adminBucketKey(7))
	}
	if _, out = h.request(t, http.MethodGet, "/api/admin/alerts", asPrimary, nil); out["configured"] != true {
		t.Fatalf("GET after save: %v", out)
	}

	// The saved settings drive delivery.
	h.mustHandle(t, sessionEvent(t, eventSessionStarted, "e1", "session_started", nil))
	sent := h.sentTo(testUserKey)
	if len(sent) != 1 || sent[0].Get("title") != "alice is watching Severance S02E01" || sent[0].Get("sound") != "bike" || sent[0].Get("priority") != "-1" {
		t.Fatalf("sent = %v", sent)
	}

	for name, bad := range map[string]map[string]any{
		"noise mode": {"noise": map[string]any{"mode": "loud"}},
		"window":     {"noise": map[string]any{"window_minutes": 5000}},
		"priority":   {"alerts": map[string]any{alertStreamStarted: map[string]any{"priority": 3}}},
		"user id":    {"filters": map[string]any{"ignore_users": []string{"a b"}}},
		"media type": {"filters": map[string]any{"media_types": []string{"movie!"}}},
		"long title": {"alerts": map[string]any{alertStreamStarted: map[string]any{"title": strings.Repeat("x", 1025)}}},
		"sound":      {"alerts": map[string]any{alertStreamStarted: map[string]any{"sound": "a b"}}},
	} {
		if status, out := h.request(t, http.MethodPut, "/api/admin/alerts", asPrimary, bad); status != http.StatusUnprocessableEntity {
			t.Fatalf("bad %s: %d %v", name, status, out)
		}
	}

	if status, _ := h.request(t, http.MethodDelete, "/api/admin/alerts", asPrimary, nil); status != http.StatusOK {
		t.Fatalf("DELETE: %d", status)
	}
	if _, found, _ := h.plugin.admins.Get(context.Background(), 7); found {
		t.Fatal("DELETE left the settings")
	}
	if status, _ := h.request(t, http.MethodPatch, "/api/admin/alerts", asPrimary, nil); status != http.StatusMethodNotAllowed {
		t.Fatalf("PATCH: %d, want 405", status)
	}
	if status, _ := h.request(t, http.MethodGet, "/api/admin/nope", asPrimary, nil); status != http.StatusNotFound {
		t.Fatalf("unknown route: %d, want 404", status)
	}
}

func TestAdminSaveWarnsWithoutPushover(t *testing.T) {
	h := newHarness(t)
	save := map[string]any{"alerts": map[string]any{alertStreamStarted: map[string]any{"enabled": true}}}
	status, out := h.request(t, http.MethodPut, "/api/admin/alerts", asPrimary, save)
	if status != http.StatusOK || !strings.Contains(out["warning"].(string), "Pushover user key") {
		t.Fatalf("PUT: %d %v", status, out)
	}
	if _, out = h.request(t, http.MethodGet, "/api/admin/alerts", asPrimary, nil); out["pushover_ready"] != false || out["pushover_message"] == nil {
		t.Fatalf("GET: %v", out)
	}
}

func TestAdminTestAlert(t *testing.T) {
	h := newHarness(t)
	body := map[string]any{"alert": alertStreamStarted, "settings": map[string]any{}}
	if status, out := h.request(t, http.MethodPost, "/api/admin/alerts/test", asPrimary, body); status != http.StatusUnprocessableEntity || out["code"] != "pushover_not_set_up" {
		t.Fatalf("test without Pushover: %d %v", status, out)
	}
	h.configure(t, func(s *ProfileSettings) { s.UserID = 7 })
	if status, out := h.request(t, http.MethodPost, "/api/admin/alerts/test", asPrimary, map[string]any{"alert": "nope"}); status != http.StatusUnprocessableEntity {
		t.Fatalf("unknown alert: %d %v", status, out)
	}

	// Unsaved wording is used.
	body["settings"] = map[string]any{"alerts": map[string]any{alertStreamStopped: map[string]any{"title": "{user} done with {title}", "message": "{watched}"}}}
	body["alert"] = alertStreamStopped
	if status, out := h.request(t, http.MethodPost, "/api/admin/alerts/test", asPrimary, body); status != http.StatusOK {
		t.Fatalf("test: %d %v", status, out)
	}
	sent := h.sentTo(testUserKey)
	if len(sent) != 1 || sent[0].Get("title") != "[Test] alice done with Severance S02E01" || sent[0].Get("message") != "42m" {
		t.Fatalf("sent = %v", sent)
	}
	if _, found, _ := h.plugin.admins.Get(context.Background(), 7); found {
		t.Fatal("a test send saved settings")
	}
}

func TestAdminUsersList(t *testing.T) {
	h := newHarness(t)
	status, out := h.request(t, http.MethodGet, "/api/admin/users", asPrimary, nil)
	if status != http.StatusOK {
		t.Fatalf("users: %d %v", status, out)
	}
	var names []string
	for _, u := range out["users"].([]any) {
		names = append(names, u.(map[string]any)["username"].(string))
	}
	if strings.Join(names, ",") != "alice,bob,josh,sam" {
		t.Fatalf("users = %v", names)
	}
}

func TestStateReportsAdminFlag(t *testing.T) {
	h := newHarness(t)
	for as, want := range map[viewerAs]bool{asPrimary: true, asKid: false} {
		if _, state := h.request(t, http.MethodGet, "/api/state", as, nil); state["admin"] != want {
			t.Fatalf("%+v: admin = %v, want %v", as, state["admin"], want)
		}
	}
}

func TestAdminStoreIsSharedAcrossServers(t *testing.T) {
	state := &memoryState{values: map[string][]byte{}}
	ctx := context.Background()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	a := newAdminStore(state)
	a.now = func() time.Time { return now }
	if list, err := a.List(ctx); err != nil || len(list) != 0 {
		t.Fatalf("List = %v, %v", list, err)
	}

	b := newAdminStore(state)
	s := defaultAdminSettings()
	s.UserID, s.ProfileID = 7, "p1"
	if err := b.Put(ctx, s); err != nil {
		t.Fatal(err)
	}
	if list, _ := a.List(ctx); len(list) != 0 {
		t.Fatal("List should use its cache inside the TTL")
	}
	now = now.Add(settingsCacheTTL)
	if list, _ := a.List(ctx); len(list) != 1 || list[0].ProfileID != "p1" {
		t.Fatalf("List after TTL = %v", list)
	}
	if got, found, _ := a.Get(ctx, 7); !found || got.UserID != 7 {
		t.Fatal("Get should read state directly")
	}
}

func TestThrottleStatePrunesOldEntries(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	sessions := sessionTable{
		"old-stop": {End: now.Add(-25 * time.Hour)},
		"new-stop": {End: now.Add(-time.Hour)},
		"old-live": {Start: now.Add(-13 * time.Hour)},
		"new-live": {Start: now.Add(-time.Hour)},
	}
	sessions.prune(now)
	if _, ok := sessions["new-stop"]; !ok || len(sessions) != 2 || sessions["new-live"] == nil {
		t.Fatalf("sessions after prune = %v", sessions)
	}
	st := &throttleState{
		Cooldowns: map[string]cooldownMark{"old": {At: now.Add(-25 * time.Hour)}, "new": {At: now}},
		Over:      map[string]cooldownMark{"old": {At: now.Add(-8 * 24 * time.Hour)}, "new": {At: now.Add(-time.Hour)}},
	}
	st.prune(now)
	if len(st.Cooldowns) != 1 || len(st.Over) != 1 {
		t.Fatalf("after prune: cooldowns %v, limit marks %v", st.Cooldowns, st.Over)
	}
}

func TestRenderTemplateTidiesEmptyValues(t *testing.T) {
	vars := map[string]string{"a": "Remote", "b": "", "c": "8 Mbps", "d": ""}
	for tmpl, want := range map[string]string{
		"{a} · {b} · {c}":   "Remote · 8 Mbps",
		"Watched {d}\nNext": "Next",
		"Plain text":        "Plain text",
		"{b} | {d} | {a}":   "Remote",
	} {
		if got := renderTemplate(tmpl, vars, false); got != want {
			t.Errorf("renderTemplate(%q) = %q, want %q", tmpl, got, want)
		}
	}
}
