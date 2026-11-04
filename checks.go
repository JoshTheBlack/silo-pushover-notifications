package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// checkTimeout bounds one settings check. The host runs it when an admin
// presses Check Connection on the plugin's settings page.
const checkTimeout = 8 * time.Second

// checkServer is the plugin's ConfigurationChecks service. Silo (build
// d10d9c93) doesn't call it yet; hosts that predate the service ignore the
// manifest's configuration_check key, so it costs nothing when never called.
type checkServer struct {
	pluginv1.UnimplementedConfigurationChecksServer
	plugin *Plugin
}

// TestConfiguration checks the staged, unsaved settings it is given, never
// the running ones, and saves nothing. The shared Pushover application token
// is tested with Pushover's limits endpoint, which also gives its monthly
// allowance. A failed check is an answer with ok false, with a message that
// never includes a secret.
func (s *checkServer) TestConfiguration(ctx context.Context, req *pluginv1.TestConfigurationRequest) (*pluginv1.TestConfigurationResponse, error) {
	cfg := parseAdminConfig(req.GetConfig())
	fail := func(msg string) (*pluginv1.TestConfigurationResponse, error) {
		return &pluginv1.TestConfigurationResponse{Ok: false, Message: msg}, nil
	}

	if cfg.PublicURL != "" {
		if u, err := url.Parse(cfg.PublicURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fail("The public URL override must be a web address like https://silo.example.com.")
		}
	}
	token := cfg.DefaultAppToken
	if token == "" {
		// The token is optional: every profile can bring its own.
		return &pluginv1.TestConfigurationResponse{Ok: true}, nil
	}
	if !pushoverKeyPattern.MatchString(token) {
		return fail("The shared Pushover application token should be 30 letters and numbers. Copy it again from pushover.net/apps.")
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	q, err := s.plugin.pushover.Limits(ctx, token)
	if err != nil {
		var perr *pushoverError
		switch {
		case errors.As(err, &perr) && perr.HTTPStatus != 0 && !perr.Retryable:
			return fail("Pushover rejected the shared application token: " + strings.Join(perr.Errors, "; ") + " Check the token at pushover.net/apps.")
		default:
			return fail("Couldn't reach Pushover to check the shared application token. Try again in a moment.")
		}
	}
	return &pluginv1.TestConfigurationResponse{
		Ok:      true,
		Message: fmt.Sprintf("Pushover accepted the shared application token. %d of its %d messages this month are left.", q.Remaining, q.Limit),
	}, nil
}
