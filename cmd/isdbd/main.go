// Command isdbd is the ISDB-T tuner / EPG / recording daemon.
//
// Wired today:
//
//	✓ config + channels.json load
//	✓ sqlite store with migrations
//	✓ HTTP API (chi) + embedded web UI, graceful shutdown
//	✓ tuner.Pool (dvbr | b25) / fanout / hls.Manager / recorder /
//	  scheduler / epg refresher
//
// The runtime pieces spawn dvbr/b25/ffmpeg on demand, so the daemon
// runs (and serves the API + UI) even without a tuner attached —
// tune-dependent endpoints just fail loudly when no adapter answers.
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

	"github.com/DuckFeather10086/ferrite/internal/api"
	"github.com/DuckFeather10086/ferrite/internal/config"
	"github.com/DuckFeather10086/ferrite/internal/epg"
	"github.com/DuckFeather10086/ferrite/internal/hls"
	"github.com/DuckFeather10086/ferrite/internal/recorder"
	"github.com/DuckFeather10086/ferrite/internal/scheduler"
	"github.com/DuckFeather10086/ferrite/internal/store"
	"github.com/DuckFeather10086/ferrite/internal/tuner"
	"github.com/DuckFeather10086/ferrite/internal/web"
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

	dvbrCLI := &tuner.DvbrCLI{
		BinPath:      cfg.DvbrBin,
		B25Bin:       cfg.B25Bin,
		ChannelsFile: cfg.ChannelsFile,
	}
	tunerPool := tuner.NewPool(dvbrCLI, channels, cfg.Adapters, 8)
	slog.Info("tuner pool initialized", "adapters", cfg.Adapters)

	recRunner := &recorder.Runner{
		Tuners:      tunerPool,
		Store:       st,
		StorageRoot: cfg.StorageRoot,
	}
	sched := &scheduler.Scheduler{
		Store:  st,
		Runner: recRunner,
	}

	hlsRoot, _ := cfg.StoragePath("hls")
	hlsMgr := &hls.Manager{
		Tuners:          tunerPool,
		OutputRoot:      hlsRoot,
		FFmpegBin:       cfg.FFmpegBin,
		FFprobeBin:      cfg.FFprobeBin,
		ProbeSeconds:    cfg.ProbeSeconds,
		AudioOffsetBias: cfg.AudioOffsetBias,
	}

	handler := api.NewRouter(api.Deps{
		Channels:  channels,
		Store:     st,
		Tuners:    tunerPool,
		HLS:       hlsMgr,
		StartedAt: time.Now(),
		Version:   version,
		Web:       web.FS(),
	})

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := sched.Run(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("scheduler exited", "err", err)
		}
	}()
	slog.Info("scheduler started")

	go func() {
		if err := hlsMgr.Run(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("hls manager exited", "err", err)
		}
	}()
	slog.Info("hls manager started", "dir", hlsRoot)

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
		Addr:        ":" + strconv.Itoa(cfg.HTTPPort),
		Handler:     handler,
		ReadTimeout: 10 * time.Second,
		// WriteTimeout must cover the slowest handler: a cold
		// /api/live/{ch}.m3u8 blocks through the frontend lock timeout
		// (~25s) plus waiting for ffmpeg's first playlist write (~30s)
		// before it can respond.
		WriteTimeout: 120 * time.Second,
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
