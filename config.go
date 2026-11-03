package main

import (
	"strings"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// adminConfigKey is the global_config_schema key in manifest.json.
const adminConfigKey = "pushover"

// adminConfig is the server-wide configuration an admin sets under
// Admin > Plugins > Pushover.
type adminConfig struct {
	// SiloAPIKey is an unscoped admin API key. The plugin needs it to list the
	// signed-in account's profiles: the plugin proxy only tells us the user ID
	// and the active profile's name, while notifications are keyed by profile ID.
	SiloAPIKey string
	// DefaultAppToken is an optional shared Pushover application token for
	// profiles that don't bring their own.
	DefaultAppToken string
	// PublicURL overrides Silo's configured public URL for item links.
	PublicURL string
}

func parseAdminConfig(entries []*pluginv1.ConfigEntry) adminConfig {
	var cfg adminConfig
	for _, entry := range entries {
		if entry.GetKey() != adminConfigKey {
			continue
		}
		fields := entry.GetValue().GetFields()
		cfg.SiloAPIKey = strings.TrimSpace(fields["silo_api_key"].GetStringValue())
		cfg.DefaultAppToken = strings.TrimSpace(fields["default_app_token"].GetStringValue())
		cfg.PublicURL = strings.TrimRight(strings.TrimSpace(fields["public_url"].GetStringValue()), "/")
	}
	return cfg
}
