package main

import (
	"context"
	_ "embed"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimedefault"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimehost"
)

//go:embed manifest.json
var manifestJSON []byte

//go:embed web/index.html
var indexHTML []byte

// Plugin holds the state shared by the runtime, event and HTTP servers.
type Plugin struct {
	logger   *slog.Logger
	store    *profileStore
	pushover *pushoverClient
	silo     *siloClient
	fetch    *http.Client // poster downloads
	// handled records the events already sent or permanently rejected.
	handled *eventLog

	cfgMu sync.RWMutex
	cfg   adminConfig

	hostInfoFn func(ctx context.Context) (*runtimehost.HostInfo, error)
	hostMu     sync.Mutex
	hostInfo   *runtimehost.HostInfo
	hostAt     time.Time

	// resolveImagesFn turns catalog image paths into URLs (the host's
	// ResolveCatalogImageURLs).
	resolveImagesFn func(ctx context.Context, paths []string, variant string) (map[string]string, error)

	// eventTimeout bounds one HandleEvent call. Silo gives each attempt 10 s.
	eventTimeout time.Duration
	// posterTimeout bounds the poster lookup and download for one message.
	posterTimeout time.Duration
}

const (
	defaultEventTimeout  = 8 * time.Second
	defaultPosterTimeout = 3 * time.Second
)

func newPlugin(logger *slog.Logger, state stateBackend) *Plugin {
	return &Plugin{
		logger:          logger,
		store:           newProfileStore(state),
		pushover:        newPushoverClient(),
		silo:            newSiloClient(),
		fetch:           &http.Client{Timeout: defaultPosterTimeout},
		handled:         newEventLog(state),
		hostInfoFn:      sdkHostInfo,
		resolveImagesFn: sdkResolveImages,
		eventTimeout:    defaultEventTimeout,
		posterTimeout:   defaultPosterTimeout,
	}
}

func (p *Plugin) config() adminConfig {
	p.cfgMu.RLock()
	defer p.cfgMu.RUnlock()
	return p.cfg
}

func (p *Plugin) setConfig(cfg adminConfig) {
	p.cfgMu.Lock()
	p.cfg = cfg
	p.cfgMu.Unlock()
	p.silo.Invalidate()
}

// host returns the cached host info, refreshing it every minute. Public URLs
// can change with a Silo config reload.
func (p *Plugin) host(ctx context.Context) (*runtimehost.HostInfo, error) {
	p.hostMu.Lock()
	defer p.hostMu.Unlock()
	if p.hostInfo != nil && time.Since(p.hostAt) < time.Minute {
		return p.hostInfo, nil
	}
	info, err := p.hostInfoFn(ctx)
	if err != nil {
		if p.hostInfo != nil {
			return p.hostInfo, nil
		}
		return nil, err
	}
	p.hostInfo, p.hostAt = info, time.Now()
	return info, nil
}

func sdkHostInfo(ctx context.Context) (*runtimehost.HostInfo, error) {
	h := sdkruntime.Host()
	if h == nil {
		return nil, errHostUnavailable
	}
	return h.GetHostInfo(ctx)
}

func sdkResolveImages(ctx context.Context, paths []string, variant string) (map[string]string, error) {
	h := sdkruntime.Host()
	if h == nil {
		return nil, errHostUnavailable
	}
	return h.ResolveCatalogImageURLs(ctx, paths, variant)
}

type runtimeServer struct {
	runtimedefault.Server
	manifest *pluginv1.PluginManifest
	plugin   *Plugin
}

func (s *runtimeServer) GetManifest(context.Context, *pluginv1.GetManifestRequest) (*pluginv1.GetManifestResponse, error) {
	return &pluginv1.GetManifestResponse{Manifest: s.manifest}, nil
}

func (s *runtimeServer) Configure(_ context.Context, req *pluginv1.ConfigureRequest) (*pluginv1.ConfigureResponse, error) {
	cfg := parseAdminConfig(req.GetConfig())
	s.plugin.setConfig(cfg)
	if cfg.LegacyKeyStored {
		// Only the fact, never the value.
		s.plugin.logger.Info("the unscoped Silo API key saved by version 0.2.0 is still stored with this plugin's settings; it is ignored. Revoke it under Admin > API Keys.")
	}
	return &pluginv1.ConfigureResponse{}, nil
}

func main() {
	manifest, err := publicmanifest.LoadWithChecksum(manifestJSON, "")
	if err != nil {
		panic(err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("plugin", manifest.GetPluginId())
	plugin := newPlugin(logger, hostStateBackend{})

	sdkruntime.Serve(sdkruntime.ServeConfig{
		Servers: sdkruntime.CapabilityServers{
			Runtime:       &runtimeServer{manifest: manifest, plugin: plugin},
			EventConsumer: &eventServer{plugin: plugin},
			HttpRoutes:    &routesServer{plugin: plugin},
		},
	})
}
