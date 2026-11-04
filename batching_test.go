package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// Tests for the per-profile settings of 0.3.0: the reason filter, season-drop
// grouping and quiet hours.

// hookState is a memoryState whose writes and reads can be intercepted, to
// stand in for another API server acting between this one's steps.
type hookState struct {
	memoryState
	mu2    sync.Mutex
	writes map[string]int
	// beforeWrite runs before each write with the key and how many writes the
	// key has had; a non-nil error fails the write.
	beforeWrite func(key string, n int) error
	// afterWrite runs after a write has landed.
	afterWrite func(key string, n int)
	// unavailable makes every call fail like a host that isn't connected yet.
	unavailable atomic.Bool
}

func newHookState() *hookState {
	return &hookState{memoryState: memoryState{values: map[string][]byte{}}, writes: map[string]int{}}
}

func (s *hookState) Read(ctx context.Context, key string) ([]byte, bool, error) {
	if s.unavailable.Load() {
		return nil, false, errHostUnavailable
	}
	return s.memoryState.Read(ctx, key)
}

func (s *hookState) Write(ctx context.Context, key string, value []byte) error {
	if s.unavailable.Load() {
		return errHostUnavailable
	}
	s.mu2.Lock()
	s.writes[key]++
	n := s.writes[key]
	hook := s.beforeWrite
	s.mu2.Unlock()
	after := s.afterWrite
	if hook != nil {
		if err := hook(key, n); err != nil {
			return err
		}
	}
	err := s.memoryState.Write(ctx, key, value)
	if after != nil {
		after(key, n)
	}
	return err
}

// groupEvt is a new-episode event for profile p1 and show s1.
func groupEvt(eventID, deliveryID string, season, episode int, extra map[string]any) *pluginv1.HandleEventRequest {
	fields := map[string]any{
		"series_id": "s1", "episode_id": "ep-" + deliveryID, "episode_title": fmt.Sprintf("Episode %d", episode),
		"season_number": season, "episode_number": episode, "reason_flags": map[string]any{"watchlist": true},
		"created_at": "2026-11-03T19:58:00Z",
	}
	maps.Copy(fields, extra)
	return episodeEvent(eventID, deliveryID, fields)
}

// groupPlugin is a plugin process on state, with profile p1 grouping episodes
// for five minutes.
func (h *harness) groupPlugin(t *testing.T, state stateBackend, edit func(*ProfileSettings)) (*Plugin, *eventServer) {
	t.Helper()
	p := h.newPlugin(state)
	s := &ProfileSettings{ProfileID: "p1", ProfileName: "Josh", Enabled: true, UserKey: testUserKey, GroupMinutes: 5}
	if edit != nil {
		edit(s)
	}
	if err := p.store.Put(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	return p, &eventServer{plugin: p}
}

func clockAt(p *Plugin, d time.Duration) { p.taskNow = func() time.Time { return testNow.Add(d) } }

func mustHandle(t *testing.T, e *eventServer, reqs ...*pluginv1.HandleEventRequest) {
	t.Helper()
	for _, r := range reqs {
		if _, err := e.HandleEvent(context.Background(), r); err != nil {
			t.Fatalf("HandleEvent(%s): %v", r.GetEventId(), err)
		}
	}
}

func readPending(t *testing.T, state stateBackend) *pendingState {
	t.Helper()
	raw, found, err := state.Read(context.Background(), pendingKey)
	if err != nil {
		t.Fatal(err)
	}
	st := &pendingState{}
	if found {
		if err := json.Unmarshal(raw, st); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func flush(t *testing.T, p *Plugin) int {
	t.Helper()
	n, err := p.flushPending(context.Background())
	if err != nil {
		t.Fatalf("flushPending: %v", err)
	}
	return n
}

// --- Settings ---

func TestOldSettingsLoadUnchanged(t *testing.T) {
	h := newHarness(t)
	// A profile bucket as 0.2.0 saved it: none of the 0.3.0 fields.
	old := `{"p1":{"profile_id":"p1","profile_name":"Josh","user_id":7,"enabled":true,"user_key":"` + testUserKey + `","priority":0,"html":false,"attach_poster":false,"include_link":false,"types":{"episode.available":{"enabled":true,"title":"New: {series}","message":"{sxe}"}},"updated_at":"2026-09-01T00:00:00Z"}}`
	h.state.values[bucketKey("p1")] = []byte(old)

	s, found, err := h.plugin.store.Get(context.Background(), "p1")
	if err != nil || !found {
		t.Fatalf("Get: %v %v", found, err)
	}
	if len(s.EpisodeReasons) != 0 || s.GroupMinutes != 0 || s.Quiet.Enabled || s.TimeZone != "" || s.groupWindow() != 0 {
		t.Fatalf("new fields should default to off: %+v", s)
	}
	// The profile's episodes are still sent one at a time, with its own wording.
	mustHandle(t, h.events, groupEvt("ev1", "d1", 2, 1, nil))
	sent := h.pushover.messages()
	if len(sent) != 1 || sent[0].Get("title") != "New: Severance" || sent[0].Get("message") != "S02E01" {
		t.Fatalf("sent %v", sent)
	}
	if _, ok := h.state.values[pendingKey]; ok {
		t.Fatal("a profile that doesn't group wrote to pending")
	}

	// Saving unchanged settings through the API stores nothing new.
	_, out := h.request(t, http.MethodGet, "/api/profiles/p1", asPrimary, nil)
	if out["group_minutes"] != float64(0) || len(out["episode_reasons"].([]any)) != 0 || out["quiet"].(map[string]any)["enabled"] != false {
		t.Fatalf("view of old settings: %v", out)
	}
	if status, out := h.request(t, http.MethodPut, "/api/profiles/p1", asPrimary, map[string]any{
		"enabled": true, "types": map[string]any{"episode.available": map[string]any{"enabled": true, "title": "New: {series}", "message": "{sxe}"}},
	}); status != http.StatusOK {
		t.Fatalf("save: %d %v", status, out)
	}
	raw := string(h.state.values[bucketKey("p1")])
	for _, field := range []string{"episode_reasons", "group_minutes", "time_zone", "quiet", "group_title", "group_message"} {
		if strings.Contains(raw, field) {
			t.Errorf("saving without the new settings stored %q: %s", field, raw)
		}
	}
}

func TestProfileSettingsAPI(t *testing.T) {
	h := newHarness(t)
	put := func(body map[string]any) (int, map[string]any) {
		body["enabled"], body["user_key"] = true, testUserKey
		return h.request(t, http.MethodPut, "/api/profiles/p1", asPrimary, body)
	}
	status, out := put(map[string]any{
		"episode_reasons": []string{"next_up", "favorite", "favorite"}, "group_minutes": 10, "time_zone": "Europe/London",
		"quiet": map[string]any{"enabled": true, "start": "23:30", "end": "06:15"},
	})
	if status != http.StatusOK {
		t.Fatalf("save: %d %v", status, out)
	}
	s, _, _ := h.plugin.store.Get(context.Background(), "p1")
	if strings.Join(s.EpisodeReasons, ",") != "favorite,next_up" || s.GroupMinutes != 10 || s.TimeZone != "Europe/London" ||
		!s.Quiet.Enabled || s.Quiet.Start != "23:30" || s.Quiet.End != "06:15" {
		t.Fatalf("saved %+v", s)
	}
	view := out["profile"].(map[string]any)
	if view["group_minutes"] != float64(10) || view["time_zone"] != "Europe/London" || len(view["episode_reason_options"].([]any)) != 4 {
		t.Fatalf("view %v", view)
	}

	// A browser that can't report its zone keeps the saved one.
	if status, out := put(map[string]any{"group_minutes": 3, "quiet": map[string]any{"enabled": true, "start": "23:30", "end": "06:15"}}); status != http.StatusOK {
		t.Fatalf("save without zone: %d %v", status, out)
	}
	if s, _, _ := h.plugin.store.Get(context.Background(), "p1"); s.TimeZone != "Europe/London" || s.GroupMinutes != 3 || len(s.EpisodeReasons) != 0 {
		t.Fatalf("after a save without a zone: %+v", s)
	}

	for name, bad := range map[string]map[string]any{
		"reason":        {"episode_reasons": []string{"popular"}},
		"minutes low":   {"group_minutes": -1},
		"minutes high":  {"group_minutes": 121},
		"zone":          {"time_zone": "Not/AZone"},
		"local":         {"time_zone": "Local"},
		"quiet times":   {"quiet": map[string]any{"enabled": true, "start": "25:00", "end": "06:00"}},
		"quiet same":    {"quiet": map[string]any{"enabled": true, "start": "06:00", "end": "06:00"}},
		"group wording": {"types": map[string]any{"episode.available": map[string]any{"enabled": true, "group_title": strings.Repeat("x", 1025)}}},
	} {
		if status, out := put(bad); status != http.StatusUnprocessableEntity {
			t.Errorf("bad %s: %d %v", name, status, out)
		}
	}
}

func TestGroupWordingDefaultsStayUnset(t *testing.T) {
	h := newHarness(t)
	_, out := h.request(t, http.MethodGet, "/api/profiles/p1", asPrimary, nil)
	ep := out["types"].(map[string]any)["episode.available"].(map[string]any)
	if ep["group_title"] != "{series}: {count} new episodes ({range})" || ep["group_message"] != "{episodes}" {
		t.Fatalf("the page needs the default grouped wording: %v", ep)
	}
	// Saving the default wording back stores nothing, so a later change to the
	// default reaches the profile.
	h.request(t, http.MethodPut, "/api/profiles/p1", asPrimary, map[string]any{"enabled": true, "user_key": testUserKey, "types": out["types"]})
	if raw := string(h.state.values[bucketKey("p1")]); strings.Contains(raw, "group_title") {
		t.Fatalf("default wording was stored: %s", raw)
	}
	// Changed wording is kept, and only on the type that can be merged.
	types := out["types"].(map[string]any)
	ep["group_title"] = "{count} from {series}"
	types["request.approved"].(map[string]any)["group_title"] = "ignored"
	h.request(t, http.MethodPut, "/api/profiles/p1", asPrimary, map[string]any{"enabled": true, "user_key": testUserKey, "types": types})
	s, _, _ := h.plugin.store.Get(context.Background(), "p1")
	if s.Types["episode.available"].GroupTitle != "{count} from {series}" || s.Types["request.approved"].GroupTitle != "" {
		t.Fatalf("stored wording: %+v", s.Types)
	}
}

// --- Reason filter ---

func TestReasonFilter(t *testing.T) {
	h := newHarness(t)
	h.configure(t, func(s *ProfileSettings) { s.EpisodeReasons = []string{reasonFavorite, reasonNextUp} })
	flags := func(f map[string]any) map[string]any { return map[string]any{"reason_flags": f} }
	mustHandle(t, h.events,
		groupEvt("e1", "d1", 1, 1, flags(map[string]any{"watchlist": true})),                      // not asked for
		groupEvt("e2", "d2", 1, 2, flags(map[string]any{"watchlist": true, "next_up": true})),     // next up: sent
		groupEvt("e3", "d3", 1, 3, flags(map[string]any{"favorite": true})),                       // favorite: sent
		groupEvt("e4", "d4", 1, 4, flags(map[string]any{})),                                       // no reason at all
		groupEvt("e5", "d5", 1, 5, flags(map[string]any{"continue_watching": true})),              // not asked for
		episodeEvent("e6", "d6", map[string]any{"type": "request.approved", "reason_flags": nil}), // other types aren't filtered
	)
	got := h.pushover.messages()
	if len(got) != 3 {
		t.Fatalf("sent %d messages, want 3: %v", len(got), titles(got))
	}
	if got[0].Get("message") != "S01E02 · Episode 2" || got[1].Get("message") != "S01E03 · Episode 3" || got[2].Get("title") != "Request approved" {
		t.Fatalf("sent %v", got)
	}
	// Filtered episodes are ignored, not recorded as handled.
	if seen, _ := h.plugin.handled.Seen(context.Background(), "e1"); seen {
		t.Fatal("a filtered episode was recorded")
	}

	// No filter: every episode, even one with no reasons.
	h2 := newHarness(t)
	h2.configure(t, nil)
	mustHandle(t, h2.events, groupEvt("e1", "d1", 1, 1, map[string]any{"reason_flags": map[string]any{}}))
	if n := len(h2.pushover.messages()); n != 1 {
		t.Fatalf("with no filter sent %d", n)
	}
}

func TestReasonFilterAppliesBeforeGrouping(t *testing.T) {
	h := newHarness(t)
	_, e := h.groupPlugin(t, h.state, func(s *ProfileSettings) { s.EpisodeReasons = []string{reasonFavorite} })
	mustHandle(t, e,
		groupEvt("e1", "d1", 1, 1, map[string]any{"reason_flags": map[string]any{"watchlist": true}}),
		groupEvt("e2", "d2", 1, 2, map[string]any{"reason_flags": map[string]any{"favorite": true}}))
	st := readPending(t, h.state)
	if len(st.Groups) != 1 || len(st.Groups[0].Episodes) != 1 || st.Groups[0].Episodes[0].DeliveryID != "d2" {
		t.Fatalf("pending %+v", st.Groups)
	}
}

// --- Grouping ---

func TestSeasonDropIsOneNotification(t *testing.T) {
	h := newHarness(t)
	p, e := h.groupPlugin(t, h.state, func(s *ProfileSettings) { s.IncludeLink = true })
	// Delivered out of order and with a repeat of the second.
	mustHandle(t, e,
		groupEvt("ev2", "d2", 2, 2, nil),
		groupEvt("ev1", "d1", 2, 1, nil),
		groupEvt("ev2", "d2", 2, 2, nil),
		groupEvt("ev3", "d3", 2, 3, map[string]any{"reason_flags": map[string]any{"next_up": true}}),
	)
	if n := len(h.pushover.messages()); n != 0 {
		t.Fatalf("the event handler sent %d messages; it should only write to pending", n)
	}
	st := readPending(t, h.state)
	if len(st.Groups) != 1 || len(st.Groups[0].Episodes) != 3 {
		t.Fatalf("pending %+v", st.Groups)
	}
	g := st.Groups[0]
	if !g.DueAt.Equal(testNow.Add(5*time.Minute)) || g.ID != "group:p1|id:s1|d2" {
		t.Fatalf("group %+v", g)
	}

	// Not due yet: nothing goes out.
	clockAt(p, 4*time.Minute+59*time.Second)
	if flush(t, p) != 0 || len(h.pushover.messages()) != 0 {
		t.Fatal("a group went out before its window ended")
	}

	clockAt(p, 5*time.Minute)
	if flush(t, p) != 1 {
		t.Fatal("the due group wasn't sent")
	}
	sent := h.pushover.messages()
	if len(sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(sent))
	}
	m := sent[0]
	if m.Get("title") != "Severance: 3 new episodes (S02E01 to S02E03)" {
		t.Errorf("title = %q", m.Get("title"))
	}
	if want := "S02E01 · Episode 1\nS02E02 · Episode 2\nS02E03 · Episode 3"; m.Get("message") != want {
		t.Errorf("message = %q, want %q", m.Get("message"), want)
	}
	if m.Get("url") != "https://silo.example.com/item/s1" {
		t.Errorf("a merged push links to the show, got %q", m.Get("url"))
	}
	if left := readPending(t, h.state); len(left.Groups) != 0 {
		t.Fatalf("pending after the flush: %+v", left.Groups)
	}
	ctx := context.Background()
	if seen, _ := p.handled.Seen(ctx, g.ID); !seen {
		t.Fatal("the group's ID wasn't recorded")
	}

	// A late retry of one of the events isn't held or sent again, and nothing
	// else is sent by later passes.
	mustHandle(t, e, groupEvt("ev1", "d1", 2, 1, nil))
	if left := readPending(t, h.state); len(left.Groups) != 0 {
		t.Fatalf("a retried event was held again: %+v", left.Groups)
	}
	clockAt(p, time.Hour)
	flush(t, p)
	if n := len(h.pushover.messages()); n != 1 {
		t.Fatalf("sent %d messages in all, want 1", n)
	}
}

func TestSingleHeldEpisodeIsSentAsOrdinary(t *testing.T) {
	h := newHarness(t)
	p, e := h.groupPlugin(t, h.state, nil)
	mustHandle(t, e, groupEvt("ev1", "d1", 2, 1, nil))
	clockAt(p, 5*time.Minute)
	flush(t, p)
	sent := h.pushover.messages()
	if len(sent) != 1 || sent[0].Get("title") != "New episode: Severance" || sent[0].Get("message") != "S02E01 · Episode 1" {
		t.Fatalf("sent %v", sent)
	}
}

func TestGroupsAreSeparatePerShowAndProfile(t *testing.T) {
	h := newHarness(t)
	p, e := h.groupPlugin(t, h.state, nil)
	if err := p.store.Put(context.Background(), &ProfileSettings{ProfileID: "p2", ProfileName: "Kid", Enabled: true, UserKey: "kidkeykidkeykidkeykidkeykidkey", GroupMinutes: 5}); err != nil {
		t.Fatal(err)
	}
	other := map[string]any{"series_id": "s2", "series_title": "The Bear"}
	mustHandle(t, e,
		groupEvt("a1", "a1", 1, 1, nil), groupEvt("a2", "a2", 1, 2, nil),
		groupEvt("b1", "b1", 3, 1, other), groupEvt("b2", "b2", 3, 2, other),
		groupEvt("c1", "c1", 1, 1, map[string]any{"profile_id": "p2"}), groupEvt("c2", "c2", 1, 2, map[string]any{"profile_id": "p2"}),
	)
	if st := readPending(t, h.state); len(st.Groups) != 3 {
		t.Fatalf("pending %d groups, want 3", len(st.Groups))
	}
	clockAt(p, 5*time.Minute)
	if got := flush(t, p); got != 3 {
		t.Fatalf("flushed %d", got)
	}
	got := titles(h.pushover.messages())
	if len(got) != 3 {
		t.Fatalf("titles %v", got)
	}
	want := map[string]bool{"Severance: 2 new episodes (S01E01 to S01E02)": true, "The Bear: 2 new episodes (S03E01 to S03E02)": true}
	n := 0
	for _, title := range got {
		if want[title] {
			n++
		}
	}
	if n != 3 { // Severance appears for both profiles
		t.Fatalf("titles %v", got)
	}
	if len(h.sentTo("kidkeykidkeykidkeykidkeykidkey")) != 1 {
		t.Fatal("the kid's group wasn't sent to the kid")
	}
}

func TestGroupWindowIsPerProfile(t *testing.T) {
	h := newHarness(t)
	p, e := h.groupPlugin(t, h.state, func(s *ProfileSettings) { s.GroupMinutes = 20 })
	mustHandle(t, e, groupEvt("a1", "a1", 1, 1, nil))
	if st := readPending(t, h.state); !st.Groups[0].DueAt.Equal(testNow.Add(20 * time.Minute)) {
		t.Fatalf("due %v", st.Groups[0].DueAt)
	}
	clockAt(p, 19*time.Minute)
	if flush(t, p) != 0 {
		t.Fatal("flushed early")
	}
}

func TestFullGroupStartsANewOne(t *testing.T) {
	h := newHarness(t)
	_, e := h.groupPlugin(t, h.state, nil)
	for i := range maxGroupEpisodes + 1 {
		mustHandle(t, e, groupEvt(fmt.Sprintf("e%d", i), fmt.Sprintf("d%d", i), 1, i+1, nil))
	}
	st := readPending(t, h.state)
	if len(st.Groups) != 2 || len(st.Groups[0].Episodes) != maxGroupEpisodes || len(st.Groups[1].Episodes) != 1 {
		t.Fatalf("groups %d", len(st.Groups))
	}
}

func TestGroupMessageRangesAndLists(t *testing.T) {
	ptr := func(v int) *int { return &v }
	g := &pendingGroup{Series: "Severance"}
	for i := 1; i <= 12; i++ {
		g.Episodes = append(g.Episodes, pendingEpisode{DeliveryID: fmt.Sprint(i), Season: ptr(1), Episode: ptr(i), Title: fmt.Sprintf("T%d", i), Reasons: []string{reasonNextUp}})
	}
	g.Episodes = append(g.Episodes, pendingEpisode{DeliveryID: "x", Season: ptr(2), Episode: ptr(1), Reasons: []string{reasonFavorite}})
	v := groupVars(g, sortedEpisodes(g), "Josh")
	if v["count"] != "13" || v["range"] != "S01E01 to S02E01" || v["first"] != "S01E01" || v["last"] != "S02E01" || v["reasons"] != "Favorite, Next Up" {
		t.Fatalf("vars %v", v)
	}
	lines := strings.Split(v["episodes"], "\n")
	if len(lines) != maxListedEpisodes+1 || lines[0] != "S01E01 · T1" || lines[len(lines)-1] != "…and 5 more" {
		t.Fatalf("episodes %q", lines)
	}

	// Without numbers there is no range, and an untitled episode still gets a line.
	g = &pendingGroup{Series: "News", Episodes: []pendingEpisode{{DeliveryID: "a", Title: "Monday"}, {DeliveryID: "b"}}}
	v = groupVars(g, sortedEpisodes(g), "Josh")
	if v["range"] != "" || v["first"] != "" || v["episodes"] != "New episode\nMonday" && v["episodes"] != "Monday\nNew episode" {
		t.Fatalf("vars %v", v)
	}
	// One shared number is not a range.
	one := &pendingGroup{Series: "S", Episodes: []pendingEpisode{{DeliveryID: "a", Season: ptr(1), Episode: ptr(4)}, {DeliveryID: "b", Season: ptr(1), Episode: ptr(4)}}}
	if v := groupVars(one, sortedEpisodes(one), "J"); v["range"] != "S01E04" {
		t.Fatalf("range %q", v["range"])
	}
}

func TestCustomGroupWording(t *testing.T) {
	h := newHarness(t)
	p, e := h.groupPlugin(t, h.state, func(s *ProfileSettings) {
		s.HTML = true
		ts := notificationTypeByKey(episodeAvailableKey).defaults()
		ts.GroupTitle, ts.GroupMessage = "<b>{count}</b> x {series}", "{first} → {last} ({reasons})"
		s.Types = map[string]TypeSettings{episodeAvailableKey: ts}
	})
	mustHandle(t, e, groupEvt("a", "a", 1, 1, nil), groupEvt("b", "b", 1, 5, nil))
	clockAt(p, 5*time.Minute)
	flush(t, p)
	m := h.pushover.messages()[0]
	if m.Get("title") != "<b>2</b> x Severance" || m.Get("message") != "S01E01 → S01E05 (Watchlist)" {
		t.Fatalf("sent %v", m)
	}
}

func TestGroupedNotificationAttachesThePoster(t *testing.T) {
	h := newHarness(t)
	p, e := h.groupPlugin(t, h.state, func(s *ProfileSettings) { s.AttachPoster = true })
	mustHandle(t, e,
		groupEvt("a", "a", 1, 1, map[string]any{"poster_url": "/poster.jpg"}),
		groupEvt("b", "b", 1, 2, map[string]any{"poster_url": "/other.jpg"}))
	clockAt(p, 5*time.Minute)
	flush(t, p)
	if m := h.pushover.messages(); len(m) != 1 || m[0].Get("_attachment") != "yes" {
		t.Fatalf("sent %v; the first episode's poster should be attached", m)
	}
}

func TestGroupedTestSend(t *testing.T) {
	h := newHarness(t)
	h.configure(t, nil)
	status, out := h.request(t, http.MethodPost, "/api/profiles/p1/test", asPrimary, map[string]any{
		"settings": map[string]any{"enabled": true, "group_minutes": 5}, "type": "episode.available", "group": true,
	})
	if status != http.StatusOK {
		t.Fatalf("test: %d %v", status, out)
	}
	m := h.pushover.messages()[0]
	if m.Get("title") != "[Test] Severance: 3 new episodes (S02E01 to S02E03)" || !strings.Contains(m.Get("message"), "S02E02 · Goodbye, Mrs. Selvig") {
		t.Fatalf("sent %v", m)
	}
}

func TestEventHandlerOnlyWritesToPending(t *testing.T) {
	h := newHarness(t)
	_, e := h.groupPlugin(t, h.state, nil)
	// Pushover never answers; the handler must not wait for it.
	h.pushover.hold = make(chan struct{})
	defer close(h.pushover.hold)
	start := time.Now()
	mustHandle(t, e, groupEvt("a", "a", 1, 1, nil))
	if time.Since(start) > time.Second {
		t.Fatal("the event handler waited for Pushover")
	}
	if len(readPending(t, h.state).Groups) != 1 {
		t.Fatal("the episode wasn't saved before the event was acknowledged")
	}
}

func TestHoldFailureAsksSiloToRetry(t *testing.T) {
	h := newHarness(t)
	hs := newHookState()
	_, e := h.groupPlugin(t, hs, nil)
	hs.beforeWrite = func(key string, n int) error {
		if key == pendingKey {
			return errors.New("state write failed")
		}
		return nil
	}
	if _, err := e.HandleEvent(context.Background(), groupEvt("a", "a", 1, 1, nil)); err == nil {
		t.Fatal("an episode that couldn't be saved was acknowledged")
	}
	// Silo's retry works once state does.
	hs.beforeWrite = nil
	mustHandle(t, e, groupEvt("a", "a", 1, 1, nil))
	if len(readPending(t, hs).Groups) != 1 {
		t.Fatal("the retried episode wasn't held")
	}
}

// A hold whose write is replaced by another server's older copy is redone, so
// the episode isn't lost although its event was acknowledged.
func TestHoldDetectsAnotherServersWrite(t *testing.T) {
	h := newHarness(t)
	hs := newHookState()
	_, e := h.groupPlugin(t, hs, nil)
	mustHandle(t, e, groupEvt("a", "a", 1, 1, nil))
	stale := append([]byte(nil), hs.values[pendingKey]...) // what the other server read

	hs.afterWrite = func(key string, n int) {
		if key != pendingKey || n != 2 {
			return
		}
		// The other server's write, built before this one, lands right after it.
		hs.values[pendingKey] = stale
	}
	mustHandle(t, e, groupEvt("b", "b", 1, 2, nil))
	st := readPending(t, hs)
	if len(st.Groups) != 1 || len(st.Groups[0].Episodes) != 2 {
		t.Fatalf("pending after a lost update: %+v", st.Groups)
	}
	if hs.writes[pendingKey] != 3 {
		t.Fatalf("pending was written %d times, want 3 (hold, replaced, hold again)", hs.writes[pendingKey])
	}

	// A write that keeps being replaced gives up and asks Silo to retry.
	hs.afterWrite = func(key string, n int) {
		if key == pendingKey {
			hs.values[pendingKey] = stale
		}
	}
	if _, err := e.HandleEvent(context.Background(), groupEvt("c", "c", 1, 3, nil)); err == nil {
		t.Fatal("an episode that never stuck was acknowledged")
	}
}

// --- Restarts and several servers ---

func TestPendingGroupSurvivesARestart(t *testing.T) {
	h := newHarness(t)
	_, e := h.groupPlugin(t, h.state, nil)
	mustHandle(t, e, groupEvt("a", "a", 1, 1, nil), groupEvt("b", "b", 1, 2, nil))

	// The process is gone; a new one (another API server, or the same one
	// restarted) flushes at startup once the window is over.
	second := h.newPlugin(h.state)
	clockAt(second, 6*time.Minute)
	second.flushTick(context.Background())
	if n := len(h.pushover.messages()); n != 1 {
		t.Fatalf("the restarted process sent %d messages, want 1", n)
	}
	// And a third that starts afterwards finds nothing to send.
	third := h.newPlugin(h.state)
	clockAt(third, 7*time.Minute)
	third.flushTick(context.Background())
	if n := len(h.pushover.messages()); n != 1 {
		t.Fatalf("a later process sent again: %d messages", n)
	}
}

func TestFlushAtStartupWithoutHostWaits(t *testing.T) {
	h := newHarness(t)
	hs := newHookState()
	p, e := h.groupPlugin(t, hs, nil)
	mustHandle(t, e, groupEvt("a", "a", 1, 1, nil), groupEvt("b", "b", 1, 2, nil))
	clockAt(p, 6*time.Minute)

	hs.unavailable.Store(true)
	if _, err := p.flushPending(context.Background()); !errors.Is(err, errHostUnavailable) {
		t.Fatalf("flush without a host: %v", err)
	}
	p.flushTick(context.Background()) // logs nothing, panics at nothing
	if len(h.pushover.messages()) != 0 {
		t.Fatal("sent without a host")
	}
	hs.unavailable.Store(false)
	p.flushTick(context.Background())
	if len(h.pushover.messages()) != 1 {
		t.Fatal("the next tick didn't send the group")
	}
}

func TestFlusherLoopSendsAtStartupAndOnTicks(t *testing.T) {
	h := newHarness(t)
	p, e := h.groupPlugin(t, h.state, nil)
	mustHandle(t, e, groupEvt("a", "a", 1, 1, nil), groupEvt("b", "b", 1, 2, nil))
	clockAt(p, 6*time.Minute) // already due when the process starts
	p.flushEvery = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.runFlusher(ctx); close(done) }()
	deadline := time.After(5 * time.Second)
	for len(h.pushover.messages()) == 0 {
		select {
		case <-deadline:
			t.Fatal("the flusher didn't send the due group")
		case <-time.After(5 * time.Millisecond):
		}
	}
	// A group that comes due later goes out on a tick.
	mustHandle(t, e, groupEvt("c", "c", 2, 1, map[string]any{"series_id": "s9", "series_title": "Other"}), groupEvt("d", "d", 2, 2, map[string]any{"series_id": "s9", "series_title": "Other"}))
	clockAt(p, 20*time.Minute)
	for len(h.pushover.messages()) < 2 {
		select {
		case <-deadline:
			t.Fatal("the flusher didn't send the second group on a tick")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
	time.Sleep(30 * time.Millisecond)
	if n := len(h.pushover.messages()); n != 2 {
		t.Fatalf("sent %d messages, want 2", n)
	}
}

func TestRetryableFlushFailureStaysPending(t *testing.T) {
	h := newHarness(t)
	p, e := h.groupPlugin(t, h.state, nil)
	mustHandle(t, e, groupEvt("a", "a", 1, 1, nil), groupEvt("b", "b", 1, 2, nil))
	clockAt(p, 5*time.Minute)

	h.pushover.fail.Store(http.StatusServiceUnavailable)
	if _, err := p.flushPending(context.Background()); err == nil {
		t.Fatal("a Pushover outage should be reported by the flush")
	}
	st := readPending(t, h.state)
	if len(st.Groups) != 1 || st.Groups[0].Lease != "" {
		t.Fatalf("a failed flush should leave the group pending and unclaimed: %+v", st.Groups)
	}
	if seen, _ := p.handled.Seen(context.Background(), st.Groups[0].ID); seen {
		t.Fatal("a failed send was recorded as handled")
	}

	// The next tick sends it, once.
	h.pushover.fail.Store(0)
	flush(t, p)
	flush(t, p)
	if n := len(h.pushover.messages()); n != 1 {
		t.Fatalf("sent %d messages, want 1", n)
	}
	if len(readPending(t, h.state).Groups) != 0 {
		t.Fatal("the sent group is still pending")
	}
}

func TestPermanentFlushFailureIsNotRetried(t *testing.T) {
	h := newHarness(t)
	p, e := h.groupPlugin(t, h.state, nil)
	mustHandle(t, e, groupEvt("a", "a", 1, 1, nil), groupEvt("b", "b", 1, 2, nil))
	clockAt(p, 5*time.Minute)
	h.pushover.fail.Store(http.StatusBadRequest)
	if flush(t, p) != 1 {
		t.Fatal("a rejected group should be finished")
	}
	h.pushover.fail.Store(0)
	flush(t, p)
	if len(h.pushover.messages()) != 0 || len(readPending(t, h.state).Groups) != 0 {
		t.Fatal("a rejected group was retried or left pending")
	}
}

// A crash (or failed write) after the send but before the group leaves pending
// must not send it again.
func TestGroupSentOnceWhenRemovalFails(t *testing.T) {
	h := newHarness(t)
	hs := newHookState()
	p, e := h.groupPlugin(t, hs, nil)
	mustHandle(t, e, groupEvt("a", "a", 1, 1, nil), groupEvt("b", "b", 1, 2, nil))
	clockAt(p, 5*time.Minute)

	// Writes to pending so far: two holds. The flush's claim is the third and
	// the removal the fourth, which fails.
	hs.beforeWrite = func(key string, n int) error {
		if key == pendingKey && n == 4 {
			return errors.New("lost the connection")
		}
		return nil
	}
	if flush(t, p) != 0 {
		t.Fatal("a group whose removal failed isn't finished")
	}
	if len(h.pushover.messages()) != 1 || len(readPending(t, hs).Groups) != 1 {
		t.Fatalf("after the failed removal: %d sent, %d pending", len(h.pushover.messages()), len(readPending(t, hs).Groups))
	}

	// Another process (this one has the ID in memory too) takes over.
	hs.beforeWrite = nil
	other := h.newPlugin(hs)
	clockAt(other, 5*time.Minute+leaseExpired)
	if flush(t, other) != 1 {
		t.Fatal("the leftover group wasn't cleared")
	}
	if n := len(h.pushover.messages()); n != 1 {
		t.Fatalf("the group was sent %d times", n)
	}
	if len(readPending(t, hs).Groups) != 0 {
		t.Fatal("pending still has the group")
	}
}

// leaseExpired is past a claim's lease, in tests that take over a dead
// process's group.
const leaseExpired = leaseTTL + time.Second

func TestFlushLeaseKeepsServersApart(t *testing.T) {
	h := newHarness(t)
	hs := newHookState()
	p, e := h.groupPlugin(t, hs, nil)
	other := h.newPlugin(hs)
	mustHandle(t, e, groupEvt("a", "a", 1, 1, nil), groupEvt("b", "b", 1, 2, nil))
	clockAt(p, 6*time.Minute)
	clockAt(other, 6*time.Minute)

	// The other server claimed the group a moment ago and is sending it.
	err := other.pending.update(context.Background(), func(st *pendingState) bool {
		g := st.Groups[0]
		g.Lease, g.LeaseUntil = other.pending.id, other.now().Add(30*time.Second)
		return true
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if flush(t, p) != 0 || len(h.pushover.messages()) != 0 {
		t.Fatal("a group claimed by another server was sent")
	}

	// Its lease runs out (that server died): this one takes the group over.
	clockAt(p, 6*time.Minute+leaseExpired)
	if flush(t, p) != 1 || len(h.pushover.messages()) != 1 {
		t.Fatal("an abandoned group wasn't taken over")
	}
}

// Two servers whose timers fire together both read the group unclaimed and
// both write a lease. Only the one whose write landed last keeps it, so the
// group is sent once.
func TestTwoServersFlushingTogetherSendOnce(t *testing.T) {
	for range 5 {
		h := newHarness(t)
		hs := newHookState()
		p, e := h.groupPlugin(t, hs, nil)
		other := h.newPlugin(hs)
		mustHandle(t, e, groupEvt("a", "a", 1, 1, nil), groupEvt("b", "b", 1, 2, nil))
		for _, x := range []*Plugin{p, other} {
			clockAt(x, 6*time.Minute)
			x.pending.settle = 100 * time.Millisecond
		}

		// Hold both claim writes until both have read.
		var mu sync.Mutex
		arrivals := 0
		gate := make(chan struct{})
		hs.beforeWrite = func(key string, n int) error {
			if key != pendingKey {
				return nil
			}
			mu.Lock()
			arrivals++
			a := arrivals
			if a == 2 {
				close(gate)
			}
			mu.Unlock()
			if a <= 2 {
				select {
				case <-gate:
				case <-time.After(2 * time.Second):
				}
			}
			return nil
		}
		var wg sync.WaitGroup
		for _, x := range []*Plugin{p, other} {
			wg.Go(func() { _, _ = x.flushPending(context.Background()) })
		}
		wg.Wait()
		if n := len(h.pushover.messages()); n != 1 {
			t.Fatalf("two servers flushing together sent %d messages, want 1", n)
		}
		if len(readPending(t, hs).Groups) != 0 {
			t.Fatal("the group is still pending")
		}
	}
}

func TestGroupDroppedWhenProfileTurnsItOff(t *testing.T) {
	h := newHarness(t)
	p, e := h.groupPlugin(t, h.state, nil)
	mustHandle(t, e, groupEvt("a", "a", 1, 1, nil), groupEvt("b", "b", 1, 2, nil))
	if err := p.store.Delete(context.Background(), "p1"); err != nil {
		t.Fatal(err)
	}
	clockAt(p, 5*time.Minute)
	if flush(t, p) != 1 || len(h.pushover.messages()) != 0 || len(readPending(t, h.state).Groups) != 0 {
		t.Fatal("a removed profile's group should be dropped without sending")
	}

	// Turning grouping off keeps what is already held: it is still sent.
	h2 := newHarness(t)
	p2, e2 := h2.groupPlugin(t, h2.state, nil)
	mustHandle(t, e2, groupEvt("a", "a", 1, 1, nil), groupEvt("b", "b", 1, 2, nil))
	s, _, _ := p2.store.Get(context.Background(), "p1")
	s.GroupMinutes = 0
	_ = p2.store.Put(context.Background(), s)
	clockAt(p2, 5*time.Minute)
	flush(t, p2)
	if len(h2.pushover.messages()) != 1 {
		t.Fatal("a held group wasn't sent after grouping was turned off")
	}
	mustHandle(t, e2, groupEvt("c", "c", 1, 3, nil))
	if len(h2.pushover.messages()) != 2 {
		t.Fatal("with grouping off an episode should be sent at once")
	}
}

// --- State size ---

func TestPendingIsCappedByEncodedSize(t *testing.T) {
	h := newHarness(t)
	_, e := h.groupPlugin(t, h.state, nil)
	big := strings.Repeat("é", 3000) // 6000 bytes of title
	const shows = 120
	for i := range shows {
		mustHandle(t, e, groupEvt(fmt.Sprintf("e%d", i), fmt.Sprintf("d%d", i), 1, 1, map[string]any{
			"series_id": fmt.Sprintf("s%d", i), "episode_title": big,
		}))
	}
	raw := h.state.values[pendingKey]
	if len(raw) == 0 || len(raw) > maxStateValue {
		t.Fatalf("pending is %d bytes; the host allows %d", len(raw), maxStateValue)
	}
	st := readPending(t, h.state)
	if len(st.Groups) == 0 || len(st.Groups) >= shows {
		t.Fatalf("%d groups kept of %d", len(st.Groups), shows)
	}
	// The newest groups survive; the oldest were dropped.
	last := st.Groups[len(st.Groups)-1]
	if last.ID != fmt.Sprintf("group:p1|id:s%d|d%d", shows-1, shows-1) {
		t.Fatalf("newest group = %s", last.ID)
	}
	if st.Groups[0].ID == "group:p1|id:s0|d0" {
		t.Fatal("the oldest group was kept while the newest were cut")
	}
}

func TestPendingStartsOverWhenDamaged(t *testing.T) {
	h := newHarness(t)
	_, e := h.groupPlugin(t, h.state, nil)
	h.state.values[pendingKey] = []byte("{not json")
	mustHandle(t, e, groupEvt("a", "a", 1, 1, nil))
	if len(readPending(t, h.state).Groups) != 1 {
		t.Fatal("a damaged pending key blocked new episodes")
	}
}

// --- Quiet hours ---

func TestProfileQuietHours(t *testing.T) {
	// 20:00 UTC is 15:00 in New York.
	quiet := func(start, end string) func(*ProfileSettings) {
		return func(s *ProfileSettings) {
			s.TimeZone, s.Quiet = "America/New_York", QuietHours{Enabled: true, Start: start, End: end}
			s.Priority = 0
			s.GroupMinutes = 0
		}
	}
	for _, c := range []struct {
		name       string
		edit       func(*ProfileSettings)
		wantPrio   string
		typePrio   *int
		wantAtPrio string
	}{
		{"inside", quiet("14:00", "16:00"), "-1", nil, ""},
		{"outside", quiet("22:00", "07:00"), "0", nil, ""},
		{"off", func(s *ProfileSettings) { quiet("14:00", "16:00")(s); s.Quiet.Enabled = false }, "0", nil, ""},
		{"no zone means UTC", func(s *ProfileSettings) { quiet("19:00", "21:00")(s); s.TimeZone = "" }, "-1", nil, ""},
		{"lowest stays lowest", func(s *ProfileSettings) { quiet("14:00", "16:00")(s); s.Priority = -2 }, "-2", nil, ""},
		{"high is turned down", func(s *ProfileSettings) { quiet("14:00", "16:00")(s); s.Priority = 1 }, "-1", nil, ""},
	} {
		h := newHarness(t)
		h.configure(t, c.edit)
		mustHandle(t, h.events, groupEvt("e1", "d1", 1, 1, nil))
		if got := h.pushover.messages()[0].Get("priority"); got != c.wantPrio {
			t.Errorf("%s: priority %s, want %s", c.name, got, c.wantPrio)
		}
	}

	// A per-type priority is turned down too.
	h := newHarness(t)
	h.configure(t, func(s *ProfileSettings) {
		quiet("14:00", "16:00")(s)
		prio := 1
		ts := notificationTypeByKey("request.approved").defaults()
		ts.Priority = &prio
		s.Types = map[string]TypeSettings{"request.approved": ts}
	})
	mustHandle(t, h.events, episodeEvent("e1", "d1", map[string]any{"type": "request.approved"}))
	if got := h.pushover.messages()[0].Get("priority"); got != "-1" {
		t.Errorf("per-type high priority became %s in quiet hours", got)
	}

	// The clock at send time decides, not when the episode was added: the
	// same profile sends normally an hour after quiet hours end.
	h2 := newHarness(t)
	h2.configure(t, quiet("14:00", "16:00"))
	h2.clock(testNow.Add(2 * time.Hour)) // 17:00 New York
	mustHandle(t, h2.events, groupEvt("e1", "d1", 1, 1, nil))
	if got := h2.pushover.messages()[0].Get("priority"); got != "0" {
		t.Errorf("after quiet hours priority = %s", got)
	}
}

func TestQuietHoursApplyToGroupedNotifications(t *testing.T) {
	h := newHarness(t)
	p, e := h.groupPlugin(t, h.state, func(s *ProfileSettings) {
		s.TimeZone, s.Quiet = "America/New_York", QuietHours{Enabled: true, Start: "00:00", End: "09:00"}
	})
	mustHandle(t, e, groupEvt("a", "a", 1, 1, nil), groupEvt("b", "b", 1, 2, nil))
	// 5 minutes after 20:00 UTC is 15:05 in New York: not quiet; the clock is
	// read when the group is sent.
	clockAt(p, 5*time.Minute)
	flush(t, p)
	if got := h.pushover.messages()[0].Get("priority"); got != "0" {
		t.Errorf("priority %s outside quiet hours", got)
	}

	h = newHarness(t)
	p, e = h.groupPlugin(t, h.state, func(s *ProfileSettings) {
		s.TimeZone, s.Quiet = "America/New_York", QuietHours{Enabled: true, Start: "14:00", End: "16:00"}
	})
	mustHandle(t, e, groupEvt("a", "a", 1, 1, nil), groupEvt("b", "b", 1, 2, nil))
	clockAt(p, 5*time.Minute)
	flush(t, p)
	if got := h.pushover.messages()[0].Get("priority"); got != "-1" {
		t.Errorf("grouped priority %s inside quiet hours, want -1", got)
	}
}

func TestProfileQuietHoursTestSendIgnoresThem(t *testing.T) {
	h := newHarness(t)
	h.configure(t, nil)
	status, out := h.request(t, http.MethodPost, "/api/profiles/p1/test", asPrimary, map[string]any{
		"settings": map[string]any{
			"enabled": true, "priority": 0, "time_zone": "America/New_York",
			"quiet": map[string]any{"enabled": true, "start": "14:00", "end": "16:00"},
		},
		"type": "episode.available",
	})
	if status != http.StatusOK {
		t.Fatalf("test: %d %v", status, out)
	}
	if got := h.pushover.messages()[0].Get("priority"); got != "0" {
		t.Errorf("a test send priority = %s; tests show the wording at the normal priority", got)
	}
}

// The profile's quiet hours share the admin code: same clock rules, same
// validation.
func TestProfileAndAdminQuietHoursShareRules(t *testing.T) {
	q := QuietHours{Enabled: true, Start: "22:00", End: "07:00"}
	loc := locationOf("America/New_York")
	for _, hour := range []int{22, 23, 0, 6, 7, 12} {
		local := time.Date(2026, 11, 3, hour, 30, 0, 0, loc)
		s := &ProfileSettings{Quiet: q, TimeZone: "America/New_York"}
		a := &AdminSettings{Quiet: q, TimeZone: "America/New_York"}
		if s.priorityAt(1, local) != a.priorityAt(nil, 1, local) {
			t.Errorf("hour %d: profile and admin disagree", hour)
		}
	}
	if _, aerr := cleanQuiet(QuietHours{Enabled: true, Start: "06:00", End: "06:00"}); aerr == nil {
		t.Error("cleanQuiet should refuse an empty window")
	}
	if tz, aerr := cleanTimeZone(" Europe/Paris "); aerr != nil || tz != "Europe/Paris" {
		t.Errorf("cleanTimeZone = %q, %v", tz, aerr)
	}
}

// --- Page ---

func TestPageHasTheProfileSettings(t *testing.T) {
	page := string(indexHTML)
	for _, want := range []string{"data-reason", "groupOn", "groupMinutes", "pqOn", "pqStart", "pqEnd", "group_minutes", "episode_reasons", "grpTest", "default_group_title"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page has no %q", want)
		}
	}
}
