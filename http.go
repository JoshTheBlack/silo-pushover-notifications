package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

type routesServer struct {
	pluginv1.UnimplementedHttpRoutesServer
	plugin *Plugin
}

func (s *routesServer) Handle(ctx context.Context, req *pluginv1.HandleHTTPRequest) (*pluginv1.HandleHTTPResponse, error) {
	path := req.GetPath()
	switch {
	case path == "" || path == "/" || path == "/index.html":
		if req.GetMethod() != http.MethodGet {
			return errorResponse(apiErr(http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed.")), nil
		}
		return serveIndex(req), nil
	case strings.HasPrefix(path, "/api/"):
		return s.plugin.serveAPI(ctx, req), nil
	}
	return errorResponse(apiErr(http.StatusNotFound, "not_found", "Not found.")), nil
}

// serveIndex returns the settings page, pre-set to the viewer's Silo theme so
// it doesn't flash the wrong colours.
func serveIndex(req *pluginv1.HandleHTTPRequest) *pluginv1.HandleHTTPResponse {
	theme := "auto"
	if t := strings.ToLower(header(req, "X-Silo-Theme")); t != "" {
		theme = "dark"
		if strings.Contains(t, "light") {
			theme = "light"
		}
	}
	page := bytes.Replace(indexHTML, []byte(`data-theme="auto"`), []byte(`data-theme="`+theme+`"`), 1)
	return &pluginv1.HandleHTTPResponse{
		StatusCode: http.StatusOK,
		Headers:    map[string]string{"Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store"},
		Body:       page,
	}
}

// viewer is the signed-in account and profile as Silo's plugin proxy reports
// them, plus the profiles it may configure. A secondary profile manages only
// itself. The primary profile manages every profile on the account when the
// read-only Silo key works, and otherwise itself plus the account's profiles
// that already have Pushover settings.
type viewer struct {
	userID   int
	activeID string
	primary  bool
	// admin is Silo's "acting admin": an admin account on its primary profile.
	admin    bool
	profiles []siloProfile
	// household is how the primary's list was built: "all" (from Silo),
	// "configured" (saved settings only) or "self".
	household string
	// notice explains a limited household list, for the primary profile.
	notice *pageNotice
}

// pageNotice is a setup message shown at the top of the page.
type pageNotice struct {
	Code    string `json:"code"`
	Level   string `json:"level"` // "info" or "warn"
	Title   string `json:"title"`
	Message string `json:"message"`
}

func (v *viewer) profile(id string) (siloProfile, bool) {
	for _, p := range v.profiles {
		if p.ID == id {
			return p, true
		}
	}
	return siloProfile{}, false
}

type apiError struct {
	status  int
	Code    string `json:"code"`
	Message string `json:"message"`
}

func apiErr(status int, code, message string) *apiError {
	return &apiError{status: status, Code: code, Message: message}
}

const adminFormPath = "Admin > Plugins > Pushover Notifications"

// resolveViewer identifies the viewer from the headers Silo's plugin proxy
// sets. The proxy drops any the browser sends, verifies that the profile
// belongs to the account (PIN included), and is the only way in, so the
// headers can be trusted.
func (p *Plugin) resolveViewer(ctx context.Context, req *pluginv1.HandleHTTPRequest) (*viewer, *apiError) {
	userID, _ := strconv.Atoi(header(req, "X-Silo-User-Id"))
	if userID <= 0 {
		return nil, apiErr(http.StatusUnauthorized, "unauthenticated", "Your session for this page has expired. Open Pushover from the Silo sidebar again.")
	}
	v := &viewer{
		userID:    userID,
		activeID:  header(req, "X-Silo-Profile-Id"),
		primary:   header(req, "X-Silo-Profile-Primary") == "true",
		admin:     header(req, "X-Silo-User-Role") == "admin",
		household: "self",
	}
	if v.activeID == "" {
		return nil, apiErr(http.StatusConflict, "no_profile", "Choose a profile in Silo, then open Pushover from the sidebar again.")
	}

	saved, err := p.store.ListByUser(ctx, userID)
	if err != nil {
		if errors.Is(err, errHostUnavailable) {
			return nil, apiErr(http.StatusServiceUnavailable, "host_unavailable", "The plugin is still starting. Try again in a moment.")
		}
		p.logger.Error("settings storage failed", "error", err)
		return nil, apiErr(http.StatusInternalServerError, "storage_failed", "Couldn't read saved settings.")
	}
	savedNames := map[string]string{}
	for _, s := range saved {
		savedNames[s.ProfileID] = s.ProfileName
	}

	active := siloProfile{ID: v.activeID, Name: header(req, "X-Silo-Profile-Name")}
	if active.Name == "" {
		active.Name = savedNames[v.activeID]
	}
	if active.Name == "" {
		active.Name = "Your profile"
	}
	v.profiles = []siloProfile{active}
	if !v.primary {
		return v, nil
	}

	household, err := p.accountProfiles(ctx, userID)
	if err == nil {
		v.household = "all"
		for _, prof := range household {
			if prof.ID == "" || prof.ID == v.activeID {
				continue
			}
			if prof.Name == "" {
				prof.Name = savedNames[prof.ID]
			}
			v.profiles = append(v.profiles, prof)
		}
		return v, nil
	}

	// Without a working key, add the account's profiles that already have
	// settings: their IDs were saved from these same trusted headers.
	v.household = "configured"
	for _, s := range saved {
		if s.ProfileID != v.activeID {
			v.profiles = append(v.profiles, siloProfile{ID: s.ProfileID, Name: s.ProfileName})
		}
	}
	v.notice = householdNotice(err, v.admin)
	if !errors.Is(err, errSiloKeyMissing) {
		p.logger.Warn("could not list the account's profiles with the read-only Silo key", "user_id", userID, "error", err)
	}
	return v, nil
}

// accountProfiles lists an account's profiles with the read-only key.
func (p *Plugin) accountProfiles(ctx context.Context, userID int) ([]siloProfile, error) {
	key := p.config().MonitorAPIKey
	if key == "" {
		return nil, errSiloKeyMissing
	}
	info, err := p.host(ctx)
	if err != nil {
		return nil, err
	}
	return p.silo.AccountProfiles(ctx, strings.TrimRight(info.InternalBaseURL, "/"), key, userID)
}

// householdNotice explains why the primary profile sees only some profiles.
// Admins get the fix; other accounts are told to ask an admin.
func householdNotice(err error, admin bool) *pageNotice {
	n := &pageNotice{Level: "warn", Title: "Some profiles may be missing."}
	switch {
	case errors.Is(err, errSiloKeyMissing):
		n.Code, n.Level, n.Title = "monitor_key_missing", "info", "Other profiles show up once they set up Pushover."
		n.Message = "Each profile on your account can set up Pushover by opening this page while signed in to that profile."
		if admin {
			n.Message += " To manage every profile from here instead, set up a read-only Silo key (scope admin:monitor:read) in the admin section below, or add one under " + adminFormPath + "."
		}
	case errors.Is(err, errSiloKeyRejected):
		n.Code = "monitor_key_rejected"
		n.Message = "Silo rejected the plugin's read-only Silo key, so only profiles that already set up Pushover are listed."
		if admin {
			n.Message += " The key may have been revoked or pasted wrong. Set up a new one in the admin section below, or replace it under " + adminFormPath + "."
		} else {
			n.Message += " Let your server admin know."
		}
	case errors.Is(err, errSiloKeyScope):
		n.Code = "monitor_key_scope"
		n.Message = "The plugin's read-only Silo key isn't allowed to list profiles, so only profiles that already set up Pushover are listed."
		if admin {
			n.Message += " Set up a new key in the admin section below, or use a key with the admin:monitor:read scope, created on an admin account, under " + adminFormPath + "."
		} else {
			n.Message += " Let your server admin know."
		}
	default:
		n.Code = "silo_unavailable"
		n.Message = "Couldn't reach Silo to list your account's profiles, so only profiles that already set up Pushover are listed. Reload the page to try again."
	}
	return n
}

func (p *Plugin) serveAPI(ctx context.Context, req *pluginv1.HandleHTTPRequest) *pluginv1.HandleHTTPResponse {
	method := req.GetMethod()
	if (method == http.MethodPost || method == http.MethodPut) &&
		!strings.HasPrefix(strings.ToLower(header(req, "Content-Type")), "application/json") {
		// Requiring JSON keeps other sites from posting forms here with the
		// viewer's cookie.
		return errorResponse(apiErr(http.StatusUnsupportedMediaType, "unsupported_media_type", "Expected a JSON body."))
	}

	parts := strings.Split(strings.Trim(strings.TrimPrefix(req.GetPath(), "/api/"), "/"), "/")
	if len(parts) == 1 && parts[0] == "state" && method == http.MethodGet {
		return p.handleState(ctx, req)
	}
	if len(parts) == 2 && parts[0] == "admin" && parts[1] == "key-setup" {
		if method != http.MethodPost {
			return errorResponse(apiErr(http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed."))
		}
		return p.handleKeySetup(ctx, req)
	}
	if len(parts) >= 2 && parts[0] == "admin" {
		return p.serveAdminAPI(ctx, req, strings.Join(parts[1:], "/"))
	}
	if len(parts) < 2 || parts[0] != "profiles" {
		return errorResponse(apiErr(http.StatusNotFound, "not_found", "Not found."))
	}

	v, aerr := p.resolveViewer(ctx, req)
	if aerr != nil {
		return errorResponse(aerr)
	}
	prof, ok := v.profile(parts[1])
	if !ok {
		return errorResponse(apiErr(http.StatusNotFound, "not_found", "That profile isn't one you can manage."))
	}

	switch {
	case len(parts) == 2 && method == http.MethodGet:
		return p.handleGetProfile(ctx, prof)
	case len(parts) == 2 && method == http.MethodPut:
		return p.handlePutProfile(ctx, v, prof, req.GetBody())
	case len(parts) == 2 && method == http.MethodDelete:
		if err := p.store.Delete(ctx, prof.ID); err != nil {
			return storeError(p, err)
		}
		return jsonResponse(http.StatusOK, map[string]bool{"ok": true})
	case len(parts) == 3 && parts[2] == "test" && method == http.MethodPost:
		return p.handleTest(ctx, v, prof, req.GetBody())
	case len(parts) == 3 && parts[2] == "validate" && method == http.MethodPost:
		return p.handleValidate(ctx, prof, req.GetBody())
	}
	return errorResponse(apiErr(http.StatusNotFound, "not_found", "Not found."))
}

type profileSummary struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Active     bool   `json:"active"`
	Configured bool   `json:"configured"`
	Enabled    bool   `json:"enabled"`
}

// handleState answers the page's first request. It reports setup problems as
// data rather than an error so the page can explain them.
func (p *Plugin) handleState(ctx context.Context, req *pluginv1.HandleHTTPRequest) *pluginv1.HandleHTTPResponse {
	state := map[string]any{
		"types":             notificationTypes,
		"sounds":            builtinSounds,
		"has_default_token": p.config().DefaultAppToken != "",
	}
	v, aerr := p.resolveViewer(ctx, req)
	if aerr != nil {
		if aerr.status == http.StatusUnauthorized {
			return errorResponse(aerr)
		}
		state["problem"] = aerr
		return jsonResponse(http.StatusOK, state)
	}
	profiles := make([]profileSummary, 0, len(v.profiles))
	for _, prof := range v.profiles {
		summary := profileSummary{ID: prof.ID, Name: prof.Name, Active: prof.ID == v.activeID}
		settings, found, err := p.store.Get(ctx, prof.ID)
		if err != nil {
			return storeError(p, err)
		}
		if found {
			summary.Configured, summary.Enabled = true, settings.Enabled
		}
		profiles = append(profiles, summary)
	}
	state["profiles"] = profiles
	state["household"] = v.household
	// admin is Silo's acting admin: the page shows the admin section, whose
	// alerts live under /api/admin/.
	state["admin"] = v.admin
	if v.admin {
		state["admin_keys"] = p.adminKeyStatus()
	}
	if v.notice != nil {
		state["notice"] = v.notice
	}
	return jsonResponse(http.StatusOK, state)
}

// profileView is what the page sees. Keys are never sent back, only a hint.
type profileView struct {
	ProfileID    string                  `json:"profile_id"`
	ProfileName  string                  `json:"profile_name"`
	Configured   bool                    `json:"configured"`
	Enabled      bool                    `json:"enabled"`
	UserKeyHint  string                  `json:"user_key_hint"`
	AppTokenHint string                  `json:"app_token_hint"`
	Device       string                  `json:"device"`
	Sound        string                  `json:"sound"`
	Priority     int                     `json:"priority"`
	HTML         bool                    `json:"html"`
	AttachPoster bool                    `json:"attach_poster"`
	IncludeLink  bool                    `json:"include_link"`
	Types        map[string]TypeSettings `json:"types"`
}

func newProfileView(prof siloProfile, s *ProfileSettings) profileView {
	view := profileView{
		ProfileID: prof.ID, ProfileName: prof.Name,
		Enabled: true, AttachPoster: true, IncludeLink: true,
		Types: map[string]TypeSettings{},
	}
	if s != nil {
		view.Configured = true
		view.Enabled = s.Enabled
		view.UserKeyHint = keyHint(s.UserKey)
		view.AppTokenHint = keyHint(s.AppToken)
		view.Device, view.Sound, view.Priority = s.Device, s.Sound, s.Priority
		view.HTML, view.AttachPoster, view.IncludeLink = s.HTML, s.AttachPoster, s.IncludeLink
	}
	for _, t := range notificationTypes {
		if s != nil {
			view.Types[t.Key] = s.typeSettings(t.Key)
		} else {
			view.Types[t.Key] = t.defaults()
		}
	}
	return view
}

func keyHint(key string) string {
	if len(key) < 8 {
		return ""
	}
	return key[:4] + "…" + key[len(key)-4:]
}

func (p *Plugin) handleGetProfile(ctx context.Context, prof siloProfile) *pluginv1.HandleHTTPResponse {
	settings, _, err := p.store.Get(ctx, prof.ID)
	if err != nil {
		return storeError(p, err)
	}
	return jsonResponse(http.StatusOK, newProfileView(prof, settings))
}

// profileInput is a save or test request. Empty keys keep the stored ones.
type profileInput struct {
	Enabled       bool                    `json:"enabled"`
	UserKey       string                  `json:"user_key"`
	AppToken      string                  `json:"app_token"`
	ClearAppToken bool                    `json:"clear_app_token"`
	Device        string                  `json:"device"`
	Sound         string                  `json:"sound"`
	Priority      int                     `json:"priority"`
	HTML          bool                    `json:"html"`
	AttachPoster  bool                    `json:"attach_poster"`
	IncludeLink   bool                    `json:"include_link"`
	Types         map[string]TypeSettings `json:"types"`
}

// applyInput merges input over the stored settings and validates the result.
func (p *Plugin) applyInput(v *viewer, prof siloProfile, existing *ProfileSettings, in profileInput) (*ProfileSettings, *apiError) {
	s := &ProfileSettings{}
	if existing != nil {
		s = existing.clone()
	}
	s.ProfileID, s.ProfileName, s.UserID = prof.ID, prof.Name, v.userID
	s.Enabled = in.Enabled
	if k := strings.TrimSpace(in.UserKey); k != "" {
		s.UserKey = k
	}
	switch {
	case in.ClearAppToken:
		s.AppToken = ""
	case strings.TrimSpace(in.AppToken) != "":
		s.AppToken = strings.TrimSpace(in.AppToken)
	}
	s.Device = strings.TrimSpace(in.Device)
	s.Sound = strings.TrimSpace(in.Sound)
	s.Priority = in.Priority
	s.HTML, s.AttachPoster, s.IncludeLink = in.HTML, in.AttachPoster, in.IncludeLink

	bad := func(msg string) *apiError { return apiErr(http.StatusUnprocessableEntity, "invalid", msg) }
	switch {
	case s.UserKey == "":
		return nil, bad("Enter your Pushover user key. It's shown at the top of pushover.net after you sign in.")
	case !pushoverKeyPattern.MatchString(s.UserKey):
		return nil, bad("A Pushover user key is 30 letters and numbers.")
	case s.AppToken != "" && !pushoverKeyPattern.MatchString(s.AppToken):
		return nil, bad("A Pushover application token is 30 letters and numbers.")
	case effectiveToken(s, p.config()) == "":
		return nil, bad("This server has no shared Pushover application. Create one at pushover.net/apps and enter its API token.")
	case !pushoverDevicePattern.MatchString(s.Device):
		return nil, bad("Device names can only use letters, numbers, dashes and underscores.")
	case !pushoverSoundPattern.MatchString(s.Sound):
		return nil, bad("That sound name isn't valid.")
	case !validPriority(s.Priority):
		return nil, bad("Priority must be between -2 and 1.")
	}

	s.Types = map[string]TypeSettings{}
	for _, t := range notificationTypes {
		ts, ok := in.Types[t.Key]
		if !ok {
			ts = t.defaults()
		}
		ts.Title, ts.Message, ts.Sound = strings.TrimSpace(ts.Title), strings.TrimSpace(ts.Message), strings.TrimSpace(ts.Sound)
		switch {
		case len(ts.Title) > 1024 || len(ts.Message) > 2048:
			return nil, bad(t.Label + ": the template is too long.")
		case ts.Priority != nil && !validPriority(*ts.Priority):
			return nil, bad(t.Label + ": priority must be between -2 and 1.")
		case !pushoverSoundPattern.MatchString(ts.Sound):
			return nil, bad(t.Label + ": that sound name isn't valid.")
		}
		if ts.Title == "" && ts.Message == "" {
			ts.Title, ts.Message = t.DefaultTitle, t.DefaultMessage
		}
		s.Types[t.Key] = ts
	}
	s.UpdatedAt = time.Now().UTC()
	return s, nil
}

// Priority 2 (emergency) needs acknowledgement handling this plugin doesn't do.
func validPriority(p int) bool { return p >= -2 && p <= 1 }

func (p *Plugin) handlePutProfile(ctx context.Context, v *viewer, prof siloProfile, body []byte) *pluginv1.HandleHTTPResponse {
	var in profileInput
	if err := json.Unmarshal(body, &in); err != nil {
		return errorResponse(apiErr(http.StatusBadRequest, "bad_request", "Couldn't read the settings."))
	}
	existing, _, err := p.store.Get(ctx, prof.ID)
	if err != nil {
		return storeError(p, err)
	}
	s, aerr := p.applyInput(v, prof, existing, in)
	if aerr != nil {
		return errorResponse(aerr)
	}

	// Check new credentials with Pushover. If Pushover can't be reached, save
	// anyway and say so rather than blocking the change.
	warning := ""
	keysChanged := existing == nil || existing.UserKey != s.UserKey || effectiveToken(existing, p.config()) != effectiveToken(s, p.config())
	if keysChanged {
		if _, _, err := p.pushover.Validate(ctx, effectiveToken(s, p.config()), s.UserKey); err != nil {
			var perr *pushoverError
			if errors.As(err, &perr) && !perr.Retryable && perr.HTTPStatus != 0 {
				return errorResponse(apiErr(http.StatusUnprocessableEntity, "pushover_rejected", err.Error()))
			}
			warning = "Saved, but Pushover couldn't be reached to check your key."
		}
	}

	if err := p.store.Put(ctx, s); err != nil {
		return storeError(p, err)
	}
	return jsonResponse(http.StatusOK, map[string]any{"profile": newProfileView(prof, s), "warning": warning})
}

type testInput struct {
	Settings profileInput `json:"settings"`
	Type     string       `json:"type"`
}

// handleTest sends a sample of one notification type using the unsaved form
// values, so wording can be tried before saving.
func (p *Plugin) handleTest(ctx context.Context, v *viewer, prof siloProfile, body []byte) *pluginv1.HandleHTTPResponse {
	var in testInput
	if err := json.Unmarshal(body, &in); err != nil {
		return errorResponse(apiErr(http.StatusBadRequest, "bad_request", "Couldn't read the test request."))
	}
	existing, _, err := p.store.Get(ctx, prof.ID)
	if err != nil {
		return storeError(p, err)
	}
	s, aerr := p.applyInput(v, prof, existing, in.Settings)
	if aerr != nil {
		return errorResponse(aerr)
	}

	t := notificationTypeByKey(in.Type)
	ts := s.typeSettings(t.Key)
	vars := map[string]string{"profile": s.ProfileName, "type": t.Key, "type_label": t.Label}
	for k, val := range t.Sample {
		vars[k] = val
	}
	msg := pushoverMessage{
		Token:    effectiveToken(s, p.config()),
		User:     s.UserKey,
		Title:    "[Test] " + renderTemplate(ts.Title, vars, s.HTML),
		Message:  renderTemplate(ts.Message, vars, s.HTML),
		Device:   s.Device,
		Sound:    s.Sound,
		Priority: s.Priority,
		HTML:     s.HTML,
	}
	if ts.Priority != nil {
		msg.Priority = *ts.Priority
	}
	if ts.Sound != "" {
		msg.Sound = ts.Sound
	}
	if s.IncludeLink {
		if base := p.publicBaseURL(ctx); base != "" {
			msg.URL, msg.URLTitle = base, "Open in Silo"
		}
	}
	if err := p.pushover.Send(ctx, msg); err != nil {
		return errorResponse(apiErr(http.StatusBadGateway, "pushover_failed", err.Error()))
	}
	return jsonResponse(http.StatusOK, map[string]bool{"ok": true})
}

type validateInput struct {
	UserKey  string `json:"user_key"`
	AppToken string `json:"app_token"`
	// ClearAppToken checks against the shared token, as saving would.
	ClearAppToken bool `json:"clear_app_token"`
}

// handleValidate checks the key pair and returns the account's devices and
// sounds for the pickers.
func (p *Plugin) handleValidate(ctx context.Context, prof siloProfile, body []byte) *pluginv1.HandleHTTPResponse {
	var in validateInput
	_ = json.Unmarshal(body, &in)
	existing, _, err := p.store.Get(ctx, prof.ID)
	if err != nil {
		return storeError(p, err)
	}
	user, token := strings.TrimSpace(in.UserKey), strings.TrimSpace(in.AppToken)
	if existing != nil {
		if user == "" {
			user = existing.UserKey
		}
		if token == "" && !in.ClearAppToken {
			token = existing.AppToken
		}
	}
	if token == "" {
		token = p.config().DefaultAppToken
	}
	if user == "" || token == "" {
		return errorResponse(apiErr(http.StatusUnprocessableEntity, "invalid", "Enter your user key (and an application token if this server has no shared one) first."))
	}
	devices, sounds, err := p.pushover.Validate(ctx, token, user)
	if err != nil {
		return errorResponse(apiErr(http.StatusUnprocessableEntity, "pushover_rejected", err.Error()))
	}
	if devices == nil {
		devices = []string{}
	}
	return jsonResponse(http.StatusOK, map[string]any{"devices": devices, "sounds": sounds})
}

// adminViewer is an acting admin whose read-only key check passed.
type adminViewer struct {
	userID    int
	profileID string
	username  string
	base      string // Silo's internal URL
	key       string // the read-only key
}

// adminRoutes lists the admin alert routes under /api/admin/ and their
// methods. POST and PUT bodies are JSON (checked in serveAPI).
var adminRoutes = map[string][]string{
	"alerts":      {http.MethodGet, http.MethodPut, http.MethodDelete},
	"alerts/test": {http.MethodPost},
	"users":       {http.MethodGet},
}

// serveAdminAPI answers the admin alert routes. Each needs an acting admin
// and a working read-only Silo key; without the key it says so.
func (p *Plugin) serveAdminAPI(ctx context.Context, req *pluginv1.HandleHTTPRequest, route string) *pluginv1.HandleHTTPResponse {
	methods, ok := adminRoutes[route]
	if !ok {
		return errorResponse(apiErr(http.StatusNotFound, "not_found", "Not found."))
	}
	method := req.GetMethod()
	if !slices.Contains(methods, method) {
		return errorResponse(apiErr(http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed."))
	}
	a, aerr := p.resolveAdmin(ctx, req)
	if aerr != nil {
		return errorResponse(aerr)
	}
	switch route + " " + method {
	case "alerts GET":
		return p.handleGetAdminAlerts(ctx, a)
	case "alerts PUT":
		return p.handlePutAdminAlerts(ctx, a, req.GetBody())
	case "alerts DELETE":
		if err := p.admins.Delete(ctx, a.userID); err != nil {
			return storeError(p, err)
		}
		return jsonResponse(http.StatusOK, map[string]bool{"ok": true})
	case "alerts/test POST":
		return p.handleTestAdminAlert(ctx, a, req.GetBody())
	default: // users GET
		return p.handleListUsers(ctx, a)
	}
}

// resolveAdmin checks that the viewer is an acting admin and that the
// read-only key works, by looking up the viewer's own account with it.
func (p *Plugin) resolveAdmin(ctx context.Context, req *pluginv1.HandleHTTPRequest) (*adminViewer, *apiError) {
	userID, _ := strconv.Atoi(header(req, "X-Silo-User-Id"))
	if userID <= 0 {
		return nil, apiErr(http.StatusUnauthorized, "unauthenticated", "Your session for this page has expired. Open Pushover from the Silo sidebar again.")
	}
	if header(req, "X-Silo-User-Role") != "admin" {
		return nil, apiErr(http.StatusForbidden, "forbidden", "Only a server admin, signed in to their primary profile, can set up admin alerts.")
	}
	a := &adminViewer{userID: userID, profileID: header(req, "X-Silo-Profile-Id"), username: header(req, "X-Silo-User-Name")}
	if a.profileID == "" {
		return nil, apiErr(http.StatusConflict, "no_profile", "Choose a profile in Silo, then open Pushover from the sidebar again.")
	}
	a.key = p.config().MonitorAPIKey
	if a.key == "" {
		return nil, monitorKeyProblem(errSiloKeyMissing)
	}
	info, err := p.host(ctx)
	if err != nil || info.InternalBaseURL == "" {
		return nil, apiErr(http.StatusServiceUnavailable, "host_unavailable", "The plugin is still starting. Try again in a moment.")
	}
	a.base = strings.TrimRight(info.InternalBaseURL, "/")
	user, err := p.silo.User(ctx, a.base, a.key, userID)
	switch {
	case errors.Is(err, errSiloKeyRejected), errors.Is(err, errSiloKeyScope):
		return nil, monitorKeyProblem(err)
	case errors.Is(err, errSiloUserNotFound), err == nil && !user.isAdmin():
		return nil, apiErr(http.StatusForbidden, "forbidden", "Silo doesn't list your account as an enabled admin, so you can't set up admin alerts.")
	case err != nil:
		p.logger.Warn("could not check the admin's account with the read-only Silo key", "user_id", userID, "error", err)
		return nil, apiErr(http.StatusBadGateway, "silo_unavailable", "Couldn't reach Silo to check the read-only key. Reload the page to try again.")
	}
	if a.username == "" {
		a.username = user.Username
	}
	return a, nil
}

// monitorKeyProblem explains why admin alerts can't run without a working
// read-only key. The page shows the key setup box for these codes.
func monitorKeyProblem(err error) *apiError {
	const fix = " Use “Set up the read-only key” in the admin section of this page, or add a key under " + adminFormPath + "."
	switch {
	case errors.Is(err, errSiloKeyRejected):
		return apiErr(http.StatusConflict, "monitor_key_rejected", "Silo rejected the plugin's read-only Silo key, so no admin alerts are sent. The key may have been revoked."+fix)
	case errors.Is(err, errSiloKeyScope):
		return apiErr(http.StatusConflict, "monitor_key_scope", "The plugin's read-only Silo key isn't allowed to look up accounts, so no admin alerts are sent. It needs the admin:monitor:read scope."+fix)
	}
	return apiErr(http.StatusConflict, "monitor_key_missing", "Admin alerts need a read-only Silo key (scope admin:monitor:read), which isn't set up yet. The plugin uses it to check that each recipient is still an admin."+fix)
}

// adminSettingsView fills in every alert type and empty lists, for the page.
func adminSettingsView(s *AdminSettings) *AdminSettings {
	v := s.clone()
	if v.Alerts == nil {
		v.Alerts = map[string]AlertSettings{}
	}
	for _, t := range adminAlertTypes {
		v.Alerts[t.Key] = s.alert(t.Key)
	}
	if v.Filters.IgnoreUsers == nil {
		v.Filters.IgnoreUsers = []string{}
	}
	if v.Filters.MediaTypes == nil {
		v.Filters.MediaTypes = []string{}
	}
	return v
}

// adminPushoverReady reports whether the admin's profile has a Pushover user
// key and a token to send with.
func (p *Plugin) adminPushoverReady(ctx context.Context, profileID string) (*ProfileSettings, bool, error) {
	prof, found, err := p.store.Get(ctx, profileID)
	if err != nil || !found {
		return nil, false, err
	}
	return prof, prof.UserKey != "" && effectiveToken(prof, p.config()) != "", nil
}

const adminPushoverMissing = "Admin alerts are sent with your profile's Pushover settings. Add your Pushover user key in your profile's section of this page first."

func (p *Plugin) handleGetAdminAlerts(ctx context.Context, a *adminViewer) *pluginv1.HandleHTTPResponse {
	settings, found, err := p.admins.Get(ctx, a.userID)
	if err != nil {
		return storeError(p, err)
	}
	if !found {
		settings = defaultAdminSettings()
		settings.UserID, settings.ProfileID, settings.Username = a.userID, a.profileID, a.username
	}
	_, ready, err := p.adminPushoverReady(ctx, a.profileID)
	if err != nil {
		return storeError(p, err)
	}
	out := map[string]any{
		"configured":     found,
		"settings":       adminSettingsView(settings),
		"alert_types":    adminAlertTypes,
		"noise_modes":    noiseModes,
		"media_types":    mediaTypeOptions,
		"sounds":         builtinSounds,
		"pushover_ready": ready,
	}
	if !ready {
		out["pushover_message"] = adminPushoverMissing
	}
	return jsonResponse(http.StatusOK, out)
}

func (p *Plugin) handlePutAdminAlerts(ctx context.Context, a *adminViewer, body []byte) *pluginv1.HandleHTTPResponse {
	var in adminInput
	if err := json.Unmarshal(body, &in); err != nil {
		return errorResponse(apiErr(http.StatusBadRequest, "bad_request", "Couldn't read the settings."))
	}
	s, aerr := applyAdminInput(a.userID, a.profileID, a.username, in)
	if aerr != nil {
		return errorResponse(aerr)
	}
	if err := p.admins.Put(ctx, s); err != nil {
		return storeError(p, err)
	}
	warning := ""
	if _, ready, err := p.adminPushoverReady(ctx, a.profileID); err == nil && !ready && s.anyEnabled() {
		warning = "Saved. " + adminPushoverMissing
	}
	return jsonResponse(http.StatusOK, map[string]any{"settings": adminSettingsView(s), "warning": warning})
}

type adminTestInput struct {
	Settings adminInput `json:"settings"`
	Alert    string     `json:"alert"`
}

// handleTestAdminAlert sends a sample of one alert with the unsaved form
// values, to the admin's profile.
func (p *Plugin) handleTestAdminAlert(ctx context.Context, a *adminViewer, body []byte) *pluginv1.HandleHTTPResponse {
	var in adminTestInput
	if err := json.Unmarshal(body, &in); err != nil {
		return errorResponse(apiErr(http.StatusBadRequest, "bad_request", "Couldn't read the test request."))
	}
	t, ok := adminAlertTypeByKey(in.Alert)
	if !ok {
		return errorResponse(apiErr(http.StatusUnprocessableEntity, "invalid", "Unknown alert."))
	}
	s, aerr := applyAdminInput(a.userID, a.profileID, a.username, in.Settings)
	if aerr != nil {
		return errorResponse(aerr)
	}
	prof, ready, err := p.adminPushoverReady(ctx, a.profileID)
	if err != nil {
		return storeError(p, err)
	}
	if !ready {
		return errorResponse(apiErr(http.StatusUnprocessableEntity, "pushover_not_set_up", adminPushoverMissing))
	}
	msg := adminMessage(prof, s.alert(t.Key), t.Sample, time.Time{})
	msg.Token = effectiveToken(prof, p.config())
	msg.Title = "[Test] " + msg.Title
	if err := p.pushover.Send(ctx, msg); err != nil {
		return errorResponse(apiErr(http.StatusBadGateway, "pushover_failed", err.Error()))
	}
	return jsonResponse(http.StatusOK, map[string]bool{"ok": true})
}

// handleListUsers lists Silo's accounts for the "users to ignore" picker.
func (p *Plugin) handleListUsers(ctx context.Context, a *adminViewer) *pluginv1.HandleHTTPResponse {
	users, err := p.silo.Users(ctx, a.base, a.key)
	switch {
	case errors.Is(err, errSiloKeyRejected), errors.Is(err, errSiloKeyScope):
		return errorResponse(monitorKeyProblem(err))
	case err != nil:
		p.logger.Warn("could not list Silo accounts with the read-only key", "error", err)
		return errorResponse(apiErr(http.StatusBadGateway, "silo_unavailable", "Couldn't reach Silo to list accounts. Try again."))
	}
	type userView struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Role     string `json:"role"`
		Enabled  bool   `json:"enabled"`
	}
	out := make([]userView, 0, len(users))
	for _, u := range users {
		out = append(out, userView{ID: u.ID, Username: u.Username, Role: u.Role, Enabled: u.Enabled})
	}
	slices.SortFunc(out, func(x, y userView) int {
		return strings.Compare(strings.ToLower(x.Username), strings.ToLower(y.Username))
	})
	return jsonResponse(http.StatusOK, map[string]any{"users": out})
}

func storeError(p *Plugin, err error) *pluginv1.HandleHTTPResponse {
	if errors.Is(err, errHostUnavailable) {
		return errorResponse(apiErr(http.StatusServiceUnavailable, "host_unavailable", "The plugin is still starting. Try again in a moment."))
	}
	p.logger.Error("settings storage failed", "error", err)
	return errorResponse(apiErr(http.StatusInternalServerError, "storage_failed", "Couldn't read or save settings."))
}

func header(req *pluginv1.HandleHTTPRequest, name string) string {
	for k, v := range req.GetHeaders() {
		if strings.EqualFold(k, name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func jsonResponse(status int, v any) *pluginv1.HandleHTTPResponse {
	body, err := json.Marshal(v)
	if err != nil {
		status, body = http.StatusInternalServerError, []byte(`{"code":"internal","message":"Internal error."}`)
	}
	return &pluginv1.HandleHTTPResponse{
		StatusCode: int32(status),
		Headers:    map[string]string{"Content-Type": "application/json", "Cache-Control": "no-store"},
		Body:       body,
	}
}

func errorResponse(e *apiError) *pluginv1.HandleHTTPResponse {
	return jsonResponse(e.status, e)
}
