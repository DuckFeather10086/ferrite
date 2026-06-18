package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DuckFeather10086/isdb-hub/internal/recorder"
	"github.com/DuckFeather10086/isdb-hub/internal/store"
)

type fakeRunner struct {
	mu       sync.Mutex
	jobs     []recorder.Job
	runCalls atomic.Int32
	fail     bool
}

func (f *fakeRunner) Run(_ context.Context, j recorder.Job) error {
	f.runCalls.Add(1)
	f.mu.Lock()
	f.jobs = append(f.jobs, j)
	f.mu.Unlock()
	if f.fail {
		return errors.New("simulated failure")
	}
	return nil
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestScheduler_DispatchesDueAndMarksDone(t *testing.T) {
	st := openStore(t)
	fr := &fakeRunner{}
	s := &Scheduler{Store: st, Runner: fr, Tick: 50 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Schedule a recording due "now".
	start := time.Now().Add(-time.Minute)
	end := start.Add(time.Hour)
	id, err := st.CreateSchedule(ctx, store.Schedule{
		Channel:   "mx",
		ServiceID: 23608,
		Start:     start,
		End:       end,
		Lead:      0,
		Trail:     0,
	})
	if err != nil {
		t.Fatal(err)
	}

	go s.Run(ctx)
	// Wait for dispatch + finalize.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fr.runCalls.Load() > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if fr.runCalls.Load() != 1 {
		t.Fatalf("expected 1 dispatch, got %d", fr.runCalls.Load())
	}

	// Final state should land shortly.
	deadline = time.Now().Add(time.Second)
	var sch []store.Schedule
	for time.Now().Before(deadline) {
		sch, _ = st.ListSchedules(ctx)
		if sch[0].State == store.ScheduleStateDone {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if sch[0].State != store.ScheduleStateDone {
		t.Fatalf("schedule %d state = %s, want done", id, sch[0].State)
	}
}

func TestScheduler_FailedRunMarksFailed(t *testing.T) {
	st := openStore(t)
	fr := &fakeRunner{fail: true}
	s := &Scheduler{Store: st, Runner: fr, Tick: 50 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := time.Now().Add(-time.Minute)
	_, _ = st.CreateSchedule(ctx, store.Schedule{
		Channel: "mx", Start: start, End: start.Add(time.Minute),
	})

	go s.Run(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sch, _ := st.ListSchedules(ctx)
		if len(sch) > 0 && sch[0].State == store.ScheduleStateFailed {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("did not see failed state")
}

func TestScheduler_DoesNotDispatchFutureSchedules(t *testing.T) {
	st := openStore(t)
	fr := &fakeRunner{}
	s := &Scheduler{Store: st, Runner: fr, Tick: 50 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Way out in the future + zero lead.
	start := time.Now().Add(time.Hour)
	_, _ = st.CreateSchedule(ctx, store.Schedule{
		Channel: "mx", Start: start, End: start.Add(time.Hour),
	})

	go s.Run(ctx)
	time.Sleep(300 * time.Millisecond)
	if fr.runCalls.Load() != 0 {
		t.Fatalf("future schedule should not dispatch, got %d calls", fr.runCalls.Load())
	}
}

func TestScheduler_NoDoubleDispatch(t *testing.T) {
	st := openStore(t)
	// Runner that blocks until released so the schedule stays "running"
	// across multiple ticks.
	rel := make(chan struct{})
	fr := &blockingRunner{release: rel}
	s := &Scheduler{Store: st, Runner: fr, Tick: 30 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := time.Now().Add(-time.Minute)
	_, _ = st.CreateSchedule(ctx, store.Schedule{
		Channel: "mx", Start: start, End: start.Add(time.Hour),
	})

	go s.Run(ctx)
	// Let several ticks elapse with the runner blocked.
	time.Sleep(250 * time.Millisecond)
	if got := fr.calls.Load(); got != 1 {
		t.Fatalf("ticks dispatched schedule %d times; want 1", got)
	}
	close(rel)
}

type blockingRunner struct {
	calls   atomic.Int32
	release <-chan struct{}
}

func (b *blockingRunner) Run(_ context.Context, _ recorder.Job) error {
	b.calls.Add(1)
	<-b.release
	return nil
}
