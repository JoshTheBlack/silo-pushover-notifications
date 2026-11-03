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
// placeholders are left as typed so mistakes are visible. A line that only
// held placeholders with no value is dropped, and separators left dangling
// by an empty value ("S02E01 · ") are trimmed.
func renderTemplate(tmpl string, vars map[string]string, escapeHTML bool) string {
	lines := strings.Split(strings.ReplaceAll(tmpl, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			out = append(out, "")
			continue
		}
		rendered := placeholderPattern.ReplaceAllStringFunc(line, func(match string) string {
			value, ok := vars[match[1:len(match)-1]]
			if !ok {
				return match
			}
			if escapeHTML {
				return html.EscapeString(value)
			}
			return value
		})
		if rendered = tidyLine(rendered); rendered != "" {
			out = append(out, rendered)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

var emptyBrackets = strings.NewReplacer("()", "", "[]", "")

// ";" is deliberately absent: it ends HTML entities in HTML mode.
const danglingSeparators = " \t·•|,:–—"

func tidyLine(line string) string {
	line = emptyBrackets.Replace(line)
	line = strings.Join(strings.Fields(line), " ")
	line = strings.TrimLeft(line, danglingSeparators)
	return strings.TrimRight(line, danglingSeparators+"-")
}
