package main

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// Tests for quiet hours, emergency priority, posters, quota tracking, the
// "Open in Silo" link, the settings check and the digest schedule.

// testNow is Tuesday 2026-11-03 20:00 UTC, which is 15:00 in New York (EST)
// and 05:00 on Wednesday in Tokyo.

func quietAround(tz string, start, end string, silenceEmergency bool) func(*AdminSettings) {
	return func(a *AdminSettings) {
		a.TimeZone = tz
		a.Quiet = QuietHours{Enabled: true, Start: start, End: end, SilenceEmergency: silenceEmergency}
	}
}

func alertPriority(key string, p int) func(*AdminSettings) {
	return func(a *AdminSettings) {
		s := a.alert(key)
		s.Priority = &p
		a.Alerts[key] = s
	}
}

func TestQuietHoursActive(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 11, 3, h, m, 0, 0, time.UTC) }
	night := QuietHours{Enabled: true, Start: "22:00", End: "07:00"}
	for _, c := range []struct {
		q    QuietHours
		t    time.Time
		want bool
	}{
		{night, at(23, 0), true},
		{night, at(22, 0), true},
		{night, at(3, 30), true},
		{night, at(6, 59), true},
		{night, at(7, 0), false},
		{night, at(12, 0), false},
		{night, at(21, 59), false},
		{QuietHours{Enabled: true, Start: "13:00", End: "14:30"}, at(14, 29), true},
		{QuietHours{Enabled: true, Start: "13:00", End: "14:30"}, at(14, 30), false},
		{QuietHours{Enabled: false, Start: "00:00", End: "23:59"}, at(12, 0), false},
		{QuietHours{Enabled: true, Start: "08:00", End: "08:00"}, at(8, 0), false},
		{QuietHours{Enabled: true, Start: "bad", End: "08:00"}, at(8, 0), false},
	} {
		if got := c.q.active(c.t); got != c.want {
			t.Errorf("%+v at %s: active = %v, want %v", c.q, c.t.Format("15:04"), got, c.want)
		}
	}
}

func TestPriorityAtQuietHours(t *testing.T) {
	a := defaultAdminSettings()
	a.TimeZone = "America/New_York"
	a.Quiet = QuietHours{Enabled: true, Start: "14:00", End: "16:00"}
	// 20:00 UTC is 15:00 in New York: quiet.
	for _, c := range []struct{ in, want int }{{1, -1}, {0, -1}, {-1, -1}, {-2, -2}, {2, 2}} {
		if got := a.priorityAt(nil, c.in, testNow); got != c.want {
			t.Errorf("quiet: priority %d became %d, want %d", c.in, got, c.want)
		}
	}
	a.Quiet.SilenceEmergency = true
	if got := a.priorityAt(nil, 2, testNow); got != -1 {
		t.Errorf("emergency with SilenceEmergency became %d, want -1", got)
	}
	// 21:00 UTC is 16:00 in New York: over. The same instant in UTC is not
	// in a quiet window set for New York's afternoon.
	if got := a.priorityAt(nil, 1, testNow.Add(time.Hour)); got != 1 {
		t.Errorf("after quiet hours priority became %d", got)
	}
	a.TimeZone = "UTC"
	if got := a.priorityAt(nil, 0, testNow); got != 0 {
		t.Errorf("14:00-16:00 must not apply at 20:00 UTC, got %d", got)
	}
	// An unknown zone falls back to UTC rather than failing.
	a.TimeZone = "Mars/Olympus"
	if a.location() != time.UTC {
		t.Errorf("unknown zone location = %v", a.location())
	}
}

// setProfileQuiet gives a saved profile quiet hours.
func (h *harness) setProfileQuiet(t *testing.T, profileID, tz string, q QuietHours) {
	t.Helper()
	ctx := context.Background()
	s, found, err := h.plugin.store.Get(ctx, profileID)
	if err != nil || !found {
		t.Fatalf("profile %s: %v %v", profileID, found, err)
	}
	s.TimeZone, s.Quiet = tz, q
	if err := h.plugin.store.Put(ctx, s); err != nil {
		t.Fatal(err)
	}
}

// Admin alerts follow the quiet hours of the admin's profile (the Pushover
// account tab); those win over quiet hours saved in the admin settings.
func TestAdminAlertsFollowProfileQuietHours(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, and(onlyAlerts(alertStreamStarted), quietAround("America/New_York", "22:00", "07:00", false)))
	h.setProfileQuiet(t, "p1", "America/New_York", QuietHours{Enabled: true, Start: "14:00", End: "16:00"})
	h.mustHandle(t, startedEvent(t, "e1"))
	if got := h.sentTo(testUserKey); len(got) != 1 || got[0].Get("priority") != "-1" {
		t.Fatalf("profile quiet hours (15:00 New York) should lower the alert: %v", got)
	}

	// The profile's emergency switch decides for emergency alerts.
	h2 := newHarness(t)
	h2.optInAdmin(t, 7, "p1", testUserKey, and(onlyAlerts(alertStreamStarted), alertPriority(alertStreamStarted, 2)))
	h2.setProfileQuiet(t, "p1", "America/New_York", QuietHours{Enabled: true, Start: "14:00", End: "16:00", SilenceEmergency: true})
	h2.mustHandle(t, startedEvent(t, "e1"))
	if got := h2.sentTo(testUserKey); len(got) != 1 || got[0].Get("priority") != "-1" {
		t.Fatalf("emergency with the profile's silence switch on: %v", got)
	}
}

// Opening the admin settings moves quiet hours saved in them onto the admin's
// profile, once, and clears them from the admin settings.
func TestAdminQuietHoursMoveToProfile(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, quietAround("America/New_York", "21:30", "06:45", true))
	if status, out := h.request(t, http.MethodGet, "/api/admin/alerts", asPrimary, nil); status != http.StatusOK {
		t.Fatalf("GET alerts: %d %v", status, out)
	}
	ctx := context.Background()
	prof, _, _ := h.plugin.store.Get(ctx, "p1")
	if !prof.Quiet.Enabled || prof.Quiet.Start != "21:30" || prof.Quiet.End != "06:45" || !prof.Quiet.SilenceEmergency || prof.TimeZone != "America/New_York" {
		t.Fatalf("profile quiet hours after the move: %+v %q", prof.Quiet, prof.TimeZone)
	}
	a, _, _ := h.plugin.admins.Get(ctx, 7)
	if a.Quiet != (QuietHours{}) {
		t.Fatalf("admin quiet hours not cleared: %+v", a.Quiet)
	}

	// A profile that already has quiet hours keeps its own.
	h2 := newHarness(t)
	h2.optInAdmin(t, 7, "p1", testUserKey, quietAround("UTC", "01:00", "02:00", false))
	h2.setProfileQuiet(t, "p1", "Europe/London", QuietHours{Enabled: true, Start: "23:00", End: "06:00"})
	h2.request(t, http.MethodGet, "/api/admin/alerts", asPrimary, nil)
	prof2, _, _ := h2.plugin.store.Get(ctx, "p1")
	if prof2.Quiet.Start != "23:00" || prof2.TimeZone != "Europe/London" {
		t.Fatalf("the profile's own quiet hours were replaced: %+v %q", prof2.Quiet, prof2.TimeZone)
	}
}

// Only an admin's own profile keeps "turn emergency alerts down too".
func TestProfileSilenceEmergencyOnlyForAdminsOwnProfile(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, nil)
	save := func(as viewerAs, profileID string) bool {
		t.Helper()
		_, view := h.request(t, http.MethodGet, "/api/profiles/"+profileID, as, nil)
		view["quiet"] = map[string]any{"enabled": true, "start": "22:00", "end": "07:00", "silence_emergency": true}
		view["user_key"] = testUserKey
		status, out := h.request(t, http.MethodPut, "/api/profiles/"+profileID, as, view)
		if status != http.StatusOK {
			t.Fatalf("save %s: %d %v", profileID, status, out)
		}
		s, _, _ := h.plugin.store.Get(context.Background(), profileID)
		return s.Quiet.SilenceEmergency
	}
	if !save(asPrimary, "p1") {
		t.Error("the admin's own profile lost the emergency switch")
	}
	if save(asKid, "p2") {
		t.Error("a non-admin profile kept the emergency switch")
	}
}

func startedEvent(t *testing.T, id string, edit ...func(map[string]any)) *pluginv1.HandleEventRequest {
	t.Helper()
	return sessionEvent(t, eventSessionStarted, id, "session_started", edits(edit...))
}

func TestQuietHoursLowerAdminAlertPriority(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, and(onlyAlerts(alertStreamStarted), quietAround("America/New_York", "14:00", "16:00", false)))
	h.mustHandle(t, startedEvent(t, "e1"))
	sent := h.sentTo(testUserKey)
	if len(sent) != 1 || sent[0].Get("priority") != "-1" {
		t.Fatalf("a quiet-hours alert went out as %v", sent)
	}

	// Outside the hours it goes out as the profile says (normal).
	h2 := newHarness(t)
	h2.optInAdmin(t, 7, "p1", testUserKey, and(onlyAlerts(alertStreamStarted), quietAround("America/New_York", "22:00", "07:00", false)))
	h2.mustHandle(t, startedEvent(t, "e1"))
	if got := h2.sentTo(testUserKey); len(got) != 1 || got[0].Get("priority") != "0" {
		t.Fatalf("outside quiet hours: %v", got)
	}

	// The time of the send decides, not the event's own time.
	h3 := newHarness(t)
	h3.optInAdmin(t, 7, "p1", testUserKey, and(onlyAlerts(alertStreamStarted), quietAround("UTC", "20:00", "21:00", false)))
	h3.mustHandle(t, startedEvent(t, "e1", at(-300)))
	if got := h3.sentTo(testUserKey); len(got) != 1 || got[0].Get("priority") != "-1" {
		t.Fatalf("quiet hours should follow the clock at send time: %v", got)
	}
}

func TestEmergencyAlertsRetryAndStillRingInQuietHours(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, and(onlyAlerts(alertStreamStarted, alertPlaybackFailed),
		quietAround("America/New_York", "14:00", "16:00", false),
		alertPriority(alertStreamStarted, 2)))
	h.mustHandle(t, startedEvent(t, "e1"))
	sent := h.sentTo(testUserKey)
	if len(sent) != 1 {
		t.Fatalf("sent %d", len(sent))
	}
	if m := sent[0]; m.Get("priority") != "2" || m.Get("retry") != "60" || m.Get("expire") != "3600" {
		t.Fatalf("emergency alert = priority %q retry %q expire %q", m.Get("priority"), m.Get("retry"), m.Get("expire"))
	}

	// Turning emergency alerts down too: quiet priority, no retry or expire.
	h2 := newHarness(t)
	h2.optInAdmin(t, 7, "p1", testUserKey, and(onlyAlerts(alertStreamStarted),
		quietAround("America/New_York", "14:00", "16:00", true),
		alertPriority(alertStreamStarted, 2)))
	h2.mustHandle(t, startedEvent(t, "e1"))
	if m := h2.sentTo(testUserKey)[0]; m.Get("priority") != "-1" || m.Get("retry") != "" || m.Get("expire") != "" {
		t.Fatalf("silenced emergency alert = priority %q retry %q expire %q", m.Get("priority"), m.Get("retry"), m.Get("expire"))
	}

	// Other priorities never carry retry or expire.
	h3 := newHarness(t)
	h3.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertStreamStarted))
	h3.mustHandle(t, startedEvent(t, "e1"))
	if m := h3.sentTo(testUserKey)[0]; m.Get("retry") != "" || m.Get("expire") != "" {
		t.Fatalf("normal alert carries retry %q expire %q", m.Get("retry"), m.Get("expire"))
	}
}

func TestEmergencyPriorityIsAdminOnly(t *testing.T) {
	h := newHarness(t)
	h.configure(t, nil)
	// An admin alert may be an emergency; a profile's notification may not.
	status, out := h.request(t, http.MethodPut, "/api/admin/alerts", asPrimary, map[string]any{
		"alerts": map[string]any{alertStreamStarted: map[string]any{"enabled": true, "priority": 2}},
	})
	if status != http.StatusOK {
		t.Fatalf("emergency admin alert: %d %v", status, out)
	}
	status, _ = h.request(t, http.MethodPut, "/api/profiles/p1", asPrimary, map[string]any{
		"enabled": true, "user_key": testUserKey, "priority": 2,
	})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("profile priority 2: %d", status)
	}
}

func TestAdminTestAlertDowngradesEmergencyAndSkipsQuietHours(t *testing.T) {
	h := newHarness(t)
	h.configure(t, nil)
	status, out := h.request(t, http.MethodPost, "/api/admin/alerts/test", asPrimary, map[string]any{
		"alert": alertStreamStarted,
		"settings": map[string]any{
			"alerts": map[string]any{alertStreamStarted: map[string]any{"enabled": true, "priority": 2}},
			"quiet":  map[string]any{"enabled": true, "start": "00:00", "end": "23:59"},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("test: %d %v", status, out)
	}
	m := h.sentTo(testUserKey)[0]
	if m.Get("priority") != "1" || m.Get("retry") != "" {
		t.Fatalf("test alert priority %q retry %q: an emergency test must not repeat", m.Get("priority"), m.Get("retry"))
	}
	if !strings.HasPrefix(m.Get("title"), "[Test] ") {
		t.Fatalf("title = %q", m.Get("title"))
	}
	if m.Get("url") != "https://silo.example.com/admin/activity" {
		t.Fatalf("test alert link = %q", m.Get("url"))
	}
}

// --- Posters ---

func TestPosterAttachedToPlaybackAlerts(t *testing.T) {
	poster := withRow(map[string]any{"poster_url": "/poster.jpg"})
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, and(onlyAlerts(alertStreamStarted, alertStreamStopped, alertScanFailed), func(a *AdminSettings) { a.AttachPoster = true }))
	h.mustHandle(t,
		sessionEvent(t, eventSessionStarted, "e1", "session_started", poster),
		// A stop's history entry has no poster; the start's is remembered.
		sessionEvent(t, eventSessionStopped, "e2", "session_stopped", nil),
		serverEvent(t, eventScanFailed, "01JSCAN", scanFailedPayload()),
	)
	sent := h.sentTo(testUserKey)
	if len(sent) != 3 {
		t.Fatalf("sent %d alerts: %q", len(sent), titles(sent))
	}
	if sent[0].Get("_attachment") != "yes" || sent[1].Get("_attachment") != "yes" {
		t.Fatalf("start and stop attachments = %q, %q", sent[0].Get("_attachment"), sent[1].Get("_attachment"))
	}
	if sent[2].Get("_attachment") != "" {
		t.Fatal("a scan failure carries a poster")
	}

	// Off by default.
	h2 := newHarness(t)
	h2.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertStreamStarted))
	h2.mustHandle(t, sessionEvent(t, eventSessionStarted, "e1", "session_started", poster))
	if got := h2.sentTo(testUserKey); len(got) != 1 || got[0].Get("_attachment") != "" {
		t.Fatalf("poster switch off: %v", got)
	}
}

func TestPosterOnlyForAdminsWhoWantIt(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, and(onlyAlerts(alertStreamStarted), func(a *AdminSettings) { a.AttachPoster = true }))
	h.optInAdmin(t, 9, "p9", testAdmin2Key, onlyAlerts(alertStreamStarted))
	h.mustHandle(t, sessionEvent(t, eventSessionStarted, "e1", "session_started", withRow(map[string]any{"poster_url": "/poster.jpg"})))
	if got := h.sentTo(testUserKey)[0].Get("_attachment"); got != "yes" {
		t.Fatalf("admin who wants posters got attachment %q", got)
	}
	if got := h.sentTo(testAdmin2Key)[0].Get("_attachment"); got != "" {
		t.Fatalf("admin who doesn't got attachment %q", got)
	}
}

func TestSlowOrMissingPosterStillSendsAlert(t *testing.T) {
	for name, posterURL := range map[string]string{"slow": "/slow.jpg", "missing": "", "unreachable": "/nope.jpg"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.plugin.posterTimeout = 150 * time.Millisecond
			h.optInAdmin(t, 7, "p1", testUserKey, and(onlyAlerts(alertStreamStarted), func(a *AdminSettings) { a.AttachPoster = true }))
			start := time.Now()
			h.mustHandle(t, sessionEvent(t, eventSessionStarted, "e1", "session_started", withRow(map[string]any{"poster_url": posterURL})))
			if time.Since(start) > 2*time.Second {
				t.Fatalf("waited %v for a poster", time.Since(start))
			}
			if got := h.sentTo(testUserKey); len(got) != 1 || got[0].Get("_attachment") != "" {
				t.Fatalf("alert = %v", got)
			}
		})
	}
}

// --- Open in Silo link ---

func TestOpenInSiloLinkOnPlaybackAndLimitAlerts(t *testing.T) {
	const link = "https://silo.example.com/admin/activity"
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, and(onlyAlerts(alertStreamStarted, alertStreamsServer, alertScanFailed, alertNewDevice),
		func(a *AdminSettings) { a.Limits.ServerStreams = 1 }))
	ep := liveRow(t, "session_started", nil)
	other := liveRow(t, "session_started", map[string]any{"session_id": "s2"})
	h.setLive(ep, other)
	h.mustHandle(t, startedEvent(t, "e1"), serverEvent(t, eventScanFailed, "01JSCAN", scanFailedPayload()))
	byTitle := map[string]url.Values{}
	for _, m := range h.sentTo(testUserKey) {
		byTitle[m.Get("title")] = m
	}
	if m := byTitle["alice (Kids) started Severance S02E01"]; m.Get("url") != link || m.Get("url_title") != "Open in Silo" {
		t.Fatalf("playback alert link = %q (%q)", m.Get("url"), m.Get("url_title"))
	}
	if m := byTitle["2 streams playing"]; m.Get("url") != link {
		t.Fatalf("stream-limit alert link = %q; alerts: %v", m.Get("url"), titles(h.sentTo(testUserKey)))
	}
	if m := byTitle["Scan failed: Movies"]; m.Get("url") != "" {
		t.Fatalf("scan alert has a link: %q", m.Get("url"))
	}
	if m := byTitle["New device for alice (Kids)"]; m != nil && m.Get("url") != "" {
		t.Fatalf("new-device alert has a link: %q", m.Get("url"))
	}

	// The public URL override wins over the host's.
	h.plugin.setConfig(adminConfig{MonitorAPIKey: testMonitorKey, DefaultAppToken: testAppToken, PublicURL: "https://media.example.org"})
	h.mustHandle(t, startedEvent(t, "e2", asSession("s9"), at(30)))
	var last url.Values
	for _, m := range h.sentTo(testUserKey) {
		if strings.Contains(m.Get("title"), "started") {
			last = m
		}
	}
	if last.Get("url") != "https://media.example.org/admin/activity" {
		t.Fatalf("link with override = %q", last.Get("url"))
	}
}

func TestOpenInSiloLinkSwitch(t *testing.T) {
	h := newHarness(t)
	off := false
	h.optInAdmin(t, 7, "p1", testUserKey, and(onlyAlerts(alertStreamStarted), func(a *AdminSettings) { a.SiloLink = &off }))
	h.mustHandle(t, startedEvent(t, "e1"))
	if m := h.sentTo(testUserKey)[0]; m.Get("url") != "" || m.Get("url_title") != "" {
		t.Fatalf("link with the switch off: %q", m.Get("url"))
	}

	// Settings saved before the switch existed have it on, and the page sees that.
	h2 := newHarness(t)
	h2.configure(t, nil)
	_, out := h2.request(t, http.MethodGet, "/api/admin/alerts", asPrimary, nil)
	if s := out["settings"].(map[string]any); s["silo_link"] != true {
		t.Fatalf("default silo_link = %v", s["silo_link"])
	}
	// Saving the switch off sticks.
	status, out := h2.request(t, http.MethodPut, "/api/admin/alerts", asPrimary, map[string]any{"silo_link": false})
	if status != http.StatusOK || out["settings"].(map[string]any)["silo_link"] != false {
		t.Fatalf("save: %d %v", status, out["settings"])
	}
	_, out = h2.request(t, http.MethodGet, "/api/admin/alerts", asPrimary, nil)
	if out["settings"].(map[string]any)["silo_link"] != false {
		t.Fatal("the switch didn't stick")
	}
}

// --- Quota ---

func TestQuotaIsTrackedFromHeaders(t *testing.T) {
	h := newHarness(t)
	h.configure(t, nil)
	h.pushover.quotaLimit.Store(10000)
	h.pushover.quotaRemaining.Store(9500)
	h.pushover.quotaReset.Store(time.Now().Add(72 * time.Hour).Unix())
	h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertStreamStarted))
	h.mustHandle(t, startedEvent(t, "e1"))

	quota := func(h *harness) map[string]any {
		_, out := h.request(t, http.MethodGet, "/api/admin/alerts", asPrimary, nil)
		q, _ := out["quota"].(map[string]any)
		return q
	}
	q := quota(h)
	if q == nil || q["remaining"] != float64(9500) || q["limit"] != float64(10000) || q["low"] != false {
		t.Fatalf("quota = %v", q)
	}

	// Another API server reads the same reading from state.
	other := &harness{plugin: h.newPlugin(h.state), pushover: h.pushover, pushURL: h.pushURL, siloURL: h.siloURL}
	other.routes = &routesServer{plugin: other.plugin}
	if q := quota(other); q == nil || q["remaining"] != float64(9500) {
		t.Fatalf("a second server saw %v", q)
	}

	// Under 10 percent: the warning flag.
	h.pushover.quotaRemaining.Store(900)
	h.mustHandle(t, startedEvent(t, "e2", asSession("s2"), at(10)))
	if q := quota(h); q["low"] != true || q["remaining"] != float64(900) {
		t.Fatalf("low quota = %v", q)
	}
	// Exactly 10 percent is not yet under it.
	h.pushover.quotaRemaining.Store(1000)
	h.mustHandle(t, startedEvent(t, "e3", asSession("s3"), at(20)))
	if q := quota(h); q["low"] != false {
		t.Fatalf("10 percent flagged low: %v", q)
	}

	// The state holds a hash of the token, never the token.
	raw, _, _ := h.state.Read(context.Background(), quotaKey)
	if len(raw) == 0 || strings.Contains(string(raw), testAppToken) {
		t.Fatalf("quota state = %q", raw)
	}
}

func TestQuotaIgnoresOutdatedAndMissingReadings(t *testing.T) {
	h := newHarness(t)
	h.configure(t, nil)
	// Pushover sends no quota headers: nothing to show.
	h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertStreamStarted))
	h.mustHandle(t, startedEvent(t, "e1"))
	if _, out := h.request(t, http.MethodGet, "/api/admin/alerts", asPrimary, nil); out["quota"] != nil {
		t.Fatalf("quota without headers = %v", out["quota"])
	}
	// A reading from before Pushover's last monthly reset no longer counts.
	h.plugin.quota.record(context.Background(), testAppToken, quota{Limit: 10000, Remaining: 3, Reset: testNow.Add(-time.Hour).Unix()})
	h.plugin.quota.now = func() time.Time { return testNow }
	if _, out := h.request(t, http.MethodGet, "/api/admin/alerts", asPrimary, nil); out["quota"] != nil {
		t.Fatalf("an outdated reading = %v", out["quota"])
	}
}

func TestQuotaFailedSendStillCounts(t *testing.T) {
	// A monthly-limit rejection (429) carries the headers too.
	if q, ok := quotaFromHeaders(http.Header{"X-Limit-App-Limit": {"10000"}, "X-Limit-App-Remaining": {"0"}, "X-Limit-App-Reset": {"1700000000"}}); !ok || q.Remaining != 0 || q.Limit != 10000 {
		t.Fatalf("headers = %+v %v", q, ok)
	}
	for _, bad := range []http.Header{{}, {"X-Limit-App-Limit": {"x"}, "X-Limit-App-Remaining": {"1"}}, {"X-Limit-App-Limit": {"0"}, "X-Limit-App-Remaining": {"0"}}} {
		if _, ok := quotaFromHeaders(bad); ok {
			t.Errorf("headers %v read as a quota", bad)
		}
	}
}

func TestQuotaTableIsCapped(t *testing.T) {
	st := newMemoryStateForTest()
	s := newQuotaStore(st)
	for i := range maxQuotaEntries * 3 {
		s.now = func() time.Time { return testNow.Add(time.Duration(i) * time.Second) }
		s.record(context.Background(), strings.Repeat("a", 30)+string(rune('A'+i%26))+strings.Repeat("b", i), quota{Limit: 100, Remaining: 50})
	}
	table, _, err := s.read(context.Background())
	if err != nil || len(table) == 0 {
		t.Fatalf("table %d, err %v", len(table), err)
	}
}

func newMemoryStateForTest() *memoryState { return &memoryState{values: map[string][]byte{}} }

// --- Settings check ---

func TestManifestDeclaresConfigurationCheck(t *testing.T) {
	if !strings.Contains(string(manifestJSON), `"configuration_check": true`) {
		t.Fatal("the manifest doesn't declare configuration_check")
	}
}

func TestConfigurationCheck(t *testing.T) {
	h := newHarness(t)
	srv := &checkServer{plugin: h.plugin}
	check := func(fields map[string]any) *pluginv1.TestConfigurationResponse {
		t.Helper()
		value, err := structpb.NewStruct(fields)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.TestConfiguration(context.Background(), &pluginv1.TestConfigurationRequest{Config: []*pluginv1.ConfigEntry{{Key: adminConfigKey, Value: value}}})
		if err != nil {
			t.Fatalf("a failed check must be an answer, not an error: %v", err)
		}
		return resp
	}

	h.pushover.quotaLimit.Store(10000)
	h.pushover.quotaRemaining.Store(8123)
	if resp := check(map[string]any{"default_app_token": testAppToken}); !resp.GetOk() || !strings.Contains(resp.GetMessage(), "8123") {
		t.Fatalf("good token: %v", resp)
	}
	// No shared token is fine: every profile can bring its own.
	if resp := check(map[string]any{}); !resp.GetOk() {
		t.Fatalf("no token: %v", resp)
	}
	bad := "bad" + strings.Repeat("x", 27)
	resp := check(map[string]any{"default_app_token": bad})
	if resp.GetOk() || !strings.Contains(resp.GetMessage(), "rejected") || strings.Contains(resp.GetMessage(), bad) {
		t.Fatalf("rejected token: %v", resp)
	}
	resp = check(map[string]any{"default_app_token": "short"})
	if resp.GetOk() || strings.Contains(resp.GetMessage(), "short") {
		t.Fatalf("malformed token: %v", resp)
	}
	if resp := check(map[string]any{"default_app_token": testAppToken, "public_url": "silo.example.com"}); resp.GetOk() {
		t.Fatalf("a public URL without a scheme passed: %v", resp)
	}
	if resp := check(map[string]any{"default_app_token": testAppToken, "public_url": "https://silo.example.com/"}); !resp.GetOk() {
		t.Fatalf("a good public URL failed: %v", resp)
	}

	// Pushover unreachable: a failed check, not a crash, and no secret.
	h.plugin.pushover.baseURL = "http://127.0.0.1:1"
	resp = check(map[string]any{"default_app_token": testAppToken})
	if resp.GetOk() || strings.Contains(resp.GetMessage(), testAppToken) {
		t.Fatalf("unreachable: %v", resp)
	}

	// The check tests the staged settings and leaves the running ones alone.
	if got := h.plugin.config().DefaultAppToken; got != testAppToken {
		t.Fatalf("the check changed the running config: %q", got)
	}
}

// --- Digest schedule ---

func TestDigestOnlyInTheAdminsChosenHour(t *testing.T) {
	h := newHarness(t)
	// Default schedule (Sunday 18:00), UTC.
	h.optInAdmin(t, 7, "p1", testUserKey, and(onlyAlerts(alertDigest), func(a *AdminSettings) { a.Digest = nil }))
	h.serveDigest(t)
	sunday := time.Date(2026, 11, 8, 18, 0, 0, 0, time.UTC)

	// Outside the hour and its catch-up: no digest and no Silo reads. This is
	// what a restart or the hourly task does at any other time.
	for _, now := range []time.Time{testNow, sunday.Add(-time.Hour), sunday.Add(7 * time.Hour), sunday.Add(-24 * time.Hour)} {
		h.clock(now)
		out := h.mustRun(t, capWeeklyDigest)
		if !strings.HasPrefix(out["status"].(string), "skipped") {
			t.Fatalf("at %s: %v", now.Format(time.RFC1123), out)
		}
	}
	if n := len(h.pushover.messages()); n != 0 || h.callsTo(pathTop) != 0 {
		t.Fatalf("outside the hour: %d messages, %d Silo reads", n, h.callsTo(pathTop))
	}

	h.clock(sunday.Add(30 * time.Minute))
	if out := h.mustRun(t, capWeeklyDigest); out["digests_sent"] != float64(1) {
		t.Fatalf("in the hour: %v", out)
	}
	// Run again in the same hour (another server, a restart): once a week.
	h.clock(sunday.Add(50 * time.Minute))
	h.mustRun(t, capWeeklyDigest)
	if n := len(h.pushover.messages()); n != 1 {
		t.Fatalf("the same week sent %d digests", n)
	}
	// The next Sunday goes out again.
	h.clock(sunday.Add(7 * 24 * time.Hour))
	h.mustRun(t, capWeeklyDigest)
	if n := len(h.pushover.messages()); n != 2 {
		t.Fatalf("the next week: %d digests in all", n)
	}
}

// A week's digest isn't lost when no run lands in the chosen hour (Silo down or
// restarting then): a later run up to 6 hours after sends it, once.
func TestDigestCatchesUpAfterAMissedHour(t *testing.T) {
	sunday := time.Date(2026, 11, 8, 18, 0, 0, 0, time.UTC) // the chosen hour (default Sunday 18:00)
	// The chosen hour's own run never happened. The first run after it is at
	// 00:25 on Monday, in the last hour of the catch-up and already in the next
	// ISO week.
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, and(onlyAlerts(alertDigest), func(a *AdminSettings) { a.Digest = nil }))
	h.serveDigest(t)
	h.clock(sunday.Add(6*time.Hour + 25*time.Minute)) // 00:25 Monday: hour 6 after the chosen one
	if out := h.mustRun(t, capWeeklyDigest); out["digests_sent"] != float64(1) {
		t.Fatalf("a run 6h25m after the chosen hour: %v", out)
	}
	// Later runs the same week, in and past the catch-up, send nothing more,
	// even though Monday is already the next ISO week.
	for _, d := range []time.Duration{6*time.Hour + 55*time.Minute, 5 * time.Hour, 7 * time.Hour, 24 * time.Hour} {
		h.clock(sunday.Add(d))
		h.mustRun(t, capWeeklyDigest)
	}
	if n := len(h.pushover.messages()); n != 1 {
		t.Fatalf("one week sent %d digests", n)
	}

	// The chosen hour's own run and a catch-up run of the same week send once.
	h = newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, and(onlyAlerts(alertDigest), func(a *AdminSettings) { a.Digest = nil }))
	h.serveDigest(t)
	h.clock(sunday.Add(10 * time.Minute))
	h.mustRun(t, capWeeklyDigest)
	h.clock(sunday.Add(3*time.Hour + 10*time.Minute))
	h.mustRun(t, capWeeklyDigest)
	// A late run after midnight (Monday, the next ISO week) is still the same
	// digest, not a second one.
	h.clock(sunday.Add(6*time.Hour + 5*time.Minute))
	h.mustRun(t, capWeeklyDigest)
	if n := len(h.pushover.messages()); n != 1 {
		t.Fatalf("hour run plus catch-up runs sent %d digests", n)
	}
	// The next week's hour still sends.
	h.clock(sunday.Add(7 * 24 * time.Hour))
	h.mustRun(t, capWeeklyDigest)
	if n := len(h.pushover.messages()); n != 2 {
		t.Fatalf("the next week: %d digests in all", n)
	}
}

func TestDigestSlotWindow(t *testing.T) {
	a := defaultAdminSettings()
	a.TimeZone = "America/New_York"
	a.Digest = &DigestSchedule{Day: int(time.Sunday), Hour: 22}
	// Sunday 22:00 New York is Monday 03:00 UTC (EST, UTC-5, after DST ends).
	slot := time.Date(2026, 11, 9, 3, 0, 0, 0, time.UTC)
	week := "2026-W45" // Sunday 2026-11-08
	for _, c := range []struct {
		at  time.Duration
		due bool
	}{
		{-time.Second, false}, {0, true}, {59 * time.Minute, true}, {3 * time.Hour, true},
		{6 * time.Hour, true}, {6*time.Hour + 59*time.Minute, true}, {7 * time.Hour, false}, {24 * time.Hour, false},
	} {
		got, due := a.digestSlot(slot.Add(c.at))
		if due != c.due || (due && got != week) {
			t.Errorf("%v after the chosen hour: due=%v week=%q, want due=%v week=%q", c.at, due, got, c.due, week)
		}
	}
	// 01:00 Monday in New York is after midnight, in the next ISO week, but the
	// digest is still the Sunday's.
	if got, due := a.digestSlot(time.Date(2026, 11, 9, 6, 0, 0, 0, time.UTC)); !due || got != week {
		t.Errorf("after midnight: due=%v week=%q", due, got)
	}
}

func TestDigestUsesEachAdminsTimeZoneAndSchedule(t *testing.T) {
	h := newHarness(t)
	// Sunday 2026-11-08 20:00 UTC is Monday 05:00 in Tokyo and 15:00 on
	// Sunday in New York.
	now := time.Date(2026, 11, 8, 20, 0, 0, 0, time.UTC)
	h.optInAdmin(t, 7, "p1", testUserKey, and(onlyAlerts(alertDigest), func(a *AdminSettings) {
		a.TimeZone, a.Digest = "Asia/Tokyo", &DigestSchedule{Day: int(time.Monday), Hour: 5}
	}))
	h.optInAdmin(t, 9, "p9", testAdmin2Key, and(onlyAlerts(alertDigest), func(a *AdminSettings) {
		a.TimeZone, a.Digest = "America/New_York", &DigestSchedule{Day: int(time.Sunday), Hour: 18}
	}))
	h.serveDigest(t)
	h.clock(now)
	h.mustRun(t, capWeeklyDigest)
	if len(h.sentTo(testUserKey)) != 1 || len(h.sentTo(testAdmin2Key)) != 0 {
		t.Fatalf("at 20:00 UTC: Tokyo admin %d, New York admin %d", len(h.sentTo(testUserKey)), len(h.sentTo(testAdmin2Key)))
	}
	// Three hours on it is 18:00 on Sunday in New York.
	h.clock(now.Add(3 * time.Hour))
	h.mustRun(t, capWeeklyDigest)
	if len(h.sentTo(testUserKey)) != 1 || len(h.sentTo(testAdmin2Key)) != 1 {
		t.Fatalf("at 23:00 UTC: Tokyo admin %d, New York admin %d", len(h.sentTo(testUserKey)), len(h.sentTo(testAdmin2Key)))
	}
}

func TestDigestWeekIsTheAdminsLocalWeek(t *testing.T) {
	a := defaultAdminSettings()
	a.TimeZone = "Pacific/Auckland"
	// Sunday 23:00 UTC is already Monday in Auckland: the next ISO week.
	now := time.Date(2026, 11, 8, 23, 0, 0, 0, time.UTC)
	if isoWeek(now.In(a.location())) == isoWeek(now) {
		t.Fatal("the local ISO week should differ from UTC's here")
	}
}

func TestDigestScheduleSettings(t *testing.T) {
	h := newHarness(t)
	h.configure(t, nil)
	_, out := h.request(t, http.MethodGet, "/api/admin/alerts", asPrimary, nil)
	s := out["settings"].(map[string]any)
	if d := s["digest"].(map[string]any); d["day"] != float64(0) || d["hour"] != float64(18) {
		t.Fatalf("default digest schedule = %v", d)
	}
	if q := s["quiet"].(map[string]any); q["enabled"] != false || q["start"] != "22:00" || q["end"] != "07:00" || q["silence_emergency"] != false {
		t.Fatalf("default quiet hours = %v", q)
	}

	status, out := h.request(t, http.MethodPut, "/api/admin/alerts", asPrimary, map[string]any{
		"digest":    map[string]any{"day": 5, "hour": 7},
		"time_zone": "Europe/London",
		"quiet":     map[string]any{"enabled": true, "start": "23:30", "end": "06:15", "silence_emergency": true},
	})
	if status != http.StatusOK {
		t.Fatalf("save: %d %v", status, out)
	}
	got, found, err := h.plugin.admins.Get(context.Background(), 7)
	if err != nil || !found {
		t.Fatal(err)
	}
	if d := got.digestSchedule(); d.Day != 5 || d.Hour != 7 || got.TimeZone != "Europe/London" || !got.Quiet.Enabled || got.Quiet.Start != "23:30" || !got.Quiet.SilenceEmergency {
		t.Fatalf("saved %+v digest %+v", got, d)
	}

	// A save without a zone (a browser that can't say) keeps the saved one.
	h.request(t, http.MethodPut, "/api/admin/alerts", asPrimary, map[string]any{"digest": map[string]any{"day": 1, "hour": 2}})
	got, _, _ = h.plugin.admins.Get(context.Background(), 7)
	if got.TimeZone != "Europe/London" || got.digestSchedule().Day != 1 {
		t.Fatalf("after a save without a zone: %q %+v", got.TimeZone, got.digestSchedule())
	}

	for name, bad := range map[string]map[string]any{
		"day":       {"digest": map[string]any{"day": 7, "hour": 1}},
		"hour":      {"digest": map[string]any{"day": 1, "hour": 24}},
		"negative":  {"digest": map[string]any{"day": -1, "hour": 1}},
		"zone":      {"time_zone": "Not/AZone"},
		"local":     {"time_zone": "Local"},
		"quiet":     {"quiet": map[string]any{"enabled": true, "start": "25:00", "end": "06:00"}},
		"same time": {"quiet": map[string]any{"enabled": true, "start": "06:00", "end": "06:00"}},
	} {
		if status, out := h.request(t, http.MethodPut, "/api/admin/alerts", asPrimary, bad); status != http.StatusUnprocessableEntity {
			t.Fatalf("bad %s: %d %v", name, status, out)
		}
	}
}

func TestSetupTextSaysHourlyDigest(t *testing.T) {
	for name, text := range map[string]string{"manifest": string(manifestJSON), "page": string(indexHTML)} {
		if !strings.Contains(text, "3600000") {
			t.Errorf("the %s doesn't give the 1 hour interval for the digest", name)
		}
		if strings.Contains(text, `"day_of_week"`) || strings.Contains(text, `\"day_of_week\"`) {
			t.Errorf("the %s still tells admins to bind the digest with a weekly trigger", name)
		}
	}
}
