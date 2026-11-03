package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Admin playback alerts. Each session.* event goes to every admin who
// switched that alert on, whose filters and noise mode let it through, and
// whom Silo still lists as an enabled admin. Alerts need the read-only Silo
// key: it's how the plugin re-checks each recipient's role.
//
// One event can reach several admins, so each recipient is recorded as
// handled on its own (event ID plus account ID). A retry after a partial
// failure sends only to the admins who didn't get it.

const (
	// roleCheckBudget bounds the role checks, leaving the rest of the event
	// deadline for sending.
	roleCheckBudget = 3 * time.Second
	// liveSessionTTL is how long a session with no stop seen still counts as
	// playing.
	liveSessionTTL = 12 * time.Hour
)

// recipient is one admin an event may go to.
type recipient struct {
	admin   *AdminSettings
	kind    string // the alert type sent to this admin
	key     string // handled-event ID for this admin, "" when not deduped
	profile *ProfileSettings
	token   string
}

// handlePlaybackEvent sends the admin alerts for one session.* event. It
// returns an error only when trying the event again could help.
func (p *Plugin) handlePlaybackEvent(ctx context.Context, eventID, eventName string, raw []byte) error {
	pb, ok := parseLifecycle(eventName, raw, time.Now())
	if !ok {
		return nil
	}
	// A replaced session goes on playing in a new one, which sends its own
	// start.
	if pb.Event == eventSessionStopped && pb.EndReason == endReasonReplaced {
		return nil
	}
	cfg := p.config()
	if cfg.MonitorAPIKey == "" {
		return nil
	}
	key := playbackDedupeKey(eventID, pb)
	log := p.logger.With("event", key, "event_name", eventName, "session_id", pb.SessionID)

	admins, err := waitForHost(ctx, func() ([]*AdminSettings, error) { return p.admins.List(ctx) })
	if err != nil {
		log.Warn("could not load admin alert settings; asking Silo to retry", "error", err)
		return fmt.Errorf("load admin alert settings: %w", err)
	}
	var candidates []*recipient
	optedIn := false
	for _, a := range admins {
		optedIn = optedIn || a.anyEnabled()
		if kind, ok := a.alertFor(pb); ok {
			r := &recipient{admin: a, kind: kind}
			if key != "" {
				r.key = key + "|admin:" + strconv.Itoa(a.UserID)
			}
			candidates = append(candidates, r)
		}
	}
	if len(candidates) == 0 {
		if optedIn {
			// Remember the session anyway, for binges and for its stop's
			// filters.
			if _, err := p.throttle.decide(ctx, log, &pb, nil); err != nil {
				log.Debug("could not read alert noise-control state", "error", err)
			}
		}
		return nil
	}

	// retry is set when some recipient couldn't be handled yet; the others
	// still get the alert now and are skipped when Silo retries.
	candidates, stop, retry := p.confirmAdmins(ctx, log, cfg.MonitorAPIKey, candidates)
	if stop {
		return nil
	}

	candidates, again, err := p.dropHandled(ctx, log, candidates)
	if err != nil {
		log.Warn("could not read handled events; asking Silo to retry", "error", err)
		return fmt.Errorf("read handled events: %w", err)
	}
	retry = retry || again

	recipients, err := p.throttle.decide(ctx, log, &pb, candidates)
	if err != nil {
		log.Warn("could not read alert noise-control state; asking Silo to retry", "error", err)
		return fmt.Errorf("read alert noise-control state: %w", err)
	}

	var handled []string
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, r := range recipients {
		wg.Go(func() {
			done, again := p.sendAdminAlert(ctx, log, pb, r)
			mu.Lock()
			defer mu.Unlock()
			if done && r.key != "" {
				handled = append(handled, r.key)
			}
			retry = retry || again
		})
	}
	wg.Wait()

	if len(handled) > 0 {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
		if err := p.handled.RecordMany(rctx, handled); err != nil {
			log.Warn("could not record handled admin alerts; a repeat may be sent twice", "error", err)
		}
		cancel()
	}
	if retry {
		return errors.New("some admin alerts couldn't be sent yet")
	}
	return nil
}

// playbackDedupeKey identifies an event across retries. Older hosts send no
// event_id (and never retry); the session and sequence number stand in.
func playbackDedupeKey(eventID string, pb playback) string {
	switch {
	case eventID != "":
		return eventID
	case pb.Seq > 0:
		return "playback:" + pb.SessionID + ":" + strconv.FormatInt(pb.Seq, 10)
	}
	return ""
}

// alertFor picks the alert an admin gets for pb, if any. A failed play goes
// out as "stream stopped" to admins who want stops but not failures.
func (a *AdminSettings) alertFor(pb playback) (string, bool) {
	kind := pb.alertKind()
	if kind == "" {
		return "", false
	}
	// A stop's location and method are only known for sure once its start
	// has been looked up (throttleStore.decide), which checks them then.
	f := a.Filters
	if pb.Event == eventSessionStopped {
		f.OnlyRemote, f.OnlyTranscodes = false, false
	}
	if !f.match(pb) {
		return "", false
	}
	if a.alert(kind).Enabled {
		return kind, true
	}
	if kind == alertPlaybackFailed && a.alert(alertStreamStopped).Enabled {
		return alertStreamStopped, true
	}
	return "", false
}

// match applies the filters. Unknown location or method counts as not
// remote and not a transcode.
func (f AdminFilters) match(pb playback) bool {
	switch {
	case slices.Contains(f.IgnoreUsers, pb.UserID):
		return false
	case f.OnlyRemote && pb.Location != "remote":
		return false
	case f.OnlyTranscodes && !pb.isTranscode():
		return false
	case len(f.MediaTypes) > 0 && !slices.Contains(f.MediaTypes, pb.MediaType):
		return false
	}
	return true
}

// confirmAdmins keeps the candidates Silo still lists as enabled admins.
// stop is true when Silo rejected the key: no admin alerts go out, and that
// is logged once until the key changes. again is true when a lookup failed
// for a reason a retry could fix.
func (p *Plugin) confirmAdmins(ctx context.Context, log *slog.Logger, apiKey string, candidates []*recipient) (kept []*recipient, stop, again bool) {
	info, err := p.host(ctx)
	if err != nil || info.InternalBaseURL == "" {
		log.Warn("Silo's internal URL isn't known yet; asking Silo to retry", "error", err)
		return nil, false, true
	}
	base := strings.TrimRight(info.InternalBaseURL, "/")
	cctx, cancel := context.WithTimeout(ctx, roleCheckBudget)
	defer cancel()

	errs := make([]error, len(candidates))
	users := make([]*siloUser, len(candidates))
	var wg sync.WaitGroup
	for i, r := range candidates {
		wg.Go(func() { users[i], errs[i] = p.silo.User(cctx, base, apiKey, r.admin.UserID) })
	}
	wg.Wait()

	for i, r := range candidates {
		err := errs[i]
		switch {
		case errors.Is(err, errSiloKeyRejected), errors.Is(err, errSiloKeyScope):
			if !p.adminKeyWarned.Swap(true) {
				log.Warn("Silo refused the read-only key, so admin alerts are paused until it's fixed. Set up the key again from the admin section of the Pushover page.", "error", err)
			}
			return nil, true, false
		case errors.Is(err, errSiloUserNotFound):
			log.Info("skipping admin alerts for an account Silo no longer has", "user_id", r.admin.UserID)
		case err != nil:
			log.Warn("could not check an admin's role; will retry for them", "user_id", r.admin.UserID, "error", err)
			again = true
		case !users[i].isAdmin():
			log.Info("skipping admin alerts for an account that is no longer an enabled admin", "user_id", r.admin.UserID)
		default:
			kept = append(kept, r)
		}
	}
	p.adminKeyWarned.Store(false)
	return kept, false, again
}

// dropHandled removes recipients that already got this event and loads each
// remaining one's Pushover settings. again is true when a recipient's
// settings couldn't be read.
func (p *Plugin) dropHandled(ctx context.Context, log *slog.Logger, candidates []*recipient) (kept []*recipient, again bool, err error) {
	var keys []string
	for _, r := range candidates {
		if r.key != "" {
			keys = append(keys, r.key)
		}
	}
	seen, err := p.handled.SeenMany(ctx, keys)
	if err != nil {
		return nil, false, err
	}
	cfg := p.config()
	for _, r := range candidates {
		if r.key != "" && seen[r.key] {
			continue
		}
		prof, found, err := p.loadSettingsForDelivery(ctx, r.admin.ProfileID)
		switch {
		case err != nil:
			log.Warn("could not load an admin's Pushover settings; will retry for them", "user_id", r.admin.UserID, "error", err)
			again = true
			continue
		case !found || prof.UserKey == "" || prof.UserID != r.admin.UserID:
			log.Info("admin has alerts on but their profile has no Pushover settings", "user_id", r.admin.UserID)
			continue
		}
		r.profile, r.token = prof, effectiveToken(prof, cfg)
		if r.token == "" {
			log.Warn("admin's profile has no Pushover application token and the server has no shared one", "user_id", r.admin.UserID)
			continue
		}
		kept = append(kept, r)
	}
	return kept, again, nil
}

// sendAdminAlert sends one admin's alert. done means it was sent or
// rejected for good; again means a retry could get it through.
func (p *Plugin) sendAdminAlert(ctx context.Context, log *slog.Logger, pb playback, r *recipient) (done, again bool) {
	log = log.With("user_id", r.admin.UserID, "alert", r.kind)
	msg := adminMessage(r.profile, r.admin.alert(r.kind), pb.vars(), pb.At)
	msg.Token = r.token
	if err := p.pushover.Send(ctx, msg); err != nil {
		var perr *pushoverError
		if !errors.As(err, &perr) || perr.Retryable {
			log.Warn("Pushover send failed; asking Silo to retry", "error", err)
			return false, true
		}
		log.Warn("Pushover rejected the admin alert; not retrying", "error", err, "http_status", perr.HTTPStatus)
		return true, false
	}
	log.Info("sent admin alert")
	return true, false
}

// adminMessage renders an alert for an admin's profile. The profile supplies
// the key, device, HTML switch and default sound and priority.
func adminMessage(prof *ProfileSettings, a AlertSettings, vars map[string]string, at time.Time) pushoverMessage {
	msg := pushoverMessage{
		User:      prof.UserKey,
		Title:     renderTemplate(a.Title, vars, prof.HTML),
		Message:   renderTemplate(a.Message, vars, prof.HTML),
		Device:    prof.Device,
		Sound:     prof.Sound,
		Priority:  prof.Priority,
		HTML:      prof.HTML,
		Timestamp: at,
	}
	if a.Priority != nil {
		msg.Priority = *a.Priority
	}
	if a.Sound != "" {
		msg.Sound = a.Sound
	}
	return msg
}

// waitForHost retries fn briefly while the host connection, bound just after
// the plugin starts, isn't ready.
func waitForHost[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	for attempt := 0; ; attempt++ {
		v, err := fn()
		if !errors.Is(err, errHostUnavailable) || attempt == 8 {
			return v, err
		}
		if err := sleepCtx(ctx, 250*time.Millisecond); err != nil {
			return v, err
		}
	}
}

// throttleStore keeps noise-control state in the "throttle" instance state
// key: recent sessions by user and show (for collapsing binges, and to
// remember a stream's location and method for its stop alert) and the last
// alert per admin, user and type (for cooldowns). It's shared by every API
// server; two servers handling events for the same user at once can each
// send, which only costs an extra alert.
type throttleStore struct {
	state stateBackend
	now   func() time.Time
	// mu makes this process's read-decide-write of the key atomic.
	mu sync.Mutex
}

const (
	throttleKey          = "throttle"
	maxThrottleSessions  = 2000
	maxThrottleCooldowns = 4000
)

func newThrottleStore(state stateBackend) *throttleStore {
	return &throttleStore{state: state, now: time.Now}
}

type throttleState struct {
	Sessions  map[string]*sessionMark `json:"sessions"`
	Cooldowns map[string]cooldownMark `json:"cooldowns"`
}

// sessionMark is what noise control remembers about one playback session.
type sessionMark struct {
	UserID string `json:"user_id"`
	Show   string `json:"show"`
	// Started is set once the session's start event was seen.
	Started bool      `json:"started,omitempty"`
	Start   time.Time `json:"start,omitzero"`
	End     time.Time `json:"end,omitzero"`
	// Location and Method come from Silo's live session row, so a stop
	// (whose history entry lacks them) can still be filtered.
	Location string `json:"location,omitempty"`
	Method   string `json:"method,omitempty"`
	// Device is the client label ("Living Room TV"), also missing from
	// history entries.
	Device string `json:"device,omitempty"`
}

// cooldownMark is the last alert of one type about one user to one admin.
// Event is the alert's handled-event ID, so a retry of the same event isn't
// held back by its own mark.
type cooldownMark struct {
	At    time.Time `json:"at"`
	Event string    `json:"event,omitempty"`
}

// active reports whether the session was playing at, or stopped less than
// window before, the given time.
func (m *sessionMark) active(at time.Time, window time.Duration) bool {
	start := m.Start
	if start.IsZero() {
		start = m.End
	}
	if start.After(at) {
		return false // it began after this event, which arrived late
	}
	if m.End.IsZero() {
		return at.Sub(start) < liveSessionTTL
	}
	return at.Sub(m.End) < window
}

func (t *throttleStore) read(ctx context.Context) (*throttleState, []byte, error) {
	st := &throttleState{Sessions: map[string]*sessionMark{}, Cooldowns: map[string]cooldownMark{}}
	raw, found, err := t.state.Read(ctx, throttleKey)
	if err != nil {
		return nil, nil, err
	}
	if found && json.Unmarshal(raw, st) != nil {
		// Damaged state only costs noise control; start over.
		st = &throttleState{}
	}
	if st.Sessions == nil {
		st.Sessions = map[string]*sessionMark{}
	}
	if st.Cooldowns == nil {
		st.Cooldowns = map[string]cooldownMark{}
	}
	return st, raw, nil
}

// decide applies each recipient's noise mode to pb, records the event in the
// noise-control state, and returns the recipients to send to. A stop is
// first filled in with the location and method its start recorded, and the
// filters are checked again with them.
func (t *throttleStore) decide(ctx context.Context, log *slog.Logger, pb *playback, candidates []*recipient) ([]*recipient, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, raw, err := t.read(ctx)
	if err != nil {
		return nil, err
	}

	if m := st.Sessions[pb.SessionID]; m != nil && pb.Event == eventSessionStopped {
		if m.Location != "" {
			pb.Location = m.Location
		}
		if pb.Method == "" {
			pb.Method = m.Method
		}
		if pb.ClientLabel == "" {
			pb.ClientLabel = m.Device
		}
	}

	var out []*recipient
	for _, r := range candidates {
		if pb.Event == eventSessionStopped && !r.admin.Filters.match(*pb) {
			continue
		}
		if st.suppressed(*pb, r) {
			continue
		}
		if at, _ := adminAlertTypeByKey(r.kind); at.NoiseControlled && r.admin.Noise.Mode == noiseCooldown {
			ck := cooldownKey(r.admin.UserID, pb.UserID, r.kind)
			if prev, ok := st.Cooldowns[ck]; !ok || !pb.At.Before(prev.At) {
				st.Cooldowns[ck] = cooldownMark{At: pb.At, Event: r.key}
			}
		}
		out = append(out, r)
	}

	st.note(*pb)
	st.prune(t.now())
	next, err := json.Marshal(st)
	if err == nil && string(next) != string(raw) {
		err = t.state.Write(ctx, throttleKey, next)
	}
	if err != nil {
		// Sending still beats dropping the alerts; noise control just
		// won't remember this event.
		log.Warn("could not save alert noise-control state", "error", err)
	}
	return out, nil
}

func cooldownKey(adminID int, userID, kind string) string {
	return strconv.Itoa(adminID) + "|" + userID + "|" + kind
}

// suppressed reports whether r's noise mode holds back pb.
func (st *throttleState) suppressed(pb playback, r *recipient) bool {
	t, _ := adminAlertTypeByKey(r.kind)
	if !t.NoiseControlled {
		return false
	}
	window := r.admin.window()
	switch r.admin.Noise.Mode {
	case noiseCooldown:
		prev, ok := st.Cooldowns[cooldownKey(r.admin.UserID, pb.UserID, r.kind)]
		if !ok || (r.key != "" && prev.Event == r.key) {
			return false
		}
		gap := pb.At.Sub(prev.At)
		return gap < window && gap > -window
	case noiseBinge:
		return st.inBinge(pb, window)
	}
	return false
}

// inBinge reports whether pb continues a binge: the same user played the
// same show in another session that is still playing or stopped less than
// window ago, or this is a later event of a session whose start was seen.
// The session's own start is left out, so a retry of a start decides the
// same way as the first try.
func (st *throttleState) inBinge(pb playback, window time.Duration) bool {
	show := pb.showKey()
	for id, m := range st.Sessions {
		if m.UserID != pb.UserID || m.Show != show {
			continue
		}
		if id == pb.SessionID {
			if pb.Event != eventSessionStarted && m.Started {
				return true
			}
			continue
		}
		if m.active(pb.At, window) {
			return true
		}
	}
	return false
}

// note records pb in the session marks. Every change is idempotent, so a
// retried event leaves the same state.
func (st *throttleState) note(pb playback) {
	m := st.Sessions[pb.SessionID]
	if m == nil {
		m = &sessionMark{UserID: pb.UserID, Show: pb.showKey()}
		st.Sessions[pb.SessionID] = m
	}
	switch pb.Event {
	case eventSessionStarted:
		m.Started = true
		m.Start = pb.At
	case eventSessionStopped:
		m.End = pb.At
	default:
		if m.Start.IsZero() {
			m.Start = cmp.Or(pb.StartedAt, pb.At)
		}
	}
	if pb.FromRow {
		m.Location, m.Method, m.Device = pb.Location, pb.Method, pb.ClientLabel
	}
}

// prune drops marks too old to matter and caps the state's size.
func (st *throttleState) prune(now time.Time) {
	const keep = maxNoiseWindow * time.Minute
	for id, m := range st.Sessions {
		stale := now.Sub(m.Start) > liveSessionTTL // never seen stopping
		if !m.End.IsZero() {
			stale = now.Sub(m.End) > keep
		}
		if stale {
			delete(st.Sessions, id)
		}
	}
	for k, c := range st.Cooldowns {
		if now.Sub(c.At) > keep {
			delete(st.Cooldowns, k)
		}
	}
	if n := len(st.Sessions) - maxThrottleSessions; n > 0 {
		ids := slices.Collect(maps.Keys(st.Sessions))
		lastSeen := func(m *sessionMark) time.Time {
			if m.End.After(m.Start) {
				return m.End
			}
			return m.Start
		}
		slices.SortFunc(ids, func(a, b string) int { return lastSeen(st.Sessions[a]).Compare(lastSeen(st.Sessions[b])) })
		for _, id := range ids[:n] {
			delete(st.Sessions, id)
		}
	}
	if n := len(st.Cooldowns) - maxThrottleCooldowns; n > 0 {
		keys := slices.Collect(maps.Keys(st.Cooldowns))
		slices.SortFunc(keys, func(a, b string) int { return st.Cooldowns[a].At.Compare(st.Cooldowns[b].At) })
		for _, k := range keys[:n] {
			delete(st.Cooldowns, k)
		}
	}
}
