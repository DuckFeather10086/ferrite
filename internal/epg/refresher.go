// Package epg refreshes the EPG store by periodically running
// `dvbr epg --schedule --json` per channel and upserting events.
//
// EPG is the lowest-priority consumer of the tuner. It drives the
// hardware out-of-process (dvbr does its own tune plus an EIT PES tap),
// so it cannot share a fanout with live viewing — it needs the adapter
// to itself. To keep that visible to the rest of the daemon it takes a
// tuner.Reservation at tuner.PrioBackground, which any live or
// recording claim preempts: on preemption the dvbr child is killed, the
// adapter is handed back, and the pass is retried later.
//
// Without that reservation (the pre-2026-07 behaviour) `dvbr epg` grabbed
// the flock on /tmp/dvbr-adapter{N}.lock behind tuner.Pool's back, and a
// concurrent live request died with "DVB adapter 0 is already in use" —
// EIT --schedule scans run for minutes, so the whole window was blocked.
//
// Default cron: every 6 hours.
package epg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/DuckFeather10086/ferrite/internal/config"
	"github.com/DuckFeather10086/ferrite/internal/store"
	"github.com/DuckFeather10086/ferrite/internal/tuner"
)

// Timing defaults.
const (
	// defaultInterval between full refresh passes.
	defaultInterval = 6 * time.Hour
	// defaultStartupDelay keeps the boot-time pass from racing the first
	// live request for the adapter. Preemption would resolve that race
	// correctly anyway, but not tuning at all beats tuning and being
	// immediately evicted.
	defaultStartupDelay = 15 * time.Second
	// defaultRetryAfterPreempt is how long to wait before retrying a
	// pass that got evicted, instead of idling until the next tick.
	defaultRetryAfterPreempt = 10 * time.Minute
	// collectTimeout bounds one `dvbr epg` child. dvbr caps its own
	// collection at MAX_EPG_COLLECT_SECS (60s); this is headroom.
	collectTimeout = 2 * time.Minute
	// childWaitDelay bounds cmd.Wait after the child is killed. Without
	// it, anything still holding the inherited stdout pipe (a helper
	// process dvbr spawned) keeps Wait blocked — which would keep the
	// reservation held and make the preemptor time out waiting for an
	// adapter that is, for practical purposes, already free.
	childWaitDelay = 3 * time.Second
)

// ErrPreempted reports that a refresh gave the adapter back to a
// higher-priority claim before finishing.
var ErrPreempted = errors.New("epg: preempted by a higher-priority tuner claim")

// Reserver is the tuner.Pool seam. Nil disables reservations, which is
// only appropriate when nothing else can touch the adapter.
type Reserver interface {
	Reserve(ctx context.Context, prio tuner.Priority, system string) (*tuner.Reservation, error)
}

// Refresher periodically runs `dvbr epg --schedule --json` against
// each configured channel and ingests the events into store.
type Refresher struct {
	DvbrBin      string
	ChannelsFile string
	Adapter      int
	Channels     *config.Channels
	ChannelNames []string // names/aliases to refresh (as appears in channels.json)
	Store        *store.Store
	Interval     time.Duration // 0 = default 6h

	// Tuners arbitrates adapter access. When nil, refreshes spawn dvbr
	// directly and rely on its flock alone — the old behaviour, kept for
	// tests and headless one-shot use.
	Tuners Reserver

	// StartupDelay defers the first pass after Run starts (0 = default,
	// negative = no delay). RetryAfterPreempt overrides the post-eviction
	// retry delay.
	StartupDelay      time.Duration
	RetryAfterPreempt time.Duration
}

// RefreshOnce runs one ingest pass over ChannelNames. Returns the
// number of events upserted across all channels. A preemption aborts
// the remaining channels and reports ErrPreempted along with the count
// ingested so far.
func (r *Refresher) RefreshOnce(ctx context.Context) (int, error) {
	if r.Store == nil {
		return 0, fmt.Errorf("epg: Store is nil")
	}
	if r.DvbrBin == "" || r.ChannelsFile == "" {
		return 0, fmt.Errorf("epg: DvbrBin / ChannelsFile required")
	}
	total := 0
	// One tune per *mux*. `dvbr epg` harvests every service in the tuned
	// transport stream, so a second channel on a frequency already visited
	// this pass would re-collect the same EIT — a wasted minute of tuner
	// time that live viewing has to preempt.
	done := map[string]string{} // frequency → the channel that covered it
	for _, name := range r.ChannelNames {
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}

		if freq := r.frequencyOf(name); freq != "" {
			if first, seen := done[freq]; seen {
				slog.Debug("epg: mux already covered this pass",
					"channel", name, "covered_by", first, "frequency", freq)
				continue
			}
			done[freq] = name
		}

		n, err := r.refreshOne(ctx, name)
		total += n
		if errors.Is(err, ErrPreempted) {
			// The adapter is wanted elsewhere; stop the whole pass
			// rather than immediately re-reserving for the next channel.
			slog.Info("epg pass preempted, yielding adapter",
				"channel", name, "events_so_far", total)
			return total, err
		}
		if err != nil {
			slog.Warn("epg refresh failed", "channel", name, "err", err)
			continue
		}
		slog.Info("epg refreshed", "channel", name, "events", n)
	}
	return total, nil
}

// frequencyOf is the mux key for channelName, or "" when it can't be
// resolved — an unresolvable name is still attempted, so it fails loudly in
// refreshOne rather than being silently skipped here.
func (r *Refresher) frequencyOf(channelName string) string {
	if r.Channels == nil {
		return ""
	}
	if ch := r.Channels.Find(channelName); ch != nil {
		return ch.Frequency()
	}
	return ""
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

	adapter := r.Adapter
	var res *tuner.Reservation
	if r.Tuners != nil {
		var err error
		// Name the channel's own delivery system: this pass is about to
		// spawn `dvbr epg` against whatever adapter comes back, and dvbr
		// tunes it from channels.json without asking the Pool again.
		res, err = r.Tuners.Reserve(ctx, tuner.PrioBackground, ch.DeliverySystem())
		if err != nil {
			// Busy is normal and not worth escalating: live viewing and
			// recordings outrank EPG by design.
			if errors.Is(err, tuner.ErrNoAdapter) {
				return 0, fmt.Errorf("%w (adapter busy)", ErrPreempted)
			}
			return 0, fmt.Errorf("reserve adapter: %w", err)
		}
		defer res.Release() // no-op if already released below
		adapter = res.Adapter
	}

	args := []string{
		"epg",
		"--adapter", strconv.Itoa(adapter),
		"--channels", r.ChannelsFile,
		"--schedule",
		"--json",
		channelName,
	}
	cmdCtx, cancel := context.WithTimeout(ctx, collectTimeout)
	defer cancel()

	// Kill the child the moment something outranks us. The reservation
	// is released only after cmd.Run returns (i.e. after the process is
	// reaped), which is what guarantees its flock is gone before the
	// preemptor tunes.
	var preempted atomic.Bool
	if res != nil {
		watchDone := make(chan struct{})
		defer close(watchDone)
		go func() {
			select {
			case <-res.Preempted():
				preempted.Store(true)
				cancel()
			case <-watchDone:
			}
		}()
	}

	cmd := exec.CommandContext(cmdCtx, r.DvbrBin, args...)
	cmd.WaitDelay = childWaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	// Hand the adapter back before parsing/upserting — those don't need
	// hardware and can take a moment on a large schedule dump.
	if res != nil {
		res.Release()
	}

	if preempted.Load() {
		return 0, ErrPreempted
	}
	if runErr != nil {
		return 0, fmt.Errorf("dvbr epg: %w (stderr: %s)", runErr, stderr.String())
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

// Run drives RefreshOnce on r.Interval (default 6h). The first pass is
// deferred by StartupDelay so it doesn't fight the first live request;
// a preempted pass is retried after RetryAfterPreempt instead of
// waiting out the full interval. Blocks until ctx is canceled. Safe to
// call once; calling concurrently is not supported.
func (r *Refresher) Run(ctx context.Context) error {
	interval := r.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	retry := r.RetryAfterPreempt
	if retry <= 0 {
		retry = defaultRetryAfterPreempt
	}
	delay := r.StartupDelay
	if delay == 0 {
		delay = defaultStartupDelay
	}
	if delay > 0 {
		slog.Info("epg: deferring first pass", "delay", delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}

	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		_, err := r.RefreshOnce(ctx)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, ErrPreempted):
			slog.Info("epg: retrying preempted pass later", "in", retry)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retry):
			}
			continue
		case err != nil:
			slog.Warn("epg refresh pass failed", "err", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
