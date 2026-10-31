// Command gateway runs the cost-aware LLM gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/app"
)

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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.New(ctx, app.Options{ConfigPath: *cfgPath, Log: log})
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
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
