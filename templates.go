package main

import (
	"fmt"
	"html"
	"regexp"
	"strings"
)

// notificationType describes one kind of Silo notification a profile can
// route to Pushover, with its default wording.
type notificationType struct {
	Key            string   `json:"key"`
	Label          string   `json:"label"`
	Description    string   `json:"description"`
	DefaultTitle   string   `json:"default_title"`
	DefaultMessage string   `json:"default_message"`
	DefaultEnabled bool     `json:"default_enabled"`
	Placeholders   []string `json:"placeholders"`
	// Sample fills the placeholders for previews and test sends.
	Sample map[string]string `json:"sample"`
}

func (t notificationType) defaults() TypeSettings {
	return TypeSettings{Enabled: t.DefaultEnabled, Title: t.DefaultTitle, Message: t.DefaultMessage}
}

// otherTypeKey covers delivery types this plugin has no dedicated entry for.
// Silo documents its delivery type registry as open, so new types land here
// until the plugin learns about them.
const otherTypeKey = "other"

var commonPlaceholders = []string{"profile", "type_label"}

var requestPlaceholders = []string{"title", "year", "title_year", "media_type"}

var notificationTypes = []notificationType{
	{
		Key:            "episode.available",
		Label:          "New episode available",
		Description:    "A new episode of a show you favorited, watchlisted or are watching was added.",
		DefaultTitle:   "New episode: {series}",
		DefaultMessage: "{sxe} · {episode_title}",
		DefaultEnabled: true,
		Placeholders:   append([]string{"series", "episode_title", "season", "episode", "sxe", "reasons"}, commonPlaceholders...),
		Sample: map[string]string{
			"series": "Severance", "episode_title": "Hello, Ms. Cobel", "season": "2", "episode": "1",
			"sxe": "S02E01", "reasons": "Watchlist, Next Up",
		},
	},
	{
		Key:            "request.approved",
		Label:          "Request approved",
		Description:    "An admin approved something you requested.",
		DefaultTitle:   "Request approved",
		DefaultMessage: "{title_year} was approved and is on its way.",
		DefaultEnabled: true,
		Placeholders:   append(append([]string{}, requestPlaceholders...), commonPlaceholders...),
		Sample: map[string]string{
			"title": "Dune: Part Two", "year": "2024", "title_year": "Dune: Part Two (2024)", "media_type": "Movie",
		},
	},
	{
		Key:            "request.declined",
		Label:          "Request declined",
		Description:    "An admin declined something you requested.",
		DefaultTitle:   "Request declined",
		DefaultMessage: "{title_year} was declined. {decline_reason}",
		DefaultEnabled: true,
		Placeholders:   append(append([]string{}, requestPlaceholders...), append([]string{"decline_reason"}, commonPlaceholders...)...),
		Sample: map[string]string{
			"title": "Dune: Part Two", "year": "2024", "title_year": "Dune: Part Two (2024)", "media_type": "Movie",
			"decline_reason": "It's already in the 4K library.",
		},
	},
	{
		Key:            "request.fulfilled",
		Label:          "Request available",
		Description:    "Something you requested is now in the library.",
		DefaultTitle:   "Now available",
		DefaultMessage: "{title_year} is ready to watch.",
		DefaultEnabled: true,
		Placeholders:   append(append([]string{}, requestPlaceholders...), commonPlaceholders...),
		Sample: map[string]string{
			"title": "Dune: Part Two", "year": "2024", "title_year": "Dune: Part Two (2024)", "media_type": "Movie",
		},
	},
	{
		Key:            otherTypeKey,
		Label:          "Other Silo notices",
		Description:    "Anything else Silo posts to your inbox, such as one of your webhooks being turned off after repeated failures.",
		DefaultTitle:   "Silo",
		DefaultMessage: "{type_label}",
		DefaultEnabled: true,
		Placeholders:   append([]string{"type"}, commonPlaceholders...),
		Sample:         map[string]string{"type": "webhook.auto_disabled", "type_label": "Webhook auto disabled"},
	},
}

// notificationTypeByKey returns the entry for a delivery type, falling back
// to the "other" entry for types without one.
func notificationTypeByKey(key string) notificationType {
	for _, t := range notificationTypes {
		if t.Key == key {
			return t
		}
	}
	return notificationTypes[len(notificationTypes)-1]
}

// humanizeType turns "webhook.auto_disabled" into "Webhook auto disabled".
func humanizeType(deliveryType string) string {
	s := strings.NewReplacer(".", " ", "_", " ").Replace(deliveryType)
	if s == "" {
		return "Notification"
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func seasonEpisode(season, episode *int) string {
	switch {
	case season != nil && episode != nil:
		return fmt.Sprintf("S%02dE%02d", *season, *episode)
	case episode != nil:
		return fmt.Sprintf("E%02d", *episode)
	default:
		return ""
	}
}

var placeholderPattern = regexp.MustCompile(`\{([a-z_]+)\}`)

// renderTemplate substitutes {placeholder}s line by line. Unknown
// placeholders are left as typed so mistakes are visible. A line whose known
// placeholders all came out empty is dropped ("Watched {watched}" with
// nothing watched), and separators left dangling or doubled by an empty
// value ("S02E01 · ", "Remote · · 8 Mbps") are tidied.
func renderTemplate(tmpl string, vars map[string]string, escapeHTML bool) string {
	lines := strings.Split(strings.ReplaceAll(tmpl, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			out = append(out, "")
			continue
		}
		known, filled := 0, 0
		rendered := placeholderPattern.ReplaceAllStringFunc(line, func(match string) string {
			value, ok := vars[match[1:len(match)-1]]
			if !ok {
				return match
			}
			known++
			if strings.TrimSpace(value) != "" {
				filled++
			}
			if escapeHTML {
				return html.EscapeString(value)
			}
			return value
		})
		if known > 0 && filled == 0 {
			continue
		}
		if rendered = tidyLine(rendered); rendered != "" {
			out = append(out, rendered)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

var emptyBrackets = strings.NewReplacer("()", "", "[]", "")

// ";" is deliberately absent: it ends HTML entities in HTML mode.
const danglingSeparators = " \t·•|,:–—"

// separatorWords are the words that only separate other words; a run of
// them left by empty values is cut to one.
var separatorWords = map[string]bool{"·": true, "•": true, "|": true, "–": true, "—": true}

func tidyLine(line string) string {
	line = emptyBrackets.Replace(line)
	words := strings.Fields(line)
	kept := words[:0]
	for _, w := range words {
		if separatorWords[w] && len(kept) > 0 && separatorWords[kept[len(kept)-1]] {
			continue
		}
		kept = append(kept, w)
	}
	line = strings.Join(kept, " ")
	line = strings.TrimLeft(line, danglingSeparators)
	return strings.TrimRight(line, danglingSeparators+"-")
}

// Admin playback alert types. Each is a switch in an admin's settings.
const (
	alertStreamStarted  = "stream.started"
	alertStreamPaused   = "stream.paused" // paused and resumed
	alertStreamStopped  = "stream.stopped"
	alertPlaybackFailed = "playback.failed"
)

// adminAlertType describes one admin alert with its default wording.
type adminAlertType struct {
	Key            string   `json:"key"`
	Label          string   `json:"label"`
	Description    string   `json:"description"`
	DefaultTitle   string   `json:"default_title"`
	DefaultMessage string   `json:"default_message"`
	Placeholders   []string `json:"placeholders"`
	// NoiseControlled alerts follow the admin's noise mode; playback
	// failures always send.
	NoiseControlled bool `json:"noise_controlled"`
	// Sample fills the placeholders for previews and test sends.
	Sample map[string]string `json:"sample"`
}

func (t adminAlertType) defaults() AlertSettings {
	return AlertSettings{Title: t.DefaultTitle, Message: t.DefaultMessage}
}

// playbackPlaceholders are filled on every playback alert.
var playbackPlaceholders = []string{
	"user", "profile", "user_profile", "title", "media_title", "series", "episode_title", "season", "episode", "sxe",
	"media_type", "device", "client", "platform", "ip", "location", "method",
}

func withPlaceholders(extra ...string) []string {
	return append(append([]string{}, playbackPlaceholders...), extra...)
}

// playbackSample is a remote transcode of an episode, for previews and tests.
var playbackSample = map[string]string{
	"user": "alice", "profile": "Alice", "user_profile": "alice (Alice)", "title": "Severance S02E01",
	"media_title": "Hello, Ms. Cobel", "series": "Severance", "episode_title": "Hello, Ms. Cobel",
	"season": "2", "episode": "1", "sxe": "S02E01", "media_type": "Episode",
	"device": "Living Room TV", "client": "Silo for Android TV 2.4.1", "platform": "android_tv",
	"ip": "203.0.113.24", "location": "Remote", "method": "Transcode",
	"video": "4K → 1080p", "resolution": "4K", "bitrate": "8.0 Mbps", "hw_accel": "nvenc",
	"action": "paused", "position": "12:34", "duration": "57:10", "progress": "22%",
	"watched": "42m", "finished": "not finished", "end_reason": "Silo couldn't keep playing it",
}

var adminAlertTypes = []adminAlertType{
	{
		Key:             alertStreamStarted,
		Label:           "Stream started",
		Description:     "Someone starts playing something.",
		DefaultTitle:    "{user_profile} started {title}",
		DefaultMessage:  "{episode_title}\n{device} · {client}\n{location} · {method} · {video} · {bitrate}",
		Placeholders:    withPlaceholders("video", "resolution", "bitrate", "hw_accel", "position", "progress"),
		NoiseControlled: true,
	},
	{
		Key:             alertStreamPaused,
		Label:           "Paused / resumed",
		Description:     "A stream is paused or resumed.",
		DefaultTitle:    "{user_profile} {action} {title}",
		DefaultMessage:  "{episode_title}\nAt {position} ({progress})\n{device}",
		Placeholders:    withPlaceholders("action", "position", "duration", "progress"),
		NoiseControlled: true,
	},
	{
		Key:             alertStreamStopped,
		Label:           "Stream stopped",
		Description:     "A stream ends.",
		DefaultTitle:    "{user_profile} stopped {title}",
		DefaultMessage:  "{episode_title}\nWatched {watched}\n{progress} · {finished}\n{device}",
		Placeholders:    withPlaceholders("watched", "position", "duration", "progress", "finished", "end_reason"),
		NoiseControlled: true,
	},
	{
		Key:            alertPlaybackFailed,
		Label:          "Playback failed",
		Description:    "Silo gave up on a stream, for example because the file is missing or the connection to the client failed.",
		DefaultTitle:   "Playback failed for {user_profile}",
		DefaultMessage: "{title}\n{episode_title}\n{device} · {client}\nAt {position}",
		Placeholders:   withPlaceholders("position", "end_reason"),
	},
}

func init() {
	for i := range adminAlertTypes {
		adminAlertTypes[i].Sample = playbackSample
	}
}

// adminAlertTypeByKey returns an alert type, or false for an unknown key.
func adminAlertTypeByKey(key string) (adminAlertType, bool) {
	for _, t := range adminAlertTypes {
		if t.Key == key {
			return t, true
		}
	}
	return adminAlertType{}, false
}
