// Command gateway runs the cost-aware LLM gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	_ "net/http/pprof" // served only on GATEWAY_PPROF (a loopback address)
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/app"
)

var version = "dev" // set with -ldflags "-X main.version=..."

func main() {
	cfgPath := flag.String("config", envOr("GATEWAY_CONFIG", "gateway.yaml"), "config file or directory")
	check := flag.Bool("check", false, "validate the configuration and exit")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if *check {
		if _, err := app.LoadConfig(*cfgPath); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("configuration OK")
		return
	}
	log.Info("starting", "version", version)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.New(ctx, app.Options{ConfigPath: *cfgPath, Log: log})
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
	if addr := os.Getenv("GATEWAY_PPROF"); addr != "" {
		// Profiling for operators; never on the public listener.
		go func() {
			log.Info("pprof listening", "addr", addr, "err", http.ListenAndServe(addr, http.DefaultServeMux))
		}()
	}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			if err := a.Reload(context.Background(), "sighup"); err != nil {
				log.Error("reload rejected", "err", err)
			}
		}
	}()
	if err := a.Serve(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server error", "err", err)
		os.Exit(1)
	}
	log.Info("shutdown complete", "at", time.Now().UTC())
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
