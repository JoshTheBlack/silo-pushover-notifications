package main

import (
	"log/slog"
	"strings"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// adminConfigKey is the global_config_schema key in manifest.json.
const adminConfigKey = "pushover"

// legacyAPIKeyField is the unscoped admin key 0.2.0 asked for. Silo keeps a
// saved value after an upgrade (an admin save merges over the stored row and
// can't clear undeclared fields), so it still arrives in Configure. Its value
// is never read.
const legacyAPIKeyField = "silo_api_key"

// adminConfig is the server-wide configuration an admin sets under
// Admin > Plugins > Pushover Notifications.
type adminConfig struct {
	// MonitorAPIKey is an optional Silo API key with the admin:monitor:read
	// scope. With it, a primary profile can manage every profile on its
	// account.
	MonitorAPIKey string
	// ControlAPIKey is an optional Silo API key with the
	// admin:sessions:control scope, for the stop-stream page.
	ControlAPIKey string
	// DefaultAppToken is an optional shared Pushover application token for
	// profiles that don't bring their own.
	DefaultAppToken string
	// PublicURL overrides Silo's configured public URL for item links.
	PublicURL string
	// LegacyKeyStored reports that 0.2.0's silo_api_key is still saved.
	LegacyKeyStored bool
}

// LogValue keeps keys out of logs if a config is ever logged.
func (c adminConfig) LogValue() slog.Value {
	set := func(s string) bool { return s != "" }
	return slog.GroupValue(
		slog.Bool("monitor_api_key_set", set(c.MonitorAPIKey)),
		slog.Bool("control_api_key_set", set(c.ControlAPIKey)),
		slog.Bool("default_app_token_set", set(c.DefaultAppToken)),
		slog.String("public_url", c.PublicURL),
		slog.Bool("legacy_key_stored", c.LegacyKeyStored),
	)
}

func parseAdminConfig(entries []*pluginv1.ConfigEntry) adminConfig {
	var cfg adminConfig
	for _, entry := range entries {
		if entry.GetKey() != adminConfigKey {
			continue
		}
		fields := entry.GetValue().GetFields()
		str := func(key string) string { return strings.TrimSpace(fields[key].GetStringValue()) }
		cfg.MonitorAPIKey = str("monitor_api_key")
		cfg.ControlAPIKey = str("control_api_key")
		cfg.DefaultAppToken = str("default_app_token")
		cfg.PublicURL = strings.TrimRight(str("public_url"), "/")
		cfg.LegacyKeyStored = str(legacyAPIKeyField) != ""
	}
	return cfg
}
