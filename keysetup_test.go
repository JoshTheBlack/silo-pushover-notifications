package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimehost"
)

// Full-access keys the fake Silo knows. Silo shows the first 11 characters
// of a key as its key_prefix.
var (
	testFullKey      = "sa_full0001" + strings.Repeat("a", 56)
	testOtherFullKey = "sa_othr0002" + strings.Repeat("b", 56)
	testLegacyFull   = "sa_lgcy0003" + strings.Repeat("c", 56)
	testScopedKey    = "sa_scop0004" + strings.Repeat("d", 56)
	testNonAdminKey  = "sa_kidk0005" + strings.Repeat("e", 56)
	testControlOnly  = "sa_ctrl0006" + strings.Repeat("f", 56)
)

type fakeKey struct {
	id     int
	userID int
	label  string
	secret string
	scopes []string
}

// fakeKeySilo models the parts of Silo's API key admin the setup uses:
// scoped keys only reach their scope's routes (api_key_scopes.go), admin
// routes need an admin account, the key list is paged, DELETE needs
// If-Match, and created keys are shown once.
type fakeKeySilo struct {
	url string

	mu     sync.Mutex
	keys   []*fakeKey
	nextID int
	admins map[int]bool
	// created are the secrets of keys made through POST.
	created []string
	// seq records each request ("METHOD path") and config save ("SAVE").
	seq []string

	pageSize      int
	failCreate    map[string]int // first scope -> HTTP status
	failDelete    int
	rejectCreated bool // keys made through POST get 401 when used
	createDelay   time.Duration
}

var profilesPath = regexp.MustCompile(`^/api/v2/admin/users/[0-9]+/profiles$`)

func newFakeKeySilo(t *testing.T) *fakeKeySilo {
	f := &fakeKeySilo{nextID: 1, admins: map[int]bool{7: true}, pageSize: 200, failCreate: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

func (f *fakeKeySilo) add(userID int, label, secret string, scopes ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, &fakeKey{id: f.nextID, userID: userID, label: label, secret: secret, scopes: scopes})
	f.nextID++
}

func (f *fakeKeySilo) record(event string) {
	f.mu.Lock()
	f.seq = append(f.seq, event)
	f.mu.Unlock()
}

func (f *fakeKeySilo) events() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.seq)
}

// count is how many recorded events start with prefix.
func (f *fakeKeySilo) count(prefix string) int {
	n := 0
	for _, e := range f.events() {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeKeySilo) key(secret string) *fakeKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.findLocked(secret)
}

func (f *fakeKeySilo) findLocked(secret string) *fakeKey {
	for _, k := range f.keys {
		if k.secret == secret {
			c := *k
			return &c
		}
	}
	return nil
}

// createdKeys returns the keys made through POST that still exist.
func (f *fakeKeySilo) createdKeys() []*fakeKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*fakeKey
	for _, s := range f.created {
		if k := f.findLocked(s); k != nil {
			out = append(out, k)
		}
	}
	return out
}

func (f *fakeKeySilo) secrets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{testFullKey, testOtherFullKey, testLegacyFull, testScopedKey, testNonAdminKey, testControlOnly}, f.created...)
}

func scopeAllows(scopes []string, method, path string) bool {
	if len(scopes) == 0 {
		return true
	}
	for _, s := range scopes {
		switch s {
		case scopeMonitorRead:
			if method == http.MethodGet && (profilesPath.MatchString(path) || path == "/api/v2/admin/system/resources/capabilities" || path == controlProbePath) {
				return true
			}
		case scopeSessionsControl:
			if method == http.MethodGet && path == controlProbePath {
				return true
			}
		}
	}
	return false
}

func problem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"title": http.StatusText(status), "status": status, "detail": detail})
}

func (f *fakeKeySilo) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	f.seq = append(f.seq, r.Method+" "+path)

	caller := f.findLocked(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	switch {
	case caller == nil, f.rejectCreated && slices.Contains(f.created, caller.secret):
		problem(w, http.StatusUnauthorized, "Invalid API key")
		return
	case !scopeAllows(caller.scopes, r.Method, path):
		problem(w, http.StatusForbidden, "API key scopes do not permit this route")
		return
	case !f.admins[caller.userID]:
		problem(w, http.StatusForbidden, "Admin access required")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && path == "/api/v2/admin/api-keys":
		offset, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
		end := min(offset+f.pageSize, len(f.keys))
		items := []map[string]any{}
		for _, k := range f.keys[offset:end] {
			scopes := k.scopes
			if scopes == nil {
				scopes = []string{}
			}
			items = append(items, map[string]any{"id": strconv.Itoa(k.id), "user_id": strconv.Itoa(k.userID), "label": k.label, "key_prefix": k.secret[:11], "scopes": scopes})
		}
		page := map[string]any{"has_more": end < len(f.keys)}
		if end < len(f.keys) {
			page["next_cursor"] = strconv.Itoa(end)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "page": page})

	case r.Method == http.MethodPost && path == "/api/v2/admin/api-keys":
		var body struct {
			Label  string   `json:"label"`
			UserID string   `json:"user_id"`
			Scopes []string `json:"scopes"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Label == "" {
			problem(w, http.StatusUnprocessableEntity, "Invalid API key configuration.")
			return
		}
		// Silo's ID type is a string on the wire.
		userID, _ := strconv.Atoi(body.UserID)
		if userID <= 0 {
			problem(w, http.StatusUnprocessableEntity, "Invalid account ID.")
			return
		}
		if len(body.Scopes) > 0 && f.failCreate[body.Scopes[0]] != 0 {
			problem(w, f.failCreate[body.Scopes[0]], "Owner keys can only be created by the Owner.")
			return
		}
		k := &fakeKey{id: f.nextID, userID: userID, label: body.Label, scopes: body.Scopes, secret: fmt.Sprintf("sa_n%07d%s", f.nextID, strings.Repeat("9", 56))}
		f.nextID++
		f.keys = append(f.keys, k)
		f.created = append(f.created, k.secret)
		if f.createDelay > 0 {
			// The key exists, but the answer comes too late.
			select {
			case <-time.After(f.createDelay):
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": strconv.Itoa(k.id), "user_id": strconv.Itoa(k.userID), "label": k.label, "key": k.secret, "scopes": k.scopes, "rate_tier": "standard"})

	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/api/v2/admin/api-keys/"):
		if r.Header.Get("If-Match") == "" {
			problem(w, http.StatusPreconditionRequired, "If-Match is required.")
			return
		}
		if f.failDelete != 0 {
			problem(w, f.failDelete, "Unable to manage API key.")
			return
		}
		id, _ := strconv.Atoi(strings.TrimPrefix(path, "/api/v2/admin/api-keys/"))
		i := slices.IndexFunc(f.keys, func(k *fakeKey) bool { return k.id == id })
		if i < 0 {
			problem(w, http.StatusNotFound, "API key not found.")
			return
		}
		f.keys = slices.Delete(f.keys, i, i+1)
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodGet && profilesPath.MatchString(path):
		_, _ = io.WriteString(w, `{"items":[{"id":"p1","name":"Josh"},{"id":"p2","name":"Kid"},{"id":"p3","name":"Kid"}]}`)
	case r.Method == http.MethodGet && (path == "/api/v2/admin/system/resources/capabilities" || path == controlProbePath):
		_, _ = io.WriteString(w, `{}`)
	default:
		problem(w, http.StatusNotFound, "Not found.")
	}
}

type keySetupHarness struct {
	*harness
	silo *fakeKeySilo
	logs *syncBuffer

	mu      sync.Mutex
	saved   []map[string]any
	saveErr error
}

// newKeySetupHarness wires a plugin to the fake key Silo and configures it
// the way Silo does at start-up, with the given stored config.
func newKeySetupHarness(t *testing.T, stored map[string]any) *keySetupHarness {
	t.Helper()
	h := newHarness(t)
	ks := &keySetupHarness{harness: h, silo: newFakeKeySilo(t)}
	ks.silo.add(7, "Claude", testFullKey)
	ks.silo.add(7, "Home Assistant", testOtherFullKey)
	h.plugin.hostInfoFn = func(context.Context) (*runtimehost.HostInfo, error) {
		return &runtimehost.HostInfo{PublicBaseURL: "https://silo.example.com", InternalBaseURL: ks.silo.url + "/"}, nil
	}
	h.plugin.setGlobalConfigFn = func(_ context.Context, key string, value map[string]any) error {
		ks.silo.record("SAVE " + key)
		ks.mu.Lock()
		defer ks.mu.Unlock()
		if ks.saveErr != nil {
			return ks.saveErr
		}
		ks.saved = append(ks.saved, maps.Clone(value))
		return nil
	}
	if stored == nil {
		stored = map[string]any{"default_app_token": testAppToken, "public_url": "https://pub.example"}
	}
	ks.logs = configureWith(t, h, stored)
	return ks
}

func (ks *keySetupHarness) setup(t *testing.T, as viewerAs, body map[string]any) (int, map[string]any) {
	t.Helper()
	return ks.request(t, http.MethodPost, "/api/admin/key-setup", as, body)
}

// lastSaved is the config row most recently written.
func (ks *keySetupHarness) lastSaved(t *testing.T) map[string]any {
	t.Helper()
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if len(ks.saved) == 0 {
		t.Fatal("the config was never saved")
	}
	return ks.saved[len(ks.saved)-1]
}

// assertNoLeaks checks that no key reached the response or the logs.
func (ks *keySetupHarness) assertNoLeaks(t *testing.T, bodies ...map[string]any) {
	t.Helper()
	var out strings.Builder
	out.WriteString(ks.logs.String())
	for _, b := range bodies {
		raw, _ := json.Marshal(b)
		out.Write(raw)
	}
	for _, secret := range ks.silo.secrets() {
		if strings.Contains(out.String(), secret) || strings.Contains(out.String(), secret[11:]) {
			t.Fatalf("a key leaked into the response or logs:\n%s", out.String())
		}
	}
}

// before reports whether event a happened before event b.
func before(events []string, a, b string) bool {
	i, j := slices.Index(events, a), slices.Index(events, b)
	return i >= 0 && j >= 0 && i < j
}

func details(body map[string]any) string {
	var parts []string
	for _, d := range body["details"].([]any) {
		parts = append(parts, d.(string))
	}
	return strings.Join(parts, "\n")
}

func TestKeySetupCreatesReadOnlyKey(t *testing.T) {
	ks := newKeySetupHarness(t, nil)
	ks.silo.pageSize = 1 // the pasted key is on the second page

	status, body := ks.setup(t, asPrimary, map[string]any{"key": " " + testFullKey + " ", "revoke": true})
	if status != http.StatusOK || body["saved"] != true || body["level"] != "ok" {
		t.Fatalf("setup: %d %v", status, body)
	}
	created := ks.silo.createdKeys()
	if len(created) != 1 {
		t.Fatalf("created %d keys, want 1", len(created))
	}
	monitor := created[0]
	if monitor.userID != 7 || monitor.label != monitorKeyLabel || !slices.Equal(monitor.scopes, []string{scopeMonitorRead}) {
		t.Fatalf("created key = %+v", monitor)
	}

	// The new key is saved with the other fields carried over and the
	// legacy field gone, and the running config uses it.
	want := map[string]any{"monitor_api_key": monitor.secret, "default_app_token": testAppToken, "public_url": "https://pub.example"}
	if got := ks.lastSaved(t); !maps.Equal(got, want) {
		t.Fatalf("saved config = %v, want %v", got, want)
	}
	if cfg := ks.plugin.config(); cfg.MonitorAPIKey != monitor.secret || cfg.DefaultAppToken != testAppToken {
		t.Fatalf("running config = %v", cfg)
	}

	// The pasted key is revoked, after the save; the other one is left alone.
	if ks.silo.key(testFullKey) != nil || ks.silo.key(testOtherFullKey) == nil {
		t.Fatal("wrong keys revoked")
	}
	events := ks.silo.events()
	for _, pair := range [][2]string{
		{"POST /api/v2/admin/api-keys", "GET /api/v2/admin/users/7/profiles"},
		{"GET /api/v2/admin/users/7/profiles", "SAVE pushover"},
		{"SAVE pushover", "DELETE /api/v2/admin/api-keys/1"},
	} {
		if !before(events, pair[0], pair[1]) {
			t.Fatalf("%q should come before %q: %v", pair[0], pair[1], events)
		}
	}

	// The page now manages the whole household with the new key.
	_, state := ks.request(t, http.MethodGet, "/api/state", asPrimary, nil)
	if state["household"] != "all" || state["admin_keys"].(map[string]any)["monitor_key_set"] != true {
		t.Fatalf("state after setup: %v", state)
	}
	ks.assertNoLeaks(t, body, state)
}

func TestKeySetupAlsoCreatesControlKey(t *testing.T) {
	ks := newKeySetupHarness(t, map[string]any{"default_app_token": testAppToken, "monitor_api_key": "sa_old_monitor", "control_api_key": "sa_old_control"})
	status, body := ks.setup(t, asPrimary, map[string]any{"key": testFullKey, "revoke": false, "control": true})
	if status != http.StatusOK || body["saved"] != true {
		t.Fatalf("setup: %d %v", status, body)
	}
	created := ks.silo.createdKeys()
	if len(created) != 2 || !slices.Equal(created[1].scopes, []string{scopeSessionsControl}) || created[1].label != controlKeyLabel {
		t.Fatalf("created keys = %+v", created)
	}
	saved := ks.lastSaved(t)
	if saved["monitor_api_key"] != created[0].secret || saved["control_api_key"] != created[1].secret {
		t.Fatalf("saved config = %v", saved)
	}
	if ks.plugin.config().ControlAPIKey != created[1].secret {
		t.Fatal("running config lacks the control key")
	}
	// Not asked to revoke: the pasted key stays, and the page says so, as
	// it does for the replaced keys.
	if ks.silo.key(testFullKey) == nil || ks.silo.count("DELETE") != 0 {
		t.Fatal("the pasted key was revoked without being asked")
	}
	d := details(body)
	for _, want := range []string{"still works", "previous read-only key", "previous stream-control key"} {
		if !strings.Contains(d, want) {
			t.Fatalf("details missing %q:\n%s", want, d)
		}
	}
	ks.assertNoLeaks(t, body)
}

func TestKeySetupSavesAlreadyScopedKey(t *testing.T) {
	ks := newKeySetupHarness(t, nil)
	ks.silo.add(7, "Made by hand", testScopedKey, scopeMonitorRead)
	status, body := ks.setup(t, asPrimary, map[string]any{"key": testScopedKey, "revoke": true, "control": true})
	if status != http.StatusOK || body["saved"] != true || body["level"] != "warn" {
		t.Fatalf("setup: %d %v", status, body)
	}
	if got := ks.lastSaved(t)["monitor_api_key"]; got != testScopedKey {
		t.Fatalf("saved monitor key = %v", got)
	}
	if ks.silo.count("POST") != 0 || ks.silo.count("DELETE") != 0 || ks.silo.key(testScopedKey) == nil {
		t.Fatalf("an already scoped key must not create or revoke anything: %v", ks.silo.events())
	}
	ks.assertNoLeaks(t, body)
}

func TestKeySetupRejectsUnusableKeys(t *testing.T) {
	cases := []struct {
		name, key, code string
		status          int
		callsSilo       bool
	}{
		{"not a Silo key", "hunter2", "invalid", http.StatusUnprocessableEntity, false},
		{"blank", "  ", "invalid", http.StatusUnprocessableEntity, false},
		{"unknown or revoked", "sa_gone0007" + strings.Repeat("0", 56), "key_rejected", http.StatusUnprocessableEntity, true},
		{"non-admin account", testNonAdminKey, "key_not_full_access", http.StatusUnprocessableEntity, true},
		{"other scope", testControlOnly, "key_not_full_access", http.StatusUnprocessableEntity, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ks := newKeySetupHarness(t, nil)
			ks.silo.add(8, "Kid's key", testNonAdminKey)
			ks.silo.add(7, "Stop streams", testControlOnly, scopeSessionsControl)
			status, body := ks.setup(t, asPrimary, map[string]any{"key": c.key, "revoke": true})
			if status != c.status || body["code"] != c.code {
				t.Fatalf("setup: %d %v, want %d %s", status, body, c.status, c.code)
			}
			if (len(ks.silo.events()) > 0) != c.callsSilo {
				t.Fatalf("Silo calls: %v", ks.silo.events())
			}
			if ks.silo.count("POST") != 0 || ks.silo.count("DELETE") != 0 || ks.silo.count("SAVE") != 0 {
				t.Fatalf("a refused key changed something: %v", ks.silo.events())
			}
			if ks.plugin.config().MonitorAPIKey != "" {
				t.Fatal("running config changed")
			}
			ks.assertNoLeaks(t, body)
		})
	}
}

func TestKeySetupCreateFailure(t *testing.T) {
	ks := newKeySetupHarness(t, nil)
	ks.silo.failCreate[scopeMonitorRead] = http.StatusUnprocessableEntity
	status, body := ks.setup(t, asPrimary, map[string]any{"key": testFullKey, "revoke": true})
	if status != http.StatusBadGateway || body["code"] != "create_failed" {
		t.Fatalf("setup: %d %v", status, body)
	}
	msg := body["message"].(string)
	if !strings.Contains(msg, "Owner keys can only be created by the Owner.") || !strings.Contains(msg, "Nothing was changed") {
		t.Fatalf("message = %q", msg)
	}
	if ks.silo.count("SAVE") != 0 || ks.silo.count("DELETE") != 0 || ks.silo.key(testFullKey) == nil {
		t.Fatalf("a failed create changed something: %v", ks.silo.events())
	}
	ks.assertNoLeaks(t, body)
}

// Silo may create a key and still answer too slowly. The message must not
// claim nothing was changed.
func TestKeySetupCreateTimeout(t *testing.T) {
	ks := newKeySetupHarness(t, nil)
	ks.silo.createDelay = 5 * time.Second
	ks.plugin.keys.timeout = 150 * time.Millisecond
	status, body := ks.setup(t, asPrimary, map[string]any{"key": testFullKey, "revoke": true})
	if status != http.StatusBadGateway || body["code"] != "create_failed" {
		t.Fatalf("setup: %d %v", status, body)
	}
	msg := body["message"].(string)
	if !strings.Contains(msg, "took too long") || !strings.Contains(msg, "“Pushover read-only” key may be left under Admin > API Keys") {
		t.Fatalf("message = %q", msg)
	}
	if strings.Contains(msg, "Nothing was changed") {
		t.Fatalf("message claims nothing changed: %q", msg)
	}
	if ks.silo.count("SAVE") != 0 || ks.silo.count("DELETE") != 0 || ks.silo.key(testFullKey) == nil {
		t.Fatalf("a timed-out create changed something: %v", ks.silo.events())
	}
	ks.assertNoLeaks(t, body)
}

func TestKeySetupVerifyFailure(t *testing.T) {
	ks := newKeySetupHarness(t, nil)
	ks.silo.rejectCreated = true
	status, body := ks.setup(t, asPrimary, map[string]any{"key": testFullKey, "revoke": true, "control": true})
	if status != http.StatusBadGateway || body["code"] != "verify_failed" {
		t.Fatalf("setup: %d %v", status, body)
	}
	// Both new keys are removed again; the pasted key and config stay.
	if n := len(ks.silo.createdKeys()); n != 0 {
		t.Fatalf("%d created keys left behind", n)
	}
	if ks.silo.count("SAVE") != 0 || ks.silo.key(testFullKey) == nil || ks.plugin.config().MonitorAPIKey != "" {
		t.Fatalf("a failed check changed something: %v", ks.silo.events())
	}
	ks.assertNoLeaks(t, body)
}

func TestKeySetupSaveFailure(t *testing.T) {
	ks := newKeySetupHarness(t, nil)
	ks.saveErr = errors.New("host connection lost")
	status, body := ks.setup(t, asPrimary, map[string]any{"key": testFullKey, "revoke": true, "control": true})
	if status != http.StatusInternalServerError || body["code"] != "save_failed" {
		t.Fatalf("setup: %d %v", status, body)
	}
	if n := len(ks.silo.createdKeys()); n != 0 {
		t.Fatalf("%d created keys left behind after the save failed", n)
	}
	if ks.silo.key(testFullKey) == nil {
		t.Fatal("the pasted key was revoked although nothing was saved")
	}
	if cfg := ks.plugin.config(); cfg.MonitorAPIKey != "" || cfg.ControlAPIKey != "" || cfg.DefaultAppToken != testAppToken {
		t.Fatalf("running config changed: %v", cfg)
	}
	if !strings.Contains(ks.logs.String(), "host connection lost") {
		t.Fatal("the save error wasn't logged")
	}
	ks.assertNoLeaks(t, body)
}

func TestKeySetupRevokeFailure(t *testing.T) {
	ks := newKeySetupHarness(t, nil)
	ks.silo.failDelete = http.StatusInternalServerError
	status, body := ks.setup(t, asPrimary, map[string]any{"key": testFullKey, "revoke": true})
	if status != http.StatusOK || body["saved"] != true || body["level"] != "warn" {
		t.Fatalf("setup: %d %v", status, body)
	}
	if d := details(body); !strings.Contains(d, "couldn't be revoked") || !strings.Contains(d, apiKeysPage) || !strings.Contains(d, "“Claude”") {
		t.Fatalf("details:\n%s", d)
	}
	if ks.lastSaved(t)["monitor_api_key"] == nil || ks.silo.key(testFullKey) == nil {
		t.Fatal("want the key saved and the pasted key still present")
	}
	ks.assertNoLeaks(t, body)
}

func TestKeySetupAmbiguousPrefixIsNotRevoked(t *testing.T) {
	ks := newKeySetupHarness(t, nil)
	twin := testFullKey[:11] + strings.Repeat("z", 56)
	ks.silo.add(7, "Twin", twin)
	status, body := ks.setup(t, asPrimary, map[string]any{"key": testFullKey, "revoke": true})
	if status != http.StatusOK || body["saved"] != true || body["level"] != "warn" {
		t.Fatalf("setup: %d %v", status, body)
	}
	if d := details(body); !strings.Contains(d, "2 keys") {
		t.Fatalf("details:\n%s", d)
	}
	if ks.silo.count("DELETE") != 0 || ks.silo.key(testFullKey) == nil || ks.silo.key(twin) == nil {
		t.Fatalf("an ambiguous key was revoked: %v", ks.silo.events())
	}
	ks.assertNoLeaks(t, body)
}

func TestKeySetupControlFailureKeepsPastedKey(t *testing.T) {
	ks := newKeySetupHarness(t, nil)
	ks.silo.failCreate[scopeSessionsControl] = http.StatusInternalServerError
	status, body := ks.setup(t, asPrimary, map[string]any{"key": testFullKey, "revoke": true, "control": true})
	if status != http.StatusOK || body["saved"] != true || body["level"] != "warn" {
		t.Fatalf("setup: %d %v", status, body)
	}
	saved := ks.lastSaved(t)
	if saved["monitor_api_key"] == nil || saved["control_api_key"] != nil {
		t.Fatalf("saved config = %v", saved)
	}
	if ks.silo.key(testFullKey) == nil || !strings.Contains(details(body), "try the stream-control key again") {
		t.Fatalf("the pasted key should be kept for a retry: %v", body)
	}
	ks.assertNoLeaks(t, body)
}

func TestKeySetupReplacesLegacyKey(t *testing.T) {
	ks := newKeySetupHarness(t, map[string]any{"silo_api_key": testLegacyFull, "default_app_token": testAppToken})
	ks.silo.add(7, "Pushover", testLegacyFull)

	_, state := ks.request(t, http.MethodGet, "/api/state", asPrimary, nil)
	if state["admin_keys"].(map[string]any)["legacy_key_stored"] != true {
		t.Fatalf("state should offer the one-click replacement: %v", state)
	}
	// One click always revokes the old key, whatever "revoke" says.
	status, body := ks.setup(t, asPrimary, map[string]any{"source": "legacy", "revoke": false})
	if status != http.StatusOK || body["saved"] != true || body["level"] != "ok" {
		t.Fatalf("setup: %d %v", status, body)
	}
	if ks.silo.key(testLegacyFull) != nil {
		t.Fatal("the 0.2.0 key wasn't revoked")
	}
	saved := ks.lastSaved(t)
	if _, ok := saved[legacyAPIKeyField]; ok || saved["monitor_api_key"] == nil || saved["default_app_token"] != testAppToken {
		t.Fatalf("saved config = %v", saved)
	}
	if cfg := ks.plugin.config(); cfg.LegacyKeyStored || cfg.legacyAPIKey != "" {
		t.Fatal("the 0.2.0 key is still held in memory")
	}
	_, state = ks.request(t, http.MethodGet, "/api/state", asPrimary, nil)
	if keys := state["admin_keys"].(map[string]any); keys["legacy_key_stored"] != false || keys["monitor_key_set"] != true {
		t.Fatalf("state after replacing: %v", keys)
	}
	// Nothing is left to replace.
	if status, body := ks.setup(t, asPrimary, map[string]any{"source": "legacy"}); status != http.StatusConflict {
		t.Fatalf("second one-click: %d %v", status, body)
	}
	ks.assertNoLeaks(t, body, state)
}

func TestKeySetupForgetsRejectedLegacyKey(t *testing.T) {
	// Silo no longer knows the 0.2.0 key (revoked by hand).
	ks := newKeySetupHarness(t, map[string]any{"silo_api_key": testLegacyFull, "default_app_token": testAppToken})
	status, body := ks.setup(t, asPrimary, map[string]any{"source": "legacy"})
	if status != http.StatusOK || body["saved"] != false || body["level"] != "warn" {
		t.Fatalf("setup: %d %v", status, body)
	}
	saved := ks.lastSaved(t)
	if want := map[string]any{"default_app_token": testAppToken}; !maps.Equal(saved, want) {
		t.Fatalf("saved config = %v, want %v", saved, want)
	}
	if ks.plugin.config().LegacyKeyStored || ks.silo.count("POST") != 0 {
		t.Fatal("want the old key forgotten and nothing created")
	}
	ks.assertNoLeaks(t, body)
}

func TestKeySetupPasteWhileLegacyStored(t *testing.T) {
	ks := newKeySetupHarness(t, map[string]any{"silo_api_key": testLegacyFull, "default_app_token": testAppToken})
	ks.silo.add(7, "Pushover", testLegacyFull)
	status, body := ks.setup(t, asPrimary, map[string]any{"key": testFullKey, "revoke": true})
	if status != http.StatusOK || body["level"] != "warn" {
		t.Fatalf("setup: %d %v", status, body)
	}
	// The saved row drops silo_api_key, and the page says the old key still
	// works in Silo.
	if _, ok := ks.lastSaved(t)[legacyAPIKeyField]; ok {
		t.Fatal("silo_api_key was carried over")
	}
	if !strings.Contains(details(body), "version 0.2.0") || ks.silo.key(testLegacyFull) == nil {
		t.Fatalf("details:\n%s", details(body))
	}
	ks.assertNoLeaks(t, body)
}

func TestKeySetupNeedsActingAdmin(t *testing.T) {
	ks := newKeySetupHarness(t, nil)
	body := map[string]any{"key": testFullKey, "revoke": true}
	for _, as := range []viewerAs{asKid, {profileID: "p1", primary: true}} {
		if status, out := ks.setup(t, as, body); status != http.StatusForbidden {
			t.Fatalf("%+v: status %d %v, want 403", as, status, out)
		}
		_, state := ks.request(t, http.MethodGet, "/api/state", as, nil)
		if state["admin_keys"] != nil {
			t.Fatalf("%+v sees admin key status", as)
		}
	}

	// Only a JSON POST reaches the setup.
	headers := asPrimary.headers()
	headers["Content-Type"] = "application/x-www-form-urlencoded"
	raw, _ := json.Marshal(body)
	resp, _ := ks.routes.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: http.MethodPost, Path: "/api/admin/key-setup", Headers: headers, Body: raw})
	if resp.GetStatusCode() != http.StatusUnsupportedMediaType {
		t.Fatalf("form post: %d, want 415", resp.GetStatusCode())
	}
	if status, _ := ks.request(t, http.MethodGet, "/api/admin/key-setup", asPrimary, nil); status != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d, want 405", status)
	}
	if n := len(ks.silo.events()); n != 0 {
		t.Fatalf("Silo was called %d times", n)
	}

	// Before Silo has configured the plugin, it can't write the config back.
	fresh := &keySetupHarness{harness: newHarness(t), silo: ks.silo}
	fresh.plugin.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if status, _ := fresh.setup(t, asPrimary, body); status != http.StatusServiceUnavailable {
		t.Fatalf("before Configure: %d, want 503", status)
	}
}

func TestAdminConfigFormattingHidesKeys(t *testing.T) {
	cfg := adminConfig{MonitorAPIKey: testScopedKey, ControlAPIKey: testControlOnly, DefaultAppToken: testAppToken, legacyAPIKey: testLegacyFull, LegacyKeyStored: true}
	var logs strings.Builder
	slog.New(slog.NewTextHandler(&logs, nil)).Info("x", "config", cfg)
	out := fmt.Sprintf("%v %+v %#v %s", cfg, cfg, cfg, cfg) + logs.String()
	for _, secret := range []string{testScopedKey, testControlOnly, testAppToken, testLegacyFull} {
		if strings.Contains(out, secret) {
			t.Fatalf("formatted config contains a key: %s", out)
		}
	}
}
