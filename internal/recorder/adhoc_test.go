package recorder

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DuckFeather10086/ferrite/internal/config"
	"github.com/DuckFeather10086/ferrite/internal/store"
	"github.com/DuckFeather10086/ferrite/internal/tuner"
)

func steadyTuner() *fakeTuner {
	payload := bytes.Repeat([]byte{0x47}, 4096)
	return &fakeTuner{makeStream: func(ctx context.Context, _ int, _ string) tuner.TsStream {
		return newScripted(ctx, [][]byte{payload, payload}, true)
	}}
}

// newManager wraps setup() and guarantees no ad-hoc job outlives the
// test — a leaked one would try to finalize against a closed store.
func newManager(t *testing.T, ft *fakeTuner) (*Manager, *store.Store) {
	t.Helper()
	r, st := setup(t, ft)
	m := &Manager{Runner: r}
	t.Cleanup(m.StopAll)
	return m, st
}

// waitForState polls until the row reaches a terminal state.
func waitForState(t *testing.T, st *store.Store, id int64, timeout time.Duration) store.Recording {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		recs, err := st.ListRecordings(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, rec := range recs {
			if rec.ID == id && rec.State != store.RecordingStateRecording {
				return rec
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("recording %d never reached a terminal state", id)
	return store.Recording{}
}

// The "record now" button: start open-ended, stop by id, and the row
// must end up 'done' with real bytes — not 'failed'.
func TestManager_StartThenStop(t *testing.T) {
	m, st := newManager(t, steadyTuner())

	id, err := m.Start(context.Background(), "mx", "Ad Hoc", 0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if id == 0 {
		t.Fatal("Start returned id 0")
	}
	if got := m.Active(); len(got) != 1 || got[0] != id {
		t.Fatalf("Active() = %v, want [%d]", got, id)
	}

	// Let a chunk or two land so the row has content to finalize.
	time.Sleep(100 * time.Millisecond)
	if err := m.Stop(id); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	rec := waitForState(t, st, id, 3*time.Second)
	if rec.State != store.RecordingStateDone {
		t.Fatalf("state = %s (err %q), want done", rec.State, rec.Error)
	}
	if !rec.SizeBytes.Valid || rec.SizeBytes.Int64 == 0 {
		t.Fatalf("size = %v, want non-zero", rec.SizeBytes)
	}
	if _, err := os.Stat(rec.Path); err != nil {
		t.Fatalf("file missing: %v", err)
	}
	if !strings.Contains(rec.Path, "Ad_Hoc") {
		t.Fatalf("path %q lost the title slug", rec.Path)
	}
	if got := m.Active(); len(got) != 0 {
		t.Fatalf("Active() = %v after stop, want empty", got)
	}
}

func TestManager_StopUnknownID(t *testing.T) {
	m, _ := newManager(t, steadyTuner())
	if err := m.Stop(4242); !errors.Is(err, ErrNotRecording) {
		t.Fatalf("err = %v, want ErrNotRecording", err)
	}
}

// Stopping twice is not an error the caller has to reason about, and
// must not panic on the closed channel.
func TestManager_DoubleStop(t *testing.T) {
	m, st := newManager(t, steadyTuner())

	id, err := m.Start(context.Background(), "mx", "", 0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := m.Stop(id); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	// The second Stop races the deregister; either outcome is fine as
	// long as it neither panics nor reports something other than
	// "not recording".
	if err := m.Stop(id); err != nil && !errors.Is(err, ErrNotRecording) {
		t.Fatalf("second Stop: %v", err)
	}
	if rec := waitForState(t, st, id, 3*time.Second); rec.State != store.RecordingStateDone {
		t.Fatalf("state = %s, want done", rec.State)
	}
}

// StopAll is the shutdown path: in-flight recordings finalize as 'done'
// instead of being killed mid-write.
func TestManager_StopAllFinalizesDone(t *testing.T) {
	m, st := newManager(t, steadyTuner())

	id, err := m.Start(context.Background(), "mx", "Shutdown", 0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	m.StopAll()

	if rec := waitForState(t, st, id, 3*time.Second); rec.State != store.RecordingStateDone {
		t.Fatalf("state = %s (err %q), want done", rec.State, rec.Error)
	}
}

// StopAllAndWait must not return until the rows are final — the daemon
// closes the store right after it, and a row written afterwards is lost
// (leaving it stuck in state 'recording').
func TestManager_StopAllAndWaitFinalizesBeforeReturning(t *testing.T) {
	m, st := newManager(t, steadyTuner())

	id, err := m.Start(context.Background(), "mx", "Shutdown", 0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	if stuck := m.StopAllAndWait(5 * time.Second); stuck != 0 {
		t.Fatalf("StopAllAndWait reported %d stuck jobs", stuck)
	}
	// No polling: the row must already be terminal.
	recs, err := st.ListRecordings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].ID != id {
		t.Fatalf("recordings = %+v", recs)
	}
	if recs[0].State != store.RecordingStateDone {
		t.Fatalf("state = %s (err %q), want done immediately after the wait",
			recs[0].State, recs[0].Error)
	}
	if m.Active() != nil && len(m.Active()) != 0 {
		t.Fatalf("Active() = %v, want empty", m.Active())
	}
}

// A recording asked to stop while its context is also being canceled
// (SIGTERM does both at once) keeps what it wrote.
func TestRunner_StopRacingContextCancelKeepsBytes(t *testing.T) {
	m, st := newManager(t, steadyTuner())
	ctx, cancel := context.WithCancel(context.Background())
	m.Base = ctx

	id, err := m.Start(context.Background(), "mx", "Racy", 0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// Fire both at the same instant, the way daemon shutdown does.
	go cancel()
	m.StopAllAndWait(5 * time.Second)

	recs, err := st.ListRecordings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].ID != id {
		t.Fatalf("recordings = %+v", recs)
	}
	if recs[0].State != store.RecordingStateDone {
		t.Fatalf("state = %s (err %q), want done — the stop should win the race",
			recs[0].State, recs[0].Error)
	}
	if !recs[0].SizeBytes.Valid || recs[0].SizeBytes.Int64 == 0 {
		t.Fatalf("size = %v, want the bytes written", recs[0].SizeBytes)
	}
}

// A fixed duration ends the recording on its own with no Stop call.
func TestManager_FixedDurationEndsItself(t *testing.T) {
	m, st := newManager(t, steadyTuner())

	id, err := m.Start(context.Background(), "mx", "Short", 300*time.Millisecond)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if rec := waitForState(t, st, id, 3*time.Second); rec.State != store.RecordingStateDone {
		t.Fatalf("state = %s (err %q), want done", rec.State, rec.Error)
	}
}

// A busy tuner still yields a row the caller can display, carrying the
// failure — Start must not swallow it or hang.
func TestManager_StartOnFailingTunerStillReturnsRow(t *testing.T) {
	m, st := newManager(t, &fakeTuner{err: errors.New("tuner offline")})

	id, err := m.Start(context.Background(), "mx", "Doomed", 0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	rec := waitForState(t, st, id, 3*time.Second)
	if rec.State != store.RecordingStateFailed {
		t.Fatalf("state = %s, want failed", rec.State)
	}
	if !strings.Contains(rec.Error, "acquire") {
		t.Fatalf("row error = %q, want it to mention acquire", rec.Error)
	}
}

// Being evicted by a higher-priority claim keeps what was already
// written (state 'done') and says so in the row.
func TestRunner_PreemptedMidRecordingKeepsBytes(t *testing.T) {
	// setup()'s config only knows "mx"; preemption needs a second
	// channel so the competing claim can't just share the session.
	st, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	channels := &config.Channels{
		Version: 1,
		Channels: []config.Channel{
			{Name: "mx", Tuning: map[string]string{"SERVICE_ID": "23608"}},
			{Name: "nhk", Tuning: map[string]string{"SERVICE_ID": "1024"}},
		},
	}
	pool := tuner.NewPool(steadyTuner(), channels, config.ISDBTAdapters(0), 4)
	m := &Manager{Runner: &Runner{
		Tuners:         pool,
		Store:          st,
		StorageRoot:    t.TempDir(),
		StartupTimeout: 500 * time.Millisecond,
		StallTimeout:   500 * time.Millisecond,
	}}
	t.Cleanup(m.StopAll)

	id, err := m.Start(context.Background(), "mx", "Bumped", 0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(150 * time.Millisecond)

	// Nothing outranks a recording, so simulate the eviction with a
	// claim the pool will honour: a second recording on another channel.
	lease, err := pool.AcquireAt(context.Background(), "nhk", tuner.PrioRecord+1)
	if err != nil {
		t.Fatalf("preempting AcquireAt: %v", err)
	}
	defer lease.Release()

	rec := waitForState(t, st, id, 3*time.Second)
	if rec.State != store.RecordingStateDone {
		t.Fatalf("state = %s (err %q), want done with the partial file", rec.State, rec.Error)
	}
	if !strings.Contains(rec.Error, "preempt") {
		t.Fatalf("row error = %q, want it to name preemption", rec.Error)
	}
	if !rec.SizeBytes.Valid || rec.SizeBytes.Int64 == 0 {
		t.Fatalf("size = %v, want the bytes written before eviction", rec.SizeBytes)
	}
}
