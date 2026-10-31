// Package app assembles the gateway process: configuration, snapshot, engine, HTTP
// listeners, discovery, reload and graceful shutdown.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/config"
	"github.com/SureshKhemka/llm-gateway/internal/gateway"
	"github.com/SureshKhemka/llm-gateway/internal/observe"
	"github.com/SureshKhemka/llm-gateway/internal/server"
	"github.com/SureshKhemka/llm-gateway/internal/store"
)

type Options struct {
	ConfigPath string
	Log        *slog.Logger
	Store      store.Store
	Factory    gateway.ProviderFactory
}

type App struct {
	opts      Options
	log       *slog.Logger
	Engine    *gateway.Engine
	Server    *server.Server
	Discovery *gateway.Discovery
	Store     store.Store

	mu       sync.Mutex
	fileStat map[string]time.Time
}

func LoadConfig(path string) (*config.Config, error) { return config.Load(path) }

func New(ctx context.Context, o Options) (*App, error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	cfg, err := config.Load(o.ConfigPath)
	if err != nil {
		return nil, err
	}
	a := &App{opts: o, log: o.Log, Discovery: gateway.NewDiscovery(), fileStat: map[string]time.Time{}}
	a.Store = o.Store
	if a.Store == nil {
		a.Store = store.NewMemory()
	}
	snap, err := gateway.BuildSnapshot(gateway.BuildInput{Config: cfg, Factory: o.Factory})
	if err != nil {
		return nil, err
	}
	var sinks observe.Multi
	if cfg.Observability.LogRequests == nil || *cfg.Observability.LogRequests {
		sinks = append(sinks, observe.NewJSONLogger(os.Stdout))
	}
	a.Engine = gateway.NewEngine(snap, gateway.EngineOptions{Store: a.Store, Events: sinks, Log: o.Log, WALDir: cfg.Storage.WALDir})
	a.Server = server.New(a.Engine, o.Log)
	a.Server.Ready = a.ready
	a.recordFileStats(cfg.Files)
	a.discover(ctx)
	return a, nil
}

func (a *App) ready() (bool, string) {
	s := a.Engine.Snapshot()
	if s == nil {
		return false, "configuration not loaded"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, p := range s.Providers.All() {
		if _, err := p.ListModels(ctx); err == nil {
			return true, ""
		}
	}
	return false, "no provider reachable"
}

// discover lists models and rebuilds the snapshot if anything new appeared.
func (a *App) discover(ctx context.Context) {
	s := a.Engine.Snapshot()
	before := len(a.Discovery.List())
	found := a.Discovery.Run(ctx, s)
	if len(found) == before && before > 0 {
		return
	}
	if err := a.rebuild(s.Config, "discovery"); err != nil {
		a.log.Warn("snapshot rebuild after discovery failed", "err", err)
	}
}

func (a *App) rebuild(cfg *config.Config, reason string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	prev := a.Engine.Snapshot()
	keys, _ := a.Store.ListKeys(context.Background(), "")
	pols, _ := a.Store.ListPolicyVersions(context.Background(), "")
	snap, err := gateway.BuildSnapshot(gateway.BuildInput{
		Config: cfg, APIKeys: keys, PolicyVers: pols, Discovered: a.Discovery.List(),
		Factory: a.opts.Factory, PrevVersion: prev.Version, Reuse: prev.Providers,
	})
	if err != nil {
		return err
	}
	a.Engine.Swap(snap)
	a.log.Info("snapshot swapped", "version", snap.Version, "reason", reason, "deployments", len(snap.Catalog.Deployments))
	return nil
}

// Reload re-reads configuration; an invalid file leaves the running state untouched
// [CFG-02, CFG-03].
func (a *App) Reload(ctx context.Context, reason string) error {
	cfg, err := config.Load(a.opts.ConfigPath)
	if err != nil {
		return err
	}
	a.recordFileStats(cfg.Files)
	return a.rebuild(cfg, reason)
}

func (a *App) recordFileStats(files []string) {
	for _, f := range files {
		if st, err := os.Stat(f); err == nil {
			a.fileStat[f] = st.ModTime()
		}
	}
}

func (a *App) filesChanged() bool {
	for f, mt := range a.fileStat {
		if st, err := os.Stat(f); err != nil || !st.ModTime().Equal(mt) {
			return true
		}
	}
	return false
}

// background runs the config poller and periodic discovery.
func (a *App) background(ctx context.Context) {
	cfg := a.Engine.Snapshot().Config
	poll := time.NewTicker(cfg.Server.ReloadEvery.D())
	disc := time.NewTicker(30 * time.Second)
	defer poll.Stop()
	defer disc.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			if a.filesChanged() {
				if err := a.Reload(ctx, "file_change"); err != nil {
					a.log.Error("reload rejected; keeping running configuration", "err", err)
					a.recordFileStats(a.Engine.Snapshot().Config.Files)
				}
			}
		case <-disc.C:
			a.discover(ctx)
		}
	}
}

// Serve runs the HTTP listeners until ctx is done, then drains [RES-10].
func (a *App) Serve(ctx context.Context) error {
	cfg := a.Engine.Snapshot().Config
	go a.background(ctx)
	srv := &http.Server{Addr: cfg.Server.Listen, Handler: a.Server.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() {
		a.log.Info("listening", "addr", cfg.Server.Listen)
		if cfg.Server.TLSCertFile != "" {
			errc <- srv.ListenAndServeTLS(cfg.Server.TLSCertFile, cfg.Server.TLSKeyFile)
		} else {
			errc <- srv.ListenAndServe()
		}
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	a.log.Info("draining", "in_flight", a.Server.InFlight(), "timeout", cfg.Server.DrainTimeout.D().String())
	dctx, cancel := context.WithTimeout(context.Background(), cfg.Server.DrainTimeout.D())
	defer cancel()
	if err := a.Server.Drain(dctx); err != nil {
		a.log.Warn("drain timed out", "in_flight", a.Server.InFlight())
	}
	_ = srv.Shutdown(dctx)
	a.Engine.Close()
	if err := a.Store.Close(); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("store close: %w", err)
	}
	return nil
}
