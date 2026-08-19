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
	"strings"
	"syscall"
	"time"

	"github.com/DuckFeather10086/ferrite/internal/api"
	"github.com/DuckFeather10086/ferrite/internal/config"
	"github.com/DuckFeather10086/ferrite/internal/epg"
	"github.com/DuckFeather10086/ferrite/internal/hls"
	"github.com/DuckFeather10086/ferrite/internal/netaddr"
	"github.com/DuckFeather10086/ferrite/internal/postprocess"
	"github.com/DuckFeather10086/ferrite/internal/recorder"
	"github.com/DuckFeather10086/ferrite/internal/scan"
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

	// Nothing to watch yet: sweep the band before anything else starts, so
	// a fresh install comes up with a usable channel list instead of
	// failing to load a file that only exists in the author's flat. This
	// runs without the Pool because the Pool does not exist yet and
	// nothing else can be contending for the adapter — the HTTP server is
	// not listening and no schedule has been read.
	if _, err := os.Stat(cfg.ChannelsFile); errors.Is(err, os.ErrNotExist) {
		slog.Info("no channel list yet; scanning for channels before starting",
			"file", cfg.ChannelsFile)
		boot := &scan.Runner{
			DvbrBin:      cfg.DvbrBin,
			ChannelsFile: cfg.ChannelsFile,
			Adapter:      cfg.AdapterList()[0].N,
		}
		if _, err := boot.Run(context.Background(), nil); err != nil {
			// Not fatal on its own: the empty list it leaves behind still
			// loads, the daemon still serves, and POST /api/scan can try
			// again with a better aerial.
			slog.Warn("first-run channel scan did not finish", "err", err)
		}
	}

	channels, err := config.LoadChannels(cfg.ChannelsFile)
	if err != nil {
		return err
	}
	slog.Info("channels loaded", "n", channels.Len(), "file", cfg.ChannelsFile)

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
	adapters := cfg.AdapterList()
	tunerPool := tuner.NewPool(dvbrCLI, channels, adapters, 8)
	slog.Info("tuner pool initialized", "adapters", adapters)

	recRunner := &recorder.Runner{
		Tuners:      tunerPool,
		Store:       st,
		StorageRoot: cfg.StorageRoot,
	}

	// The post-pass: a finished recording becomes an MP4 a browser can play,
	// with subtitle sidecars beside it. Off unless the config asks for it —
	// it is real work on a box whose first job is to keep recording.
	var post *postprocess.Runner
	if cfg.Transcode.Enable {
		post = &postprocess.Runner{
			Store:           st,
			StorageRoot:     cfg.StorageRoot,
			FFmpegBin:       cfg.FFmpegBin,
			CaptionBin:      cfg.AribCaptionBin,
			InputArgs:       cfg.Transcode.InputArgs,
			TranscodeArgs:   cfg.Transcode.OutputArgs,
			AudioOffsetBias: cfg.AudioOffsetBias,
			DeleteSource:    cfg.Transcode.DeleteSource,
		}
		recRunner.OnFinished = post.Enqueue
	}
	sched := &scheduler.Scheduler{
		Store:  st,
		Runner: recRunner,
	}

	// Live segments go wherever hls_root says, falling back to
	// storage_root/hls. On this box that is a tmpfs the unit creates
	// (RuntimeDirectory=ferrite), which keeps a day's worth of
	// write-and-delete off the same disk the recordings are on.
	hlsRoot, err := cfg.LiveRoot()
	if err != nil {
		return err
	}
	hlsMgr := &hls.Manager{
		Tuners:     tunerPool,
		OutputRoot: hlsRoot,
		FFmpegBin:  cfg.FFmpegBin,
		FFprobeBin: cfg.FFprobeBin,
		CaptionBin: cfg.AribCaptionBin,
		// Live quality tiers, and the hardware-decode setup they share.
		// An empty [live] section leaves this nil, which the manager reads
		// as its single built-in tier — the encode it has always done.
		FFmpegArgs:      cfg.Live.InputArgs,
		Qualities:       liveQualities(cfg),
		ProbeSeconds:    cfg.ProbeSeconds,
		AudioOffsetBias: cfg.AudioOffsetBias,
		// Persist the measured A/V skew so a channel only pays the
		// ~5s ffprobe pass once, not on every tune.
		Offsets: st,
		Canonical: func(name string) string {
			if ch := channels.Find(name); ch != nil {
				return ch.Name
			}
			return name
		},
	}

	// The same tiers, for a recording. Under storage_root and not the live
	// tmpfs: a full-length tier is hundreds of megabytes of segments, which
	// belongs on the disk the recording itself is on rather than in RAM.
	vod := &hls.VOD{
		OutputRoot:      filepath.Join(cfg.StorageRoot, "vod"),
		FFmpegBin:       cfg.FFmpegBin,
		FFmpegArgs:      cfg.Live.InputArgs,
		Qualities:       liveQualities(cfg),
		Offsets:         st,
		AudioOffsetBias: cfg.AudioOffsetBias,
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Ad-hoc ("record now") recordings outlive the HTTP request that
	// started them. Base is deliberately *not* the signal context: on
	// SIGTERM that would cancel the job before StopAllAndWait below can
	// ask it to stop gracefully, and the row would land as 'failed' with
	// its bytes already on disk. Shutdown reaches these jobs only
	// through StopAllAndWait.
	adhoc := &recorder.Manager{Runner: recRunner, Base: context.Background()}

	// Rescanning on demand goes through the Pool at background priority,
	// so a sweep — which owns the frontend for ten minutes — yields to
	// anyone who wants to watch television.
	scanner := &scan.Runner{
		DvbrBin:      cfg.DvbrBin,
		ChannelsFile: cfg.ChannelsFile,
		Tuners:       tunerPool,
	}

	handler := api.NewRouter(api.Deps{
		Channels: channels,
		Scanner:  scanner,
		// Where a scan writes, and where the in-memory list is re-read
		// from when one finishes.
		ChannelsFile: cfg.ChannelsFile,
		Store:        st,
		Tuners:       tunerPool,
		HLS:          hlsMgr,
		VOD:          vod,
		Recorder:     adhoc,
		Postprocess:  post,
		// Bounds which files /api/recordings/{id}/file will serve and
		// DELETE will unlink — same root the recorder writes under.
		StorageRoot: cfg.StorageRoot,
		// Lets /api/status report the addresses a viewer can reach the
		// stream at. Only this process can see its own interfaces.
		HTTPPort:  cfg.HTTPPort,
		StartedAt: time.Now(),
		Version:   version,
		Web:       web.FS(),
	})

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

	go func() {
		if err := vod.Run(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("recording transcoder exited", "err", err)
		}
	}()
	slog.Info("recording transcoder started", "dir", vod.OutputRoot)

	if post != nil {
		go func() {
			if err := post.Run(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("postprocess exited", "err", err)
			}
		}()
	}

	// EPG refresher (best-effort: missing dvbr binary or no
	// epg_channels just means no ingest, not a fatal).
	if len(cfg.EPGChannels) > 0 && cfg.DvbrBin != "" {
		refresher := &epg.Refresher{
			DvbrBin:      cfg.DvbrBin,
			ChannelsFile: cfg.ChannelsFile,
			Adapter:      adapters[0].N,
			Channels:     channels,
			ChannelNames: cfg.EPGChannels,
			Store:        st,
			// Route EPG through the pool at background priority: live
			// viewing and recordings evict it instead of dying on dvbr's
			// flock (which is what starved live HLS before).
			Tuners: tunerPool,
		}
		go func() {
			if err := refresher.Run(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("epg refresher exited", "err", err)
			}
		}()
		slog.Info("epg refresher started",
			"channels", cfg.EPGChannels, "adapter", adapters[0].N)
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
		// Every address the stream can be opened from, not just loopback:
		// after a boot on the tuner box this log line is what you read to
		// know what to paste into a phone.
		var watch []string
		for _, a := range netaddr.Addresses(cfg.HTTPPort) {
			watch = append(watch, string(a.Kind)+"="+a.URL(api.StreamPath))
		}
		slog.Info("http listening", "addr", srv.Addr,
			"watch", strings.Join(watch, " "),
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

	// Let in-flight "record now" jobs close their file and write their
	// row before the store goes away (deferred st.Close runs after this
	// function returns). Without the wait the row stays stuck in state
	// 'recording' forever.
	adhoc.StopAllAndWait(5 * time.Second)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	slog.Info("isdbd stopped")
	return nil
}

// liveQualities turns the config's [live.quality.*] tables into the
// manager's tier list. Nil when none are declared, which is how a daemon
// that has not opted into tiers keeps behaving exactly as it did.
func liveQualities(cfg *config.Daemon) []hls.Quality {
	declared := cfg.LiveQualities()
	if len(declared) == 0 {
		return nil
	}
	out := make([]hls.Quality, 0, len(declared))
	for _, q := range declared {
		out = append(out, hls.Quality{
			Name:       q.Name(),
			Label:      q.Label,
			Bandwidth:  q.Bandwidth,
			OutputArgs: q.OutputArgs,
		})
	}
	return out
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
