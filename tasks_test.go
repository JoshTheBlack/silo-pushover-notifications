package main

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
)

const (
	pathNodes     = "/api/v2/admin/nodes/summary"
	pathStatus    = "/api/v2/admin/server/status"
	pathResources = "/api/v2/admin/system/resources"
	pathTop       = "/api/v2/admin/stats/top-activity"
	pathActivity  = "/api/v2/admin/stats/playback-activity"
)

// serve makes the fake Silo answer path with a testdata fixture, changed by
// edit.
func (h *harness) serve(t *testing.T, path, fixtureName string, edit func(map[string]any)) {
	t.Helper()
	body := fixture(t, fixtureName)
	if edit != nil {
		edit(body)
	}
	h.siloMu.Lock()
	h.static[path] = body
	h.siloMu.Unlock()
}

// serveHealth makes the three health routes answer with their fixtures: two
// healthy nodes, nothing needing a restart, 50 percent memory, a scratch disk
// 20 percent full and a library disk 60 percent full.
func (h *harness) serveHealth(t *testing.T) {
	t.Helper()
	h.serve(t, pathNodes, "nodes_summary", nil)
	h.serve(t, pathStatus, "server_status", nil)
	h.serve(t, pathResources, "system_resources", nil)
}

// editNode changes fields of the fixture's node with the ID.
func editNode(id string, fields map[string]any) func(map[string]any) {
	return func(b map[string]any) {
		for _, it := range b["items"].([]any) {
			if n := it.(map[string]any); n["id"] == id {
				maps.Copy(n, fields)
			}
		}
	}
}

// editSystem changes the resources fixture's system stats.
func editSystem(fields map[string]any) func(map[string]any) {
	return func(b map[string]any) { maps.Copy(b["system"].(map[string]any), fields) }
}

// editDisk sets how much of a disk (by role) is used, of its total.
func editDisk(role string, used float64) func(map[string]any) {
	return func(b map[string]any) {
		for _, it := range b["system"].(map[string]any)["disks"].([]any) {
			if d := it.(map[string]any); d["role"] == role {
				d["used_gb"] = used
			}
		}
	}
}

// at sets the clock the next task runs see.
func (h *harness) clock(now time.Time) { h.plugin.taskNow = func() time.Time { return now } }

func (h *harness) runTask(t *testing.T, capability string) (map[string]any, error) {
	t.Helper()
	resp, err := (&taskServer{plugin: h.plugin}).Run(context.Background(), &pluginv1.RunScheduledTaskRequest{TaskKey: "plugin:9:" + capability})
	if err != nil {
		return nil, err
	}
	return resp.GetOutput().AsMap(), nil
}

func (h *harness) mustRun(t *testing.T, capability string) map[string]any {
	t.Helper()
	out, err := h.runTask(t, capability)
	if err != nil {
		t.Fatalf("%s: %v", capability, err)
	}
	return out
}

func withLimits(disk, memory int) func(*AdminSettings) {
	return func(a *AdminSettings) { a.Limits.DiskPct, a.Limits.MemoryPct = disk, memory }
}

// sentMessages is "title: message" for each message an admin got.
func (h *harness) sentMessages(userKey string) []string {
	var out []string
	for _, m := range h.sentTo(userKey) {
		out = append(out, m.Get("title")+": "+m.Get("message"))
	}
	return out
}

func TestTaskCapability(t *testing.T) {
	for key, want := range map[string]string{
		"plugin:9:health_check":   "health_check",
		"plugin:12:weekly_digest": "weekly_digest",
		"plugin:9:odd:id":         "odd:id",
		"health_check":            "health_check",
	} {
		if got := taskCapability(key); got != want {
			t.Errorf("taskCapability(%q) = %q, want %q", key, got, want)
		}
	}
	h := newHarness(t)
	if _, err := h.runTask(t, "nope"); err == nil {
		t.Fatal("an unknown task should fail")
	}
}

func TestManifestDeclaresScheduledTasks(t *testing.T) {
	manifest, err := publicmanifest.LoadWithChecksum(manifestJSON, "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range manifest.GetCapabilities() {
		if c.GetType() == "scheduled_task.v1" {
			got[c.GetId()] = c.GetDisplayName()
		}
	}
	if len(got) != 2 || got[capHealthCheck] == "" || got[capWeeklyDigest] == "" {
		t.Fatalf("scheduled task capabilities = %v", got)
	}
	setup := manifest.GetPresentation().GetSetupMarkdown()
	for _, want := range []string{"task-bindings/{capability_id}", "interval_ms", "weekly", "Admin > Plugins", "restart"} {
		if !strings.Contains(setup, want) {
			t.Errorf("setup text doesn't mention %q", want)
		}
	}
	// The page explains the same steps.
	for _, want := range []string{"task-bindings/health_check", "task-bindings/weekly_digest", "Restart Silo", "Admin &gt; Plugins"} {
		if !strings.Contains(string(indexHTML), want) {
			t.Errorf("the page doesn't mention %q", want)
		}
	}
}

func TestHealthCheckAlertsOnChangesAndRecovers(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertHealth, alertHealthRecovered))
	h.serveHealth(t)
	h.clock(testNow)

	out := h.mustRun(t, capHealthCheck)
	if len(h.pushover.messages()) != 0 || out["status"] != "ok" || out["nodes_checked"] != float64(2) {
		t.Fatalf("a healthy server sent %v (%v)", h.sentMessages(testUserKey), out)
	}

	// A node goes down.
	h.serve(t, pathNodes, "nodes_summary", editNode("3", map[string]any{"healthy": false}))
	h.clock(testNow.Add(5 * time.Minute))
	h.mustRun(t, capHealthCheck)
	want := "Silo needs attention: Transcode node “gpu-1” isn't healthy"
	if got := h.sentMessages(testUserKey); len(got) != 1 || got[0] != want {
		t.Fatalf("sent %q, want %q", got, want)
	}
	// Still down: nothing more, however many checks run.
	for i := 2; i < 5; i++ {
		h.clock(testNow.Add(time.Duration(i*5) * time.Minute))
		h.mustRun(t, capHealthCheck)
	}
	if n := len(h.pushover.messages()); n != 1 {
		t.Fatalf("a node that stayed down sent %d alerts", n)
	}

	// Restart required joins it: only the new problem is announced, and the
	// open one is mentioned.
	h.serve(t, pathStatus, "server_status", func(b map[string]any) {
		b["restart_required"] = true
		b["restart_required_reasons"] = []any{"plugin_task_binding"}
	})
	h.mustRun(t, capHealthCheck)
	got := h.sentMessages(testUserKey)
	if len(got) != 2 || got[1] != "Silo needs attention: Silo needs a restart (plugin_task_binding)\nStill open: Transcode node “gpu-1” isn't healthy" {
		t.Fatalf("sent %q", got)
	}

	// The node comes back: a recovery push names it and what is still open.
	h.serve(t, pathNodes, "nodes_summary", nil)
	h.mustRun(t, capHealthCheck)
	got = h.sentMessages(testUserKey)
	if len(got) != 3 || got[2] != "Silo recovered: Transcode node “gpu-1” is healthy again\nStill open: Silo needs a restart (plugin_task_binding)" {
		t.Fatalf("sent %q", got)
	}
	// Everything clears.
	h.serveHealth(t)
	h.mustRun(t, capHealthCheck)
	got = h.sentMessages(testUserKey)
	if len(got) != 4 || got[3] != "Silo recovered: Silo no longer needs a restart" {
		t.Fatalf("sent %q", got)
	}
	h.mustRun(t, capHealthCheck)
	if n := len(h.pushover.messages()); n != 4 {
		t.Fatalf("a healthy server sent %d alerts", n-4)
	}

	// The same node failing again later is a new alert, not a repeat of the
	// first one.
	h.serve(t, pathNodes, "nodes_summary", editNode("3", map[string]any{"healthy": false}))
	h.mustRun(t, capHealthCheck)
	if got := h.sentMessages(testUserKey); len(got) != 5 || got[4] != want {
		t.Fatalf("after it failed again: %q", got)
	}
}

func TestHealthCheckKeepsLastResultInState(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertHealth))
	h.serveHealth(t)
	h.clock(testNow)
	h.serve(t, pathNodes, "nodes_summary", editNode("4", map[string]any{"healthy": false}))
	h.mustRun(t, capHealthCheck)

	var st healthState
	if err := json.Unmarshal(h.state.values[healthStateKey], &st); err != nil {
		t.Fatal(err)
	}
	if st.Last == nil || !st.Last.NodesOK || len(st.Last.Nodes) != 2 || st.Last.MemPct != 50 || len(st.Last.Disks) != 2 || !st.Last.At.Equal(testNow) {
		t.Fatalf("last result = %+v", st.Last)
	}
	if a := st.Admins["7"]; a == nil || a.Seq != int(testNow.Unix()/3600)+1 || len(a.Open) != 1 || a.Open["node:4"].Text != "Proxy node “edge-1” isn't healthy" {
		t.Fatalf("admin state = %+v", st.Admins["7"])
	}
	// An unchanged result isn't written again every run.
	before := string(h.state.values[healthStateKey])
	h.clock(testNow.Add(5 * time.Minute))
	h.mustRun(t, capHealthCheck)
	if string(h.state.values[healthStateKey]) != before {
		t.Fatal("an unchanged check rewrote the state")
	}
}

func TestHealthCheckDiskAndMemoryLimitsPerAdmin(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, and(onlyAlerts(alertHealth, alertHealthRecovered), withLimits(90, 0)))
	h.optInAdmin(t, 9, "p9", testAdmin2Key, and(onlyAlerts(alertHealth, alertHealthRecovered), withLimits(80, 70)))
	h.serveHealth(t)
	h.clock(testNow)

	// Library disk 85% full, memory 75%: only the stricter admin hears.
	h.serve(t, pathResources, "system_resources", func(b map[string]any) {
		editDisk("library-1", 850)(b)
		editSystem(map[string]any{"mem_used_mb": 12288})(b)
	})
	h.mustRun(t, capHealthCheck)
	if got := h.sentMessages(testUserKey); len(got) != 0 {
		t.Fatalf("admin 7 (limit 90) got %q", got)
	}
	got := h.sentMessages(testAdmin2Key)
	if len(got) != 1 || got[0] != "Silo needs attention: Memory is 75% used (limit 70%)\nDisk “library 1” is 85% full (limit 80%)" &&
		got[0] != "Silo needs attention: Disk “library 1” is 85% full (limit 80%)\nMemory is 75% used (limit 70%)" {
		t.Fatalf("admin 9 got %q", got)
	}

	// 93% full crosses admin 7's limit too.
	h.serve(t, pathResources, "system_resources", func(b map[string]any) {
		editDisk("library-1", 930)(b)
		editSystem(map[string]any{"mem_used_mb": 12288})(b)
	})
	h.mustRun(t, capHealthCheck)
	if got := h.sentMessages(testUserKey); len(got) != 1 || got[0] != "Silo needs attention: Disk “library 1” is 93% full (limit 90%)" {
		t.Fatalf("admin 7 got %q", got)
	}
	if n := len(h.sentTo(testAdmin2Key)); n != 1 {
		t.Fatalf("admin 9 was told again (%d messages)", n)
	}

	// 89% is under 90 but not clearly: the problem stays open. 87% clears it.
	h.serve(t, pathResources, "system_resources", func(b map[string]any) {
		editDisk("library-1", 890)(b)
		editSystem(map[string]any{"mem_used_mb": 12288})(b)
	})
	h.mustRun(t, capHealthCheck)
	if got := h.sentMessages(testUserKey); len(got) != 1 {
		t.Fatalf("a reading just under the limit sent %q", got)
	}
	h.serve(t, pathResources, "system_resources", func(b map[string]any) {
		editDisk("library-1", 870)(b)
		editSystem(map[string]any{"mem_used_mb": 12288})(b)
	})
	h.mustRun(t, capHealthCheck)
	if got := h.sentMessages(testUserKey); len(got) != 2 || got[1] != "Silo recovered: Disk “library 1” is down to 87%" {
		t.Fatalf("admin 7 got %q", got)
	}
	// Admin 9's disk (limit 80) is at 87%: still a problem for them, no push.
	if n := len(h.sentTo(testAdmin2Key)); n != 1 {
		t.Fatalf("admin 9 got %d messages", n)
	}

	// Turning a limit off drops the problem without a recovery push.
	if err := h.plugin.admins.Put(context.Background(), func() *AdminSettings {
		a, _, _ := h.plugin.admins.Get(context.Background(), 9)
		a.Limits.MemoryPct = 0
		return a
	}()); err != nil {
		t.Fatal(err)
	}
	h.plugin.admins.loadedAt = time.Time{}
	h.mustRun(t, capHealthCheck)
	if n := len(h.sentTo(testAdmin2Key)); n != 1 {
		t.Fatalf("switching a limit off sent %d messages", n-1)
	}
	var st healthState
	_ = json.Unmarshal(h.state.values[healthStateKey], &st)
	if _, ok := st.Admins["9"].Open["memory"]; ok {
		t.Fatal("the memory problem stayed open after its limit was removed")
	}
}

// A part of Silo the check couldn't read neither raises nor clears problems.
func TestHealthCheckKeepsProblemsItCannotCheck(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, and(onlyAlerts(alertHealth, alertHealthRecovered), withLimits(90, 0)))
	h.serveHealth(t)
	h.clock(testNow)
	h.serve(t, pathNodes, "nodes_summary", editNode("3", map[string]any{"healthy": false}))
	h.serve(t, pathResources, "system_resources", editDisk("library-1", 950))
	h.mustRun(t, capHealthCheck)
	if n := len(h.pushover.messages()); n != 1 {
		t.Fatalf("sent %d, want 1", n)
	}

	// The node list and a stale resource sample can't be trusted now.
	h.failPath(pathNodes, true)
	h.serve(t, pathResources, "system_resources", func(b map[string]any) {
		b["stale"] = true
		editDisk("library-1", 10)(b)
	})
	h.mustRun(t, capHealthCheck)
	if n := len(h.pushover.messages()); n != 1 {
		t.Fatalf("an unreadable source cleared problems (%d messages)", n)
	}
	var st healthState
	_ = json.Unmarshal(h.state.values[healthStateKey], &st)
	if len(st.Admins["7"].Open) != 2 {
		t.Fatalf("open problems = %v", st.Admins["7"].Open)
	}

	// A disk Silo hasn't measured is carried too; a measured one clears.
	h.failPath(pathNodes, false)
	h.serve(t, pathNodes, "nodes_summary", nil)
	h.serve(t, pathResources, "system_resources", func(b map[string]any) {
		for _, it := range b["system"].(map[string]any)["disks"].([]any) {
			if d := it.(map[string]any); d["role"] == "library-1" {
				d["stale"] = true
			}
		}
	})
	h.mustRun(t, capHealthCheck)
	got := h.sentMessages(testUserKey)
	if len(got) != 2 || got[1] != "Silo recovered: Transcode node “gpu-1” is healthy again\nStill open: Disk “library 1” is 95% full (limit 90%)" {
		t.Fatalf("sent %q", got)
	}
}

func TestHealthCheckDependenciesAndNewNodes(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertHealth, alertHealthRecovered))
	h.serveHealth(t)
	h.clock(testNow)
	h.serve(t, pathStatus, "server_status", func(b map[string]any) {
		b["health"].(map[string]any)["redis"] = map[string]any{"configured": true, "ok": false}
	})
	// A node that was never checked, or is disabled, isn't a problem.
	h.serve(t, pathNodes, "nodes_summary", func(b map[string]any) {
		editNode("3", map[string]any{"healthy": false, "last_health_check": nil})(b)
		editNode("4", map[string]any{"healthy": false, "enabled": false})(b)
	})
	h.mustRun(t, capHealthCheck)
	if got := h.sentMessages(testUserKey); len(got) != 1 || got[0] != "Silo needs attention: Redis isn't responding" {
		t.Fatalf("sent %q", got)
	}
}

func TestHealthCheckFailedSendIsRetriedOnceWithoutRepeats(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertHealth, alertHealthRecovered))
	h.serveHealth(t)
	h.clock(testNow)
	h.serve(t, pathNodes, "nodes_summary", editNode("3", map[string]any{"healthy": false}))

	h.pushover.fail.Store(http.StatusServiceUnavailable)
	if _, err := h.runTask(t, capHealthCheck); err == nil {
		t.Fatal("a send that could still work should fail the run, so it shows in Silo's task history")
	}
	h.pushover.fail.Store(0)
	h.clock(testNow.Add(5 * time.Minute))
	h.mustRun(t, capHealthCheck) // the next run sends it
	h.mustRun(t, capHealthCheck)
	if got := h.sentMessages(testUserKey); len(got) != 1 {
		t.Fatalf("sent %q", got)
	}

	// The state write is lost after the send (another server overwrote it,
	// say): the same change is announced under the same ID, which is skipped.
	delete(h.state.values, healthStateKey)
	h.mustRun(t, capHealthCheck)
	delete(h.state.values, healthStateKey)
	other := h.newPlugin(h.state) // and it may be another server's run
	other.taskNow = func() time.Time { return testNow.Add(10 * time.Minute) }
	if _, err := (&taskServer{plugin: other}).Run(context.Background(), &pluginv1.RunScheduledTaskRequest{TaskKey: "plugin:9:" + capHealthCheck}); err != nil {
		t.Fatal(err)
	}
	if got := h.sentMessages(testUserKey); len(got) != 1 {
		t.Fatalf("a repeated change sent %q", got)
	}

	// A permanent rejection is final: no retry loop.
	h2 := newHarness(t)
	h2.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertHealth))
	h2.serveHealth(t)
	h2.serve(t, pathNodes, "nodes_summary", editNode("3", map[string]any{"healthy": false}))
	h2.pushover.fail.Store(http.StatusBadRequest)
	if _, err := h2.runTask(t, capHealthCheck); err != nil {
		t.Fatalf("a rejected alert should not fail the run: %v", err)
	}
}

func TestHealthCheckSkips(t *testing.T) {
	t.Run("no read-only key", func(t *testing.T) {
		h := newHarness(t)
		h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertHealth))
		h.plugin.setConfig(adminConfig{DefaultAppToken: testAppToken})
		out := h.mustRun(t, capHealthCheck)
		if !strings.HasPrefix(out["status"].(string), "skipped") || h.siloCalls.Load() != 0 {
			t.Fatalf("%v, %d Silo calls", out, h.siloCalls.Load())
		}
	})
	t.Run("nobody opted in", func(t *testing.T) {
		h := newHarness(t)
		h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertStreamStarted, alertDigest))
		h.serveHealth(t)
		out := h.mustRun(t, capHealthCheck)
		if !strings.HasPrefix(out["status"].(string), "skipped") || h.callsTo(pathNodes) != 0 {
			t.Fatalf("%v, %d node calls", out, h.callsTo(pathNodes))
		}
	})
	t.Run("an admin no longer an admin", func(t *testing.T) {
		h := newHarness(t)
		h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertHealth))
		h.setAccount("7", fakeAccount{"josh", "user", true})
		h.serveHealth(t)
		h.serve(t, pathNodes, "nodes_summary", editNode("3", map[string]any{"healthy": false}))
		h.mustRun(t, capHealthCheck)
		if n := len(h.pushover.messages()); n != 0 {
			t.Fatalf("a demoted admin got %d alerts", n)
		}
	})
	t.Run("rejected key", func(t *testing.T) {
		h := newHarness(t)
		h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertHealth))
		h.plugin.setConfig(adminConfig{MonitorAPIKey: "sa_revoked", DefaultAppToken: testAppToken})
		h.serveHealth(t)
		out := h.mustRun(t, capHealthCheck)
		if !strings.HasPrefix(out["status"].(string), "skipped") || len(h.pushover.messages()) != 0 {
			t.Fatalf("%v", out)
		}
	})
	t.Run("silo down", func(t *testing.T) {
		h := newHarness(t)
		h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertHealth))
		h.serveHealth(t)
		h.failPath(pathNodes, true)
		h.failPath(pathStatus, true)
		h.failPath(pathResources, true)
		if _, err := h.runTask(t, capHealthCheck); err == nil {
			t.Fatal("a check that read nothing should fail")
		}
		if len(h.pushover.messages()) != 0 {
			t.Fatal("an unreadable Silo sent alerts")
		}
	})
}

// Admins who stop wanting health alerts are forgotten, so turning them back on
// reports whatever is wrong then.
func TestHealthCheckForgetsAdminsWhoTurnItOff(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertHealth, alertHealthRecovered))
	h.serveHealth(t)
	h.clock(testNow)
	h.serve(t, pathNodes, "nodes_summary", editNode("3", map[string]any{"healthy": false}))
	h.mustRun(t, capHealthCheck)
	h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertStreamStarted))
	h.plugin.admins.loadedAt = time.Time{}
	h.mustRun(t, capHealthCheck)
	var st healthState
	_ = json.Unmarshal(h.state.values[healthStateKey], &st)
	// What the admin was told is forgotten; their change counter isn't.
	if a := st.Admins["7"]; a == nil || len(a.Open) != 0 || a.Seq != int(testNow.Unix()/3600)+1 {
		t.Fatalf("admins = %+v", st.Admins["7"])
	}
	h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertHealth))
	h.plugin.admins.loadedAt = time.Time{}
	h.mustRun(t, capHealthCheck)
	if n := len(h.pushover.messages()); n != 2 {
		t.Fatalf("turning it back on sent %d messages in all, want 2", n)
	}
}

func TestHealthStateIsCapped(t *testing.T) {
	st := &healthState{Last: &healthResult{}, Admins: map[string]*healthAdmin{}}
	for i := range 6000 {
		st.Last.Nodes = append(st.Last.Nodes, healthNode{ID: strings.Repeat("n", 20) + string(rune('a'+i%26)), Name: strings.Repeat("name", 10), Type: "transcode"})
		st.Last.Disks = append(st.Last.Disks, healthDisk{Key: "disk:" + strings.Repeat("d", 40), Name: strings.Repeat("disk", 10)})
	}
	for i := range 20 {
		st.Admins[string(rune('A'+i))] = &healthAdmin{Seq: i, Updated: testNow.Add(time.Duration(i) * time.Minute)}
	}
	raw, err := encodeCapped(st)
	if err != nil || len(raw) > maxStateValue {
		t.Fatalf("encodeCapped: %d bytes, %v", len(raw), err)
	}
	if len(st.Admins) != 20 {
		t.Fatalf("the cap dropped %d admins before it had to", 20-len(st.Admins))
	}
}

// --- Weekly digest ---

func (h *harness) serveDigest(t *testing.T) {
	t.Helper()
	h.serve(t, pathTop, "top_activity", nil)
	h.serve(t, pathActivity, "playback_activity", nil)
}

func TestWeeklyDigest(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertDigest))
	h.serveDigest(t)
	h.clock(testNow)

	out := h.mustRun(t, capWeeklyDigest)
	if out["status"] != "ok" || out["digests_sent"] != float64(1) || out["week"] != "2026-W45" {
		t.Fatalf("output = %v", out)
	}
	msgs := h.sentTo(testUserKey)
	if len(msgs) != 1 {
		t.Fatalf("sent %d digests", len(msgs))
	}
	want := "128 plays · 28h 47m watched\n6 profiles · 32% transcoded · 78% finished\n" +
		"Top viewers\n1. alice (Kids) · 31 plays · 12h 5m\n2. bob · 22 plays · 9h 40m\n3. sam · 18 plays · 7h 2m\n" +
		"Top titles\n1. Severance · 14 plays\n2. The Bear · 9 plays\n3. Dune: Part Two · 1 play"
	if got := msgs[0].Get("message"); got != want {
		t.Fatalf("message:\n%s\nwant:\n%s", got, want)
	}
	if msgs[0].Get("title") != "Silo this week" {
		t.Fatalf("title = %q", msgs[0].Get("title"))
	}
	h.siloMu.Lock()
	topQuery, activityQuery := h.lastQuery[pathTop], h.lastQuery[pathActivity]
	h.siloMu.Unlock()
	if topQuery != "days=7&limit=25" || activityQuery != "hours=168" {
		t.Fatalf("queries = %q, %q", topQuery, activityQuery)
	}

	// The same week again (a restart, another server, a startup trigger): no
	// second digest and no Silo reads.
	calls := h.callsTo(pathTop)
	h.clock(testNow.Add(3 * time.Hour))
	out = h.mustRun(t, capWeeklyDigest)
	other := h.newPlugin(h.state)
	other.taskNow = func() time.Time { return testNow.Add(4 * time.Hour) }
	if _, err := (&taskServer{plugin: other}).Run(context.Background(), &pluginv1.RunScheduledTaskRequest{TaskKey: "plugin:9:" + capWeeklyDigest}); err != nil {
		t.Fatal(err)
	}
	if n := len(h.pushover.messages()); n != 1 || h.callsTo(pathTop) != calls || !strings.HasPrefix(out["status"].(string), "skipped") {
		t.Fatalf("a repeat in the same week sent %d messages, %d Silo reads (%v)", n, h.callsTo(pathTop)-calls, out)
	}

	// Next week's digest goes out.
	h.clock(testNow.Add(7 * 24 * time.Hour))
	h.mustRun(t, capWeeklyDigest)
	if n := len(h.pushover.messages()); n != 2 {
		t.Fatalf("next week sent %d messages in all", n)
	}
}

func TestWeeklyDigestLeavesOutIgnoredUsers(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, and(onlyAlerts(alertDigest), func(a *AdminSettings) { a.Filters.IgnoreUsers = []string{"12"} }))
	h.optInAdmin(t, 9, "p9", testAdmin2Key, onlyAlerts(alertDigest))
	h.serveDigest(t)
	h.mustRun(t, capWeeklyDigest)
	mine, theirs := h.sentTo(testUserKey)[0].Get("message"), h.sentTo(testAdmin2Key)[0].Get("message")
	if strings.Contains(mine, "alice") || !strings.Contains(mine, "1. bob · 22 plays · 9h 40m") || !strings.Contains(mine, "watched") {
		t.Fatalf("admin who ignores alice got:\n%s", mine)
	}
	if !strings.Contains(theirs, "1. alice (Kids)") {
		t.Fatalf("the other admin got:\n%s", theirs)
	}
}

func TestWeeklyDigestStaysUnderPushoverLimit(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertDigest))
	// Pushover's HTML mode escapes the names, which makes them longer.
	if err := h.plugin.store.Put(context.Background(), &ProfileSettings{ProfileID: "p1", ProfileName: "Josh", UserID: 7, Enabled: true, UserKey: testUserKey, HTML: true}); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("Ünïcödé & <name> ", 10)
	h.serve(t, pathTop, "top_activity", func(b map[string]any) {
		var profiles, titles []any
		for i := range 25 {
			profiles = append(profiles, map[string]any{"user_id": string(rune('a' + i)), "username": long, "profile_id": "p", "profile_name": long + "x", "plays": 100 + i, "total_seconds": 360000 - i})
			titles = append(titles, map[string]any{"media_item_id": "m", "title": long, "media_type": "movie", "plays": 50 - i, "total_seconds": 1})
		}
		b["profiles"], b["titles"] = profiles, titles
	})
	h.serve(t, pathActivity, "playback_activity", nil)
	h.mustRun(t, capWeeklyDigest)
	msg := h.sentTo(testUserKey)[0].Get("message")
	if n := utf8.RuneCountInString(msg); n > maxDigestRunes || n < 100 {
		t.Fatalf("digest is %d characters:\n%s", n, msg)
	}
	if !strings.Contains(msg, "at least") {
		t.Fatalf("a cut-off viewer list should say the watch time is a minimum:\n%s", msg)
	}
	// A quiet week still reads well.
	h2 := newHarness(t)
	h2.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertDigest))
	h2.serve(t, pathTop, "top_activity", func(b map[string]any) { b["profiles"], b["titles"] = []any{}, []any{} })
	h2.serve(t, pathActivity, "playback_activity", func(b map[string]any) {
		b["buckets"] = []any{}
		b["reliability"] = map[string]any{"sessions_started": 0, "transcode_starts": 0, "finalized_sessions": 0, "completed_sessions": 0, "completion_rate": 0, "unique_profiles": 0}
	})
	h2.mustRun(t, capWeeklyDigest)
	if got := h2.sentTo(testUserKey)[0].Get("message"); got != "0 plays · 0m watched\n0 profiles · 0% transcoded · 0% finished" {
		t.Fatalf("quiet week: %q", got)
	}
}

func TestWeeklyDigestRetriesAndFails(t *testing.T) {
	t.Run("a send that fails once is tried again in the run", func(t *testing.T) {
		h := newHarness(t)
		h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertDigest))
		h.serveDigest(t)
		h.pushover.fail.Store(http.StatusServiceUnavailable)
		time.AfterFunc(300*time.Millisecond, func() { h.pushover.fail.Store(0) })
		h.mustRun(t, capWeeklyDigest)
		if n := len(h.pushover.messages()); n != 1 {
			t.Fatalf("sent %d digests", n)
		}
	})
	t.Run("a send that keeps failing fails the run, and the next run sends it", func(t *testing.T) {
		h := newHarness(t)
		h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertDigest))
		h.serveDigest(t)
		h.pushover.fail.Store(http.StatusServiceUnavailable)
		if _, err := h.runTask(t, capWeeklyDigest); err == nil {
			t.Fatal("expected an error")
		}
		h.pushover.fail.Store(0)
		h.mustRun(t, capWeeklyDigest)
		if n := len(h.pushover.messages()); n != 1 {
			t.Fatalf("sent %d digests", n)
		}
	})
	t.Run("statistics that can't be read", func(t *testing.T) {
		h := newHarness(t)
		h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertDigest))
		h.serveDigest(t)
		h.failPath(pathTop, true)
		if _, err := h.runTask(t, capWeeklyDigest); err == nil {
			t.Fatal("expected an error")
		}
		if len(h.pushover.messages()) != 0 {
			t.Fatal("sent a digest without its numbers")
		}
	})
	t.Run("nobody opted in, or no key", func(t *testing.T) {
		h := newHarness(t)
		h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertHealth))
		h.serveDigest(t)
		out := h.mustRun(t, capWeeklyDigest)
		if !strings.HasPrefix(out["status"].(string), "skipped") || h.callsTo(pathTop) != 0 {
			t.Fatalf("%v", out)
		}
		h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertDigest))
		h.plugin.admins.loadedAt = time.Time{}
		h.plugin.setConfig(adminConfig{DefaultAppToken: testAppToken})
		if out = h.mustRun(t, capWeeklyDigest); !strings.HasPrefix(out["status"].(string), "skipped") {
			t.Fatalf("%v", out)
		}
	})
	t.Run("an admin who is no longer one", func(t *testing.T) {
		h := newHarness(t)
		h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertDigest))
		h.setAccount("7", fakeAccount{"josh", "user", true})
		h.serveDigest(t)
		h.mustRun(t, capWeeklyDigest)
		if len(h.pushover.messages()) != 0 {
			t.Fatal("a demoted admin got the digest")
		}
	})
}

// --- Settings page and API ---

func TestAdminSettingsCoverScheduledAlerts(t *testing.T) {
	h := newHarness(t)
	h.configure(t, nil)
	status, out := h.request(t, http.MethodGet, "/api/admin/alerts", asPrimary, nil)
	if status != http.StatusOK {
		t.Fatalf("GET: %d %v", status, out)
	}
	byKey := map[string]map[string]any{}
	for _, raw := range out["alert_types"].([]any) {
		at := raw.(map[string]any)
		byKey[at["key"].(string)] = at
	}
	health, recovered, digest := byKey[alertHealth], byKey[alertHealthRecovered], byKey[alertDigest]
	if health == nil || recovered == nil || digest == nil {
		t.Fatalf("alert types = %v", byKey)
	}
	if health["group"] != groupScheduled || recovered["parent"] != alertHealth || digest["group"] != groupScheduled {
		t.Fatalf("groups: %v %v %v", health, recovered, digest)
	}
	limits := health["limits"].([]any)
	if len(limits) != 2 || limits[0].(map[string]any)["key"] != "disk_pct" || limits[0].(map[string]any)["optional"] != true || limits[1].(map[string]any)["key"] != "memory_pct" {
		t.Fatalf("health limits = %v", limits)
	}
	if lim := byKey[alertBandwidth]["limits"].([]any); len(lim) != 1 || lim[0].(map[string]any)["optional"] != nil {
		t.Fatalf("bandwidth limits = %v", lim)
	}

	save := func(alerts map[string]any, limits map[string]any) (int, map[string]any) {
		return h.request(t, http.MethodPut, "/api/admin/alerts", asPrimary, map[string]any{"alerts": alerts, "limits": limits})
	}
	// The recovery wording follows the health switch.
	status, out = save(map[string]any{alertHealth: map[string]any{"enabled": true, "title": "T", "message": "M"}, alertHealthRecovered: map[string]any{"enabled": false, "title": "R", "message": "R"}}, map[string]any{"disk_pct": 85})
	if status != http.StatusOK {
		t.Fatalf("PUT: %d %v", status, out)
	}
	stored, _, _ := h.plugin.admins.Get(context.Background(), 7)
	if !stored.alert(alertHealth).Enabled || !stored.alert(alertHealthRecovered).Enabled || stored.alert(alertHealthRecovered).Title != "R" || stored.Limits.DiskPct != 85 || stored.Limits.MemoryPct != 0 {
		t.Fatalf("stored = %+v", stored)
	}
	// Health with no limits is fine; limits must be 1..99.
	if status, out = save(map[string]any{alertHealth: map[string]any{"enabled": true}}, nil); status != http.StatusOK {
		t.Fatalf("health with no limits: %d %v", status, out)
	}
	for _, bad := range []map[string]any{{"disk_pct": 100}, {"memory_pct": -1}, {"memory_pct": 1000}} {
		if status, _ = save(nil, bad); status != http.StatusUnprocessableEntity {
			t.Fatalf("limits %v: %d", bad, status)
		}
	}

	// Test sends use the sample and the unsaved wording.
	for _, key := range []string{alertHealth, alertHealthRecovered, alertDigest} {
		before := len(h.pushover.messages())
		status, out = h.request(t, http.MethodPost, "/api/admin/alerts/test", asPrimary, map[string]any{"alert": key, "settings": map[string]any{}})
		if status != http.StatusOK || len(h.pushover.messages()) != before+1 {
			t.Fatalf("test send of %s: %d %v", key, status, out)
		}
	}
	msgs := h.pushover.messages()
	last := msgs[len(msgs)-1]
	if !strings.HasPrefix(last.Get("title"), "[Test] Silo this week") || !strings.Contains(last.Get("message"), "128 plays · 41h 20m watched") || len(last.Get("message")) >= maxMessageRunes {
		t.Fatalf("digest test send = %q / %q", last.Get("title"), last.Get("message"))
	}
}

// Every scheduled alert's sample renders every placeholder it offers.
func TestScheduledAlertSamplesFillTheirPlaceholders(t *testing.T) {
	for _, key := range []string{alertHealth, alertHealthRecovered, alertDigest} {
		at, _ := adminAlertTypeByKey(key)
		for _, ph := range at.Placeholders {
			if _, ok := at.Sample[ph]; !ok {
				t.Errorf("%s: no sample for {%s}", key, ph)
			}
		}
		for _, tmpl := range []string{at.DefaultTitle, at.DefaultMessage} {
			for _, m := range placeholderPattern.FindAllStringSubmatch(tmpl, -1) {
				if !strings.Contains(strings.Join(at.Placeholders, ","), m[1]) {
					t.Errorf("%s: default wording uses {%s}, which isn't offered", key, m[1])
				}
			}
		}
	}
}

// An admin who only wants scheduled alerts doesn't make the plugin track
// every stream.
func TestScheduledAlertsDontTrackSessions(t *testing.T) {
	h := newHarness(t)
	h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertHealth, alertDigest))
	h.mustHandle(t, sessionEvent(t, eventSessionStarted, "e1", "session_started", nil))
	if _, tracked := h.state.values[sessionsKey]; tracked || len(h.pushover.messages()) != 0 {
		t.Fatal("a stream was tracked for an admin with only scheduled alerts")
	}
	h.optInAdmin(t, 7, "p1", testUserKey, onlyAlerts(alertHealth, alertStreamStarted))
	h.plugin.admins.loadedAt = time.Time{}
	h.mustHandle(t, sessionEvent(t, eventSessionStarted, "e2", "session_started", nil))
	if _, tracked := h.state.values[sessionsKey]; !tracked {
		t.Fatal("a stream wasn't tracked for an admin who wants stream alerts")
	}
}

// titleCase works on characters, so a name that starts with a multi-byte one
// isn't mangled.
func TestTitleCaseKeepsMultibyteText(t *testing.T) {
	for in, want := range map[string]string{"remote": "Remote", "": "", "élan vital": "Élan vital", "日本語": "日本語", "Ünï": "Ünï", "a": "A"} {
		if got := titleCase(in); got != want {
			t.Errorf("titleCase(%q) = %q, want %q", in, got, want)
		}
	}
	if got := humanizeType("élan.vital_x"); got != "Élan vital x" {
		t.Errorf("humanizeType = %q", got)
	}
}
