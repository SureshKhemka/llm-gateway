// Package app assembles the gateway process: configuration, storage, snapshot, engine,
// HTTP listeners, discovery, probes, reload and graceful shutdown.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/SureshKhemka/llm-gateway/internal/admin"
	"github.com/SureshKhemka/llm-gateway/internal/config"
	"github.com/SureshKhemka/llm-gateway/internal/gateway"
	"github.com/SureshKhemka/llm-gateway/internal/governance"
	"github.com/SureshKhemka/llm-gateway/internal/observe"
	"github.com/SureshKhemka/llm-gateway/internal/server"
	"github.com/SureshKhemka/llm-gateway/internal/store"
)

type Options struct {
	ConfigPath string
	Log        *slog.Logger
	Store      store.Store
	Counters   governance.Counters
	Factory    gateway.ProviderFactory
}

type App struct {
	opts   Options
	log    *slog.Logger
	Engine *gateway.Engine
	Server *server.Server
	Admin  *admin.API
	Store  store.Store
	redis  redis.UniversalClient

	mu       sync.Mutex
	fileStat map[string]time.Time
	fileCfg  *config.Config
}

func LoadConfig(path string) (*config.Config, error) { return config.Load(path) }

// optionalSecret resolves a storage reference; an unset environment variable means
// "not configured" (single-node development mode).
func optionalSecret(ref config.SecretRef) (string, bool, error) {
	if ref.IsZero() {
		return "", false, nil
	}
	v, err := ref.Resolve()
	if err != nil {
		if strings.HasPrefix(string(ref), "env:") && strings.Contains(err.Error(), "is not set") {
			return "", false, nil
		}
		return "", false, err
	}
	return v, v != "", nil
}

func New(ctx context.Context, o Options) (*App, error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	cfg, err := config.Load(o.ConfigPath)
	if err != nil {
		return nil, err
	}
	a := &App{opts: o, log: o.Log, fileStat: map[string]time.Time{}, fileCfg: cfg}

	a.Store = o.Store
	if a.Store == nil {
		dsn, ok, err := optionalSecret(cfg.Storage.PostgresDSN)
		if err != nil {
			return nil, fmt.Errorf("storage.postgres_dsn: %w", err)
		}
		if ok {
			pg, err := store.OpenPostgres(ctx, dsn)
			if err != nil {
				return nil, fmt.Errorf("postgres: %w", err)
			}
			a.Store = pg
			o.Log.Info("using postgres store")
		} else {
			a.Store = store.NewMemory()
			o.Log.Warn("storage.postgres_dsn not set: records are kept in memory (single-node development mode)")
		}
	}
	counters := o.Counters
	if counters == nil {
		url, ok, err := optionalSecret(cfg.Storage.RedisURL)
		if err != nil {
			return nil, fmt.Errorf("storage.redis_url: %w", err)
		}
		if ok {
			opt, err := redis.ParseURL(url)
			if err != nil {
				return nil, fmt.Errorf("storage.redis_url: %w", err)
			}
			a.redis = redis.NewClient(opt)
			counters = governance.NewRedisCounters(a.redis, "gw:")
			o.Log.Info("using redis for budgets, rate limits and cache")
		} else {
			counters = governance.NewMemCounters()
			o.Log.Warn("storage.redis_url not set: budgets and limits are per instance (single-node development mode)")
		}
	}

	snap, err := gateway.BuildSnapshot(gateway.BuildInput{Config: cfg, Factory: o.Factory})
	if err != nil {
		return nil, err
	}
	var sinks observe.Multi
	if cfg.Observability.LogRequests == nil || *cfg.Observability.LogRequests {
		sinks = append(sinks, observe.NewJSONLogger(os.Stdout))
	}
	a.Engine = gateway.NewEngine(snap, gateway.EngineOptions{Store: a.Store, Events: sinks, Log: o.Log,
		WALDir: cfg.Storage.WALDir, Counters: counters, Factory: o.Factory})
	a.Engine.Gov.WatchRevocations(ctx)
	if err := a.Engine.Rebuild(ctx, cfg, "startup"); err != nil {
		return nil, err
	}
	a.Server = server.New(a.Engine, o.Log)
	a.Server.Ready = a.ready
	a.Admin = admin.New(a.Engine)
	a.Admin.Reload = a.Reload
	if cfg.Server.AdminListen == "" {
		a.Admin.Mount(a.Server.Mux)
	}
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
	before := len(a.Engine.Discovery.List())
	found := a.Engine.Discovery.Run(ctx, a.Engine.Snapshot())
	if len(found) == before {
		return
	}
	if err := a.Engine.Rebuild(ctx, nil, "discovery"); err != nil {
		a.log.Warn("snapshot rebuild after discovery failed", "err", err)
	}
}

// Reload re-reads configuration files; an invalid file leaves the running state
// untouched [CFG-02, CFG-03].
func (a *App) Reload(ctx context.Context, reason string) error {
	cfg, err := config.Load(a.opts.ConfigPath)
	if err != nil {
		return err
	}
	if err := a.Engine.Rebuild(ctx, cfg, reason); err != nil {
		return err
	}
	a.mu.Lock()
	a.fileCfg = cfg
	a.mu.Unlock()
	a.recordFileStats(cfg.Files)
	return nil
}

func (a *App) recordFileStats(files []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, f := range files {
		if st, err := os.Stat(f); err == nil {
			a.fileStat[f] = st.ModTime()
		}
	}
}

func (a *App) filesChanged() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for f, mt := range a.fileStat {
		if st, err := os.Stat(f); err != nil || !st.ModTime().Equal(mt) {
			return true
		}
	}
	return false
}

// background polls config files, refreshes admin-API state written by other instances
// (keys, budgets: applied within seconds, BUD-06), discovers models and rolls up old
// cost records.
func (a *App) background(ctx context.Context) {
	cfg := a.Engine.Snapshot().Config
	poll := time.NewTicker(cfg.Server.ReloadEvery.D())
	disc := time.NewTicker(30 * time.Second)
	rollup := time.NewTicker(6 * time.Hour)
	defer poll.Stop()
	defer disc.Stop()
	defer rollup.Stop()
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
			} else if err := a.Engine.Rebuild(ctx, nil, "refresh"); err != nil {
				a.log.Warn("refresh failed", "err", err)
			}
		case <-disc.C:
			a.discover(ctx)
		case <-rollup.C:
			if pg, ok := a.Store.(*store.Postgres); ok {
				if n, err := pg.Rollup(ctx, 395*24*time.Hour); err != nil {
					a.log.Error("cost rollup failed", "err", err)
				} else if n > 0 {
					a.log.Info("rolled up old cost records", "rows", n)
				}
			}
		}
	}
}

// Serve runs the HTTP listeners until ctx is done, then drains [RES-10].
func (a *App) Serve(ctx context.Context) error {
	cfg := a.Engine.Snapshot().Config
	go a.background(ctx)
	go a.Engine.RunProbes(ctx, 15*time.Second)
	srv := &http.Server{Addr: cfg.Server.Listen, Handler: a.Server.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 2)
	serve := func(s *http.Server) {
		a.log.Info("listening", "addr", s.Addr)
		if cfg.Server.TLSCertFile != "" {
			errc <- s.ListenAndServeTLS(cfg.Server.TLSCertFile, cfg.Server.TLSKeyFile)
		} else {
			errc <- s.ListenAndServe()
		}
	}
	go serve(srv)
	var adminSrv *http.Server
	if cfg.Server.AdminListen != "" {
		// A separate listener keeps the admin API off the client-facing port [NFR-11].
		mux := http.NewServeMux()
		a.Admin.Mount(mux)
		adminSrv = &http.Server{Addr: cfg.Server.AdminListen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go serve(adminSrv)
	}
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
	if adminSrv != nil {
		_ = adminSrv.Shutdown(dctx)
	}
	a.Engine.Close()
	if a.redis != nil {
		a.redis.Close()
	}
	if err := a.Store.Close(); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("store close: %w", err)
	}
	return nil
}
