// Package recorder runs a single recording job: waits until lead-in
// time, acquires a tuner Lease, drains fanout chunks into a file,
// and updates the store row through the recording lifecycle.
//
// Failure modes that must surface as state='failed' on the row
// (with a non-empty error message):
//   - tuner.Pool.Acquire failure
//   - startup watchdog: no chunks within StartupTimeout
//   - stall watchdog: chunks stop flowing mid-stream
//   - disk write error
//
// The watchdog discipline is ported from legacy live_hls.py: an
// "appears to be running" recording that produces no bytes is the
// failure mode that lost us shows in the past — fail loudly to the
// row so the UI can show it.
package recorder

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/DuckFeather10086/isdbd/internal/store"
	"github.com/DuckFeather10086/isdbd/internal/tuner"
)

// Default watchdog timings.
const (
	DefaultStartupTimeout = 15 * time.Second
	DefaultStallTimeout   = 10 * time.Second
	watchdogTick          = 500 * time.Millisecond
)

// Acquirer is the seam between Runner and a real tuner.Pool. Tests
// pass a hand-rolled implementation backed by a fake tuner.
type Acquirer interface {
	Acquire(ctx context.Context, channel string) (*tuner.Lease, error)
}

// Runner executes recording jobs.
type Runner struct {
	Tuners         Acquirer
	Store          *store.Store
	StorageRoot    string        // recordings written under {StorageRoot}/recordings/...
	StartupTimeout time.Duration // 0 → DefaultStartupTimeout
	StallTimeout   time.Duration // 0 → DefaultStallTimeout
}

// Job describes one recording task.
type Job struct {
	ScheduleID int64
	Channel    string
	Title      string
	Start      time.Time
	End        time.Time
	Lead       time.Duration
	Trail      time.Duration
}

// Run executes j synchronously. The recording row is created up
// front and finalized regardless of outcome.
func (r *Runner) Run(ctx context.Context, j Job) error {
	if r.Store == nil || r.Tuners == nil || r.StorageRoot == "" {
		return errors.New("recorder: Store/Tuners/StorageRoot required")
	}
	startup := r.StartupTimeout
	if startup == 0 {
		startup = DefaultStartupTimeout
	}
	stall := r.StallTimeout
	if stall == 0 {
		stall = DefaultStallTimeout
	}

	path := r.namePath(j)
	recID, err := r.Store.CreateRecording(ctx, store.Recording{
		ScheduleID: nullableInt64(j.ScheduleID),
		Channel:    j.Channel,
		Title:      j.Title,
		Start:      time.Now().UTC(),
		Path:       path,
	})
	if err != nil {
		return fmt.Errorf("recorder: create row: %w", err)
	}

	// Finalize the row no matter how we return.
	finalState := store.RecordingStateFailed
	finalErr := ""
	endActual := time.Time{}
	var bytesWritten int64
	defer func() {
		if endActual.IsZero() {
			endActual = time.Now().UTC()
		}
		if err := r.Store.FinalizeRecording(context.Background(),
			recID, endActual, bytesWritten, finalState, finalErr); err != nil {
			slog.Warn("recorder: finalize row", "id", recID, "err", err)
		}
	}()

	// Wait until lead-in time.
	leadStart := j.Start.Add(-j.Lead)
	if d := time.Until(leadStart); d > 0 {
		select {
		case <-ctx.Done():
			finalErr = "canceled before lead-in"
			return ctx.Err()
		case <-time.After(d):
		}
	}

	lease, err := r.Tuners.Acquire(ctx, j.Channel)
	if err != nil {
		finalErr = fmt.Sprintf("acquire: %v", err)
		return err
	}
	defer lease.Release()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		finalErr = fmt.Sprintf("mkdir: %v", err)
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		finalErr = fmt.Sprintf("create file: %v", err)
		return err
	}
	defer f.Close()

	slog.Info("recorder: started",
		"id", recID, "channel", j.Channel, "path", path,
		"end", j.End.Add(j.Trail).Format(time.RFC3339))

	// Bound the read loop by end+trail.
	deadlineCtx, cancel := context.WithDeadline(ctx, j.End.Add(j.Trail))
	defer cancel()

	ticker := time.NewTicker(watchdogTick)
	defer ticker.Stop()

	startupDeadline := time.Now().Add(startup)
	lastChunk := time.Now()
	gotFirst := false

	for {
		select {
		case <-deadlineCtx.Done():
			// Either ctx canceled or normal completion at end+trail.
			if errors.Is(deadlineCtx.Err(), context.DeadlineExceeded) {
				if !gotFirst {
					finalErr = "no chunks received before deadline"
					return errors.New(finalErr)
				}
				finalState = store.RecordingStateDone
				endActual = time.Now().UTC()
				return nil
			}
			finalErr = "canceled"
			return deadlineCtx.Err()

		case chunk, ok := <-lease.Sub.Ch:
			if !ok {
				// Source closed (dvbr died or unsubscribed).
				if !gotFirst {
					finalErr = "source closed before any chunk"
					return errors.New(finalErr)
				}
				// Past first chunk — count what we got as done.
				finalState = store.RecordingStateDone
				endActual = time.Now().UTC()
				return nil
			}
			if _, err := f.Write(chunk.Data); err != nil {
				chunk.Release()
				finalErr = fmt.Sprintf("write: %v", err)
				return err
			}
			bytesWritten += int64(len(chunk.Data))
			gotFirst = true
			lastChunk = time.Now()
			chunk.Release()

		case <-ticker.C:
			if !gotFirst && time.Now().After(startupDeadline) {
				finalErr = fmt.Sprintf(
					"startup watchdog: no chunks within %s", startup)
				return errors.New(finalErr)
			}
			if gotFirst && time.Since(lastChunk) > stall {
				finalErr = fmt.Sprintf(
					"stall watchdog: no chunks for %s", stall)
				return errors.New(finalErr)
			}
		}
	}
}

// namePath constructs the on-disk path for j.
// Layout: {StorageRoot}/recordings/{YYYY-MM-DD}/{channel}_{HHMM}_{slug}.ts
func (r *Runner) namePath(j Job) string {
	day := j.Start.UTC().Format("2006-01-02")
	hm := j.Start.UTC().Format("1504")
	slug := slugify(j.Title)
	if slug == "" {
		slug = "untitled"
	}
	name := fmt.Sprintf("%s_%s_%s.ts", sanitize(j.Channel), hm, slug)
	return filepath.Join(r.StorageRoot, "recordings", day, name)
}

var slugSubst = regexp.MustCompile(`[^\p{L}\p{N}_\-]+`)

func slugify(s string) string {
	s = strings.TrimSpace(s)
	s = slugSubst.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

func sanitize(s string) string {
	return slugSubst.ReplaceAllString(s, "_")
}

func nullableInt64(v int64) sql.NullInt64 {
	if v == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: v, Valid: true}
}
