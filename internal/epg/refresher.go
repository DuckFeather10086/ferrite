// Package epg refreshes the EPG store by periodically running
// `dvbr epg --schedule --json` per channel and upserting events.
//
// Each refresh acquires the DVB adapter (via tuner.Pool, when wired)
// so it serializes against live viewing / recording. Default cron:
// every 6 hours.
package epg

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/DuckFeather10086/isdb-hub/internal/config"
	"github.com/DuckFeather10086/isdb-hub/internal/store"
)

// Refresher periodically runs `dvbr epg --schedule --json` against
// each configured channel and ingests the events into store.
type Refresher struct {
	DvbrBin      string
	ChannelsFile string
	Adapter      int
	Channels     *config.Channels
	ChannelNames []string      // names/aliases to refresh (as appears in channels.json)
	Store        *store.Store
	Interval     time.Duration // 0 = derive from config (default 6h)
}

// RefreshOnce runs one ingest pass over ChannelNames. Returns the
// number of events upserted across all channels.
func (r *Refresher) RefreshOnce(ctx context.Context) (int, error) {
	if r.Store == nil {
		return 0, fmt.Errorf("epg: Store is nil")
	}
	if r.DvbrBin == "" || r.ChannelsFile == "" {
		return 0, fmt.Errorf("epg: DvbrBin / ChannelsFile required")
	}
	total := 0
	for _, name := range r.ChannelNames {
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}

		n, err := r.refreshOne(ctx, name)
		if err != nil {
			slog.Warn("epg refresh failed", "channel", name, "err", err)
			continue
		}
		slog.Info("epg refreshed", "channel", name, "events", n)
		total += n
	}
	return total, nil
}

func (r *Refresher) refreshOne(ctx context.Context, channelName string) (int, error) {
	ch := r.Channels.Find(channelName)
	if ch == nil {
		return 0, fmt.Errorf("channel %q not found in channels.json", channelName)
	}
	sid := ch.ServiceID()
	if sid == 0 {
		return 0, fmt.Errorf("channel %q has no SERVICE_ID", channelName)
	}

	args := []string{
		"epg",
		"--adapter", strconv.Itoa(r.Adapter),
		"--channels", r.ChannelsFile,
		"--schedule",
		"--json",
		channelName,
	}
	// Subprocess is bounded: an EPG collection should finish in well
	// under MAX_EPG_COLLECT_SECS (60s in dvbr); give some headroom.
	cmdCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, r.DvbrBin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("dvbr epg: %w (stderr: %s)", err, stderr.String())
	}

	events, skipped := Parse(&stdout, sid)
	for _, sk := range skipped {
		slog.Debug("epg event skipped", "channel", channelName, "err", sk)
	}
	if len(events) == 0 {
		return 0, nil
	}
	if err := r.Store.UpsertEPGEvents(ctx, events); err != nil {
		return 0, fmt.Errorf("upsert: %w", err)
	}
	return len(events), nil
}

// Run drives RefreshOnce on r.Interval (default 6h). Blocks until
// ctx is canceled. Safe to call once; calling concurrently is not
// supported.
func (r *Refresher) Run(ctx context.Context) error {
	interval := r.Interval
	if interval <= 0 {
		interval = 6 * time.Hour
	}

	// First pass immediately on startup, then on the ticker.
	if _, err := r.RefreshOnce(ctx); err != nil && ctx.Err() == nil {
		slog.Warn("initial epg refresh failed", "err", err)
	}

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if _, err := r.RefreshOnce(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("epg refresh tick failed", "err", err)
			}
		}
	}
}

// stubRunner is the testing seam: lets tests inject a fake "dvbr"
// without spawning a subprocess. Not yet used in production code; the
// hook is here for the integration tests that will follow.
var _ = sync.Mutex{}
