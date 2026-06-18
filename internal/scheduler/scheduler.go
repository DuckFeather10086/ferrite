// Package scheduler ticks periodically, finds schedules whose
// (start - lead) <= now, and dispatches them to recorder.Runner.
//
// The "running" state is marked atomically in the store before the
// dispatch goroutine starts so a subsequent tick (or a process
// restart that re-runs the tick mid-job) does not double-dispatch.
// On completion the row transitions to done / failed based on the
// Runner's return.
package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/DuckFeather10086/isdb-hub/internal/recorder"
	"github.com/DuckFeather10086/isdb-hub/internal/store"
)

// DefaultTick is how often the scheduler scans the store.
const DefaultTick = 30 * time.Second

// Runner is the seam between Scheduler and recorder.Runner. Tests
// substitute a fake.
type Runner interface {
	Run(ctx context.Context, j recorder.Job) error
}

type Scheduler struct {
	Store  *store.Store
	Runner Runner
	Tick   time.Duration // 0 → DefaultTick
}

// Run drives the scheduler loop. Blocks until ctx is canceled.
// On exit, in-flight recordings continue (they hold their own
// ctx); the scheduler just stops dispatching new ones.
func (s *Scheduler) Run(ctx context.Context) error {
	tick := s.Tick
	if tick == 0 {
		tick = DefaultTick
	}
	t := time.NewTicker(tick)
	defer t.Stop()

	// One immediate tick so a startup catches things that were due
	// while the daemon was down.
	s.tickOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.tickOnce(ctx)
		}
	}
}

func (s *Scheduler) tickOnce(ctx context.Context) {
	due, err := s.Store.DueSchedules(ctx, time.Now())
	if err != nil {
		slog.Warn("scheduler: query due schedules", "err", err)
		return
	}
	for _, sch := range due {
		// Reserve the row by flipping to running BEFORE we dispatch.
		// If the UPDATE fails, skip — the next tick will retry.
		if err := s.Store.UpdateScheduleState(ctx, sch.ID, store.ScheduleStateRunning); err != nil {
			slog.Warn("scheduler: reserve schedule", "id", sch.ID, "err", err)
			continue
		}
		go s.runOne(ctx, sch)
	}
}

func (s *Scheduler) runOne(ctx context.Context, sch store.Schedule) {
	job := recorder.Job{
		ScheduleID: sch.ID,
		Channel:    sch.Channel,
		Start:      sch.Start,
		End:        sch.End,
		Lead:       sch.Lead,
		Trail:      sch.Trail,
	}
	err := s.Runner.Run(ctx, job)

	final := store.ScheduleStateDone
	if err != nil {
		final = store.ScheduleStateFailed
		slog.Warn("scheduler: recording failed",
			"schedule_id", sch.ID, "channel", sch.Channel, "err", err)
	} else {
		slog.Info("scheduler: recording done",
			"schedule_id", sch.ID, "channel", sch.Channel)
	}

	// Use background ctx so the final state lands even if the daemon
	// is shutting down — we want the row to reflect what happened.
	if err := s.Store.UpdateScheduleState(context.Background(), sch.ID, final); err != nil {
		slog.Warn("scheduler: finalize schedule state",
			"id", sch.ID, "err", err)
	}
}

// noopRunner is here just to keep the package compileable for callers
// that haven't yet built a recorder.Runner. Not used in production.
var _ Runner = noopRunner{}

type noopRunner struct{ mu sync.Mutex }

func (noopRunner) Run(context.Context, recorder.Job) error { return nil }
