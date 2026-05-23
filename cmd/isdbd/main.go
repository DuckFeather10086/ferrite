// Command isdbd is the ISDB-T tuner / EPG / recording daemon.
//
// What's wired today (and what isn't):
//   ✓ config load + channels.json load
//   ✓ sqlite store with migrations
//   ✓ HTTP API (chi) on configured port, graceful shutdown
//   ✗ tuner.Pool / fanout / hls.Manager / recorder / scheduler / epg
//     refresher — packages exist but Run methods still panic.
//
// You can curl /api/status, /api/channels, /api/epg etc. against this
// — they answer with empty or 503 until the runtime pieces are wired.
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
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/DuckFeather10086/isdbd/internal/api"
	"github.com/DuckFeather10086/isdbd/internal/config"
	"github.com/DuckFeather10086/isdbd/internal/epg"
	"github.com/DuckFeather10086/isdbd/internal/store"
)

// Set via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var (
		cfgPath  = flag.String("config", "configs/isdbd.toml", "path to daemon config")
		logLevel = flag.String("log-level", "info", "debug|info|warn|error")
	)
	flag.Parse()

	if err := run(*cfgPath, *logLevel); err != nil {
		slog.Error("isdbd failed", "err", err)
		os.Exit(1)
	}
}

func run(cfgPath, logLevel string) error {
	setupLogger(logLevel)
	slog.Info("isdbd starting", "version", version, "config", cfgPath)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	channels, err := config.LoadChannels(cfg.ChannelsFile)
	if err != nil {
		return err
	}
	slog.Info("channels loaded", "n", len(channels.Channels), "file", cfg.ChannelsFile)

	dbPath, err := cfg.StoragePath("isdbd.db")
	if err != nil {
		return fmt.Errorf("resolve db path: %w", err)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	slog.Info("store opened", "path", dbPath)

	handler := api.NewRouter(api.Deps{
		Channels:  channels,
		Store:     st,
		StartedAt: time.Now(),
		Version:   version,
	})

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// EPG refresher (best-effort: missing dvbr binary or no
	// epg_channels just means no ingest, not a fatal).
	if len(cfg.EPGChannels) > 0 && cfg.DvbrBin != "" {
		refresher := &epg.Refresher{
			DvbrBin:      cfg.DvbrBin,
			ChannelsFile: cfg.ChannelsFile,
			Adapter:      cfg.Adapters[0],
			Channels:     channels,
			ChannelNames: cfg.EPGChannels,
			Store:        st,
		}
		go func() {
			if err := refresher.Run(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("epg refresher exited", "err", err)
			}
		}()
		slog.Info("epg refresher started",
			"channels", cfg.EPGChannels, "adapter", cfg.Adapters[0])
	} else {
		slog.Info("epg refresher disabled (no epg_channels or no dvbr_bin)")
	}

	srv := &http.Server{
		Addr:         ":" + strconv.Itoa(cfg.HTTPPort),
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("http listening", "addr", srv.Addr,
			"local", "http://localhost"+srv.Addr,
			"endpoints", "/health /api/status /api/channels /api/epg /api/schedule /api/recordings")
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("http: %w", err)
	case <-ctx.Done():
		slog.Info("shutdown signal received", "sig", ctx.Err())
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	slog.Info("isdbd stopped")
	return nil
}

func setupLogger(level string) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: lvl,
	})))
}

// Compile-time anchor: ensure filepath is imported even if main moves.
var _ = filepath.Join
