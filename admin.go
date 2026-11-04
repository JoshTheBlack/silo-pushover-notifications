package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimehost"
)

// AdminSettings is one admin's playback alert configuration, keyed by Silo
// account ID. It's created when an acting admin saves the admin section of
// the page. Alerts go to the Pushover user key and token of ProfileID, the
// admin's primary profile.
type AdminSettings struct {
	UserID int `json:"user_id"`
	// ProfileID is the primary profile the admin saved from (Silo sends
	// X-Silo-User-Role: admin only on an admin's primary profile).
	ProfileID string                   `json:"profile_id"`
	Username  string                   `json:"username,omitempty"`
	Alerts    map[string]AlertSettings `json:"alerts"`
	Filters   AdminFilters             `json:"filters"`
	Noise     NoiseSettings            `json:"noise"`
	Limits    AdminLimits              `json:"limits"`
	UpdatedAt time.Time                `json:"updated_at"`
}

// AlertSettings controls one admin alert type. All are off by default.
type AlertSettings struct {
	Enabled bool   `json:"enabled"`
	Title   string `json:"title"`
	Message string `json:"message"`
	// Sound overrides the profile's sound when non-empty.
	Sound string `json:"sound,omitempty"`
	// Priority overrides the profile's priority when set.
	Priority *int `json:"priority,omitempty"`
}

// AdminFilters narrow which playback an admin hears about. They apply to all
// playback alerts; IgnoreUsers also applies to the watchdog alerts about one
// account (its stream limit, new devices and new addresses).
type AdminFilters struct {
	// IgnoreUsers are Silo account IDs whose playback is never alerted.
	IgnoreUsers    []string `json:"ignore_users"`
	OnlyRemote     bool     `json:"only_remote"`
	OnlyTranscodes bool     `json:"only_transcodes"`
	// MediaTypes limits alerts to these media types; empty means all.
	MediaTypes []string `json:"media_types"`
	// Libraries limits alerts to items in these Silo libraries (IDs);
	// empty means all. An item Silo's catalog can't place (audiobooks, for
	// one) matches none.
	Libraries []string `json:"libraries"`
}

// AdminLimits are the numbers the limit alerts compare against. Zero means
// not set; a limit alert can only be switched on with its limit set.
type AdminLimits struct {
	// UserStreams: alert when one account plays more streams than this.
	UserStreams int `json:"user_streams"`
	// ServerStreams: alert when more streams than this play at once.
	ServerStreams int `json:"server_streams"`
	// BandwidthMbps: alert when the streams' bitrates add up to more.
	BandwidthMbps int `json:"bandwidth_mbps"`
	// BandwidthRemoteOnly counts only remote streams toward the bandwidth.
	BandwidthRemoteOnly bool `json:"bandwidth_remote_only"`
}

const (
	maxUserStreams   = 100
	maxServerStreams = 1000
	maxBandwidthMbps = 100000
)

// limit returns the limit a limit alert uses.
func (l AdminLimits) limit(kind string) int {
	switch kind {
	case alertStreamsUser:
		return l.UserStreams
	case alertStreamsServer:
		return l.ServerStreams
	case alertBandwidth:
		return l.BandwidthMbps
	}
	return 0
}

// NoiseSettings is how an admin wants repeated start, pause/resume and stop
// alerts thinned out.
type NoiseSettings struct {
	Mode          string `json:"mode"`
	WindowMinutes int    `json:"window_minutes"`
}

// Noise modes.
const (
	// noiseBinge sends the first alert about a user and show, then nothing
	// more about them until the window has passed since their last stop.
	noiseBinge = "binge"
	// noiseCooldown sends at most one alert of each type per user per window.
	noiseCooldown = "cooldown"
	// noiseEvery sends every alert.
	noiseEvery = "every"

	defaultNoiseWindow = 30
	maxNoiseWindow     = 24 * 60
)

var noiseModes = []option{
	{noiseBinge, "Collapse binges"},
	{noiseCooldown, "Per-user cooldown"},
	{noiseEvery, "Every event"},
}

// mediaTypeOptions are the media types Silo plays. Others are accepted too.
var mediaTypeOptions = []option{{"movie", "Movies"}, {"episode", "TV episodes"}, {"audiobook", "Audiobooks"}}

func defaultAdminSettings() *AdminSettings {
	return &AdminSettings{Alerts: map[string]AlertSettings{}, Noise: NoiseSettings{Mode: noiseBinge, WindowMinutes: defaultNoiseWindow}}
}

// alert returns the stored settings for an alert type, or its defaults.
func (s *AdminSettings) alert(key string) AlertSettings {
	if a, ok := s.Alerts[key]; ok {
		return a
	}
	t, _ := adminAlertTypeByKey(key)
	return t.defaults()
}

// window is the noise window.
func (s *AdminSettings) window() time.Duration {
	return time.Duration(s.Noise.WindowMinutes) * time.Minute
}

// anyEnabled reports whether at least one alert is switched on.
func (s *AdminSettings) anyEnabled() bool {
	for _, t := range adminAlertTypes {
		if s.alert(t.Key).Enabled {
			return true
		}
	}
	return false
}

func (s *AdminSettings) clone() *AdminSettings {
	c := *s
	c.Alerts = maps.Clone(s.Alerts)
	c.Filters.IgnoreUsers = slices.Clone(s.Filters.IgnoreUsers)
	c.Filters.MediaTypes = slices.Clone(s.Filters.MediaTypes)
	c.Filters.Libraries = slices.Clone(s.Filters.Libraries)
	return &c
}

// adminInput is a save or test request from the page.
type adminInput struct {
	Alerts  map[string]AlertSettings `json:"alerts"`
	Filters AdminFilters             `json:"filters"`
	Noise   NoiseSettings            `json:"noise"`
	Limits  AdminLimits              `json:"limits"`
}

var (
	accountIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	libraryIDPattern = accountIDPattern
	mediaTypePattern = regexp.MustCompile(`^[a-z_]{1,32}$`)
)

// applyAdminInput validates input into settings for the admin.
func applyAdminInput(userID int, profileID, username string, in adminInput) (*AdminSettings, *apiError) {
	bad := func(msg string) *apiError { return apiErr(http.StatusUnprocessableEntity, "invalid", msg) }
	s := defaultAdminSettings()
	s.UserID, s.ProfileID, s.Username = userID, profileID, username

	for _, t := range adminAlertTypes {
		a, ok := in.Alerts[t.Key]
		if !ok {
			a = t.defaults()
		}
		a.Title, a.Message, a.Sound = strings.TrimSpace(a.Title), strings.TrimSpace(a.Message), strings.TrimSpace(a.Sound)
		switch {
		case len(a.Title) > 1024 || len(a.Message) > 2048:
			return nil, bad(t.Label + ": the template is too long.")
		case a.Priority != nil && !validPriority(*a.Priority):
			return nil, bad(t.Label + ": priority must be between -2 and 1.")
		case !pushoverSoundPattern.MatchString(a.Sound):
			return nil, bad(t.Label + ": that sound name isn't valid.")
		}
		if a.Title == "" && a.Message == "" {
			a.Title, a.Message = t.DefaultTitle, t.DefaultMessage
		}
		s.Alerts[t.Key] = a
	}

	f := in.Filters
	s.Filters.OnlyRemote, s.Filters.OnlyTranscodes = f.OnlyRemote, f.OnlyTranscodes
	if len(f.IgnoreUsers) > 500 || len(f.MediaTypes) > 20 || len(f.Libraries) > 200 {
		return nil, bad("Too many filter entries.")
	}
	for _, id := range f.IgnoreUsers {
		id = strings.TrimSpace(id)
		if !accountIDPattern.MatchString(id) {
			return nil, bad("A user to ignore isn't a valid Silo account ID.")
		}
		if !slices.Contains(s.Filters.IgnoreUsers, id) {
			s.Filters.IgnoreUsers = append(s.Filters.IgnoreUsers, id)
		}
	}
	for _, mt := range f.MediaTypes {
		mt = strings.ToLower(strings.TrimSpace(mt))
		if !mediaTypePattern.MatchString(mt) {
			return nil, bad("That media type isn't valid.")
		}
		if !slices.Contains(s.Filters.MediaTypes, mt) {
			s.Filters.MediaTypes = append(s.Filters.MediaTypes, mt)
		}
	}
	for _, id := range f.Libraries {
		id = strings.TrimSpace(id)
		if !libraryIDPattern.MatchString(id) {
			return nil, bad("A library to filter on isn't a valid Silo library ID.")
		}
		if !slices.Contains(s.Filters.Libraries, id) {
			s.Filters.Libraries = append(s.Filters.Libraries, id)
		}
	}

	s.Limits.BandwidthRemoteOnly = in.Limits.BandwidthRemoteOnly
	for _, l := range []struct {
		kind  string
		value int
		max   int
		set   *int
	}{
		{alertStreamsUser, in.Limits.UserStreams, maxUserStreams, &s.Limits.UserStreams},
		{alertStreamsServer, in.Limits.ServerStreams, maxServerStreams, &s.Limits.ServerStreams},
		{alertBandwidth, in.Limits.BandwidthMbps, maxBandwidthMbps, &s.Limits.BandwidthMbps},
	} {
		t, _ := adminAlertTypeByKey(l.kind)
		switch {
		case l.value < 0 || l.value > l.max:
			return nil, bad(fmt.Sprintf("%s: the limit must be between 1 and %d.", t.Label, l.max))
		case l.value == 0 && s.Alerts[l.kind].Enabled:
			return nil, bad(t.Label + ": set a limit, or switch the alert off.")
		}
		*l.set = l.value
	}

	switch in.Noise.Mode {
	case "":
	case noiseBinge, noiseCooldown, noiseEvery:
		s.Noise.Mode = in.Noise.Mode
	default:
		return nil, bad("Pick a noise mode: collapse binges, per-user cooldown or every event.")
	}
	if in.Noise.WindowMinutes != 0 {
		if in.Noise.WindowMinutes < 1 || in.Noise.WindowMinutes > maxNoiseWindow {
			return nil, bad(fmt.Sprintf("The noise window must be between 1 and %d minutes.", maxNoiseWindow))
		}
		s.Noise.WindowMinutes = in.Noise.WindowMinutes
	}
	s.UpdatedAt = time.Now().UTC()
	return s, nil
}

// adminStore keeps admin alert settings in instance state, spread across
// adminBuckets keys by account ID. Event delivery lists every admin, so the
// whole set is cached and reloaded once older than ttl; page requests read
// their own bucket fresh. Each write re-reads its bucket first so it doesn't
// undo another API server's change.
type adminStore struct {
	state stateBackend
	ttl   time.Duration
	now   func() time.Time

	// mu serializes loads and writes and guards the cache.
	mu       sync.Mutex
	loadedAt time.Time
	admins   map[int]*AdminSettings
}

const adminBuckets = 8

func newAdminStore(state stateBackend) *adminStore {
	return &adminStore{state: state, ttl: settingsCacheTTL, now: time.Now, admins: map[int]*AdminSettings{}}
}

func adminBucketKey(userID int) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(strconv.Itoa(userID)))
	return fmt.Sprintf("admins/%02d", h.Sum32()%adminBuckets)
}

func (s *adminStore) readBucket(ctx context.Context, key string) (map[string]*AdminSettings, error) {
	raw, found, err := s.state.Read(ctx, key)
	if err != nil {
		return nil, err
	}
	bucket := map[string]*AdminSettings{}
	if !found {
		return bucket, nil
	}
	if err := json.Unmarshal(raw, &bucket); err != nil {
		return nil, fmt.Errorf("decode admin settings bucket %s: %w", key, err)
	}
	return bucket, nil
}

// List returns copies of every admin's settings, sorted by account ID. If a
// reload fails but an older copy exists, that copy is used.
func (s *adminStore) List(ctx context.Context) ([]*AdminSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadedAt.IsZero() || s.now().Sub(s.loadedAt) >= s.ttl {
		admins := map[int]*AdminSettings{}
		var err error
		for i := range adminBuckets {
			var bucket map[string]*AdminSettings
			if bucket, err = s.readBucket(ctx, fmt.Sprintf("admins/%02d", i)); err != nil {
				break
			}
			for _, a := range bucket {
				if a != nil && a.UserID > 0 {
					admins[a.UserID] = a
				}
			}
		}
		switch {
		case err == nil:
			s.admins, s.loadedAt = admins, s.now()
		case s.loadedAt.IsZero():
			return nil, err
		}
	}
	out := make([]*AdminSettings, 0, len(s.admins))
	for _, a := range s.admins {
		out = append(out, a.clone())
	}
	slices.SortFunc(out, func(a, b *AdminSettings) int { return a.UserID - b.UserID })
	return out, nil
}

// Get reads one admin's settings from state.
func (s *adminStore) Get(ctx context.Context, userID int) (*AdminSettings, bool, error) {
	bucket, err := s.readBucket(ctx, adminBucketKey(userID))
	if err != nil {
		return nil, false, err
	}
	a, ok := bucket[strconv.Itoa(userID)]
	if !ok || a == nil {
		return nil, false, nil
	}
	return a, true, nil
}

// Put stores an admin's settings.
func (s *adminStore) Put(ctx context.Context, a *AdminSettings) error {
	return s.mutate(ctx, a.UserID, a.clone())
}

// Delete removes an admin's settings.
func (s *adminStore) Delete(ctx context.Context, userID int) error {
	return s.mutate(ctx, userID, nil)
}

func (s *adminStore) mutate(ctx context.Context, userID int, next *AdminSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := adminBucketKey(userID)
	bucket, err := s.readBucket(ctx, key)
	if err != nil {
		return err
	}
	if next == nil {
		delete(bucket, strconv.Itoa(userID))
	} else {
		bucket[strconv.Itoa(userID)] = next
	}
	raw, err := json.Marshal(bucket)
	if err != nil {
		return err
	}
	if len(raw) > runtimehost.MaxInstanceStateValueBytes {
		return errors.New("too many admins are configured to store another one")
	}
	if err := s.state.Write(ctx, key, raw); err != nil {
		return err
	}
	// Refresh this bucket's admins in the cache, if there is one.
	if !s.loadedAt.IsZero() {
		for id := range s.admins {
			if adminBucketKey(id) == key {
				delete(s.admins, id)
			}
		}
		for _, a := range bucket {
			if a != nil && a.UserID > 0 {
				s.admins[a.UserID] = a
			}
		}
	}
	return nil
}
