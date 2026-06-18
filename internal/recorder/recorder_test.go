package recorder

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DuckFeather10086/ferrite/internal/config"
	"github.com/DuckFeather10086/ferrite/internal/store"
	"github.com/DuckFeather10086/ferrite/internal/tuner"
)

// ── test harness ───────────────────────────────────────────────────

func setup(t *testing.T, ft *fakeTuner) (*Runner, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	channels := &config.Channels{
		Version: 1,
		Channels: []config.Channel{
			{Name: "mx", Tuning: map[string]string{"SERVICE_ID": "23608"}},
		},
	}
	pool := tuner.NewPool(ft, channels, []int{0}, 4)

	r := &Runner{
		Tuners:         pool,
		Store:          st,
		StorageRoot:    t.TempDir(),
		StartupTimeout: 500 * time.Millisecond, // tight for tests
		StallTimeout:   500 * time.Millisecond,
	}
	return r, st
}

type fakeTuner struct {
	makeStream func(ctx context.Context, adapter int, channel string) tuner.TsStream
	err        error
}

func (f *fakeTuner) Tune(ctx context.Context, adapter int, channel string) (tuner.TsStream, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.makeStream(ctx, adapter, channel), nil
}

// scriptedStream emits a fixed list of byte slices then either EOFs
// or holds forever (blocking on ctx cancel).
type scriptedStream struct {
	ctx        context.Context
	chunks     [][]byte
	off        int
	holdAtEnd  bool
	done       chan struct{}
	once       sync.Once
}

func newScripted(ctx context.Context, chunks [][]byte, holdAtEnd bool) *scriptedStream {
	return &scriptedStream{ctx: ctx, chunks: chunks, holdAtEnd: holdAtEnd, done: make(chan struct{})}
}

func (s *scriptedStream) Read(p []byte) (int, error) {
	if s.off < len(s.chunks) {
		n := copy(p, s.chunks[s.off])
		s.off++
		return n, nil
	}
	if !s.holdAtEnd {
		return 0, io.EOF
	}
	select {
	case <-s.ctx.Done():
	case <-s.done:
	}
	return 0, io.EOF
}

func (s *scriptedStream) Close() error {
	s.once.Do(func() { close(s.done) })
	return nil
}

// ── tests ──────────────────────────────────────────────────────────

func TestRunner_HappyPath(t *testing.T) {
	payload := bytes.Repeat([]byte{0x47}, 4096)
	ft := &fakeTuner{makeStream: func(ctx context.Context, _ int, _ string) tuner.TsStream {
		return newScripted(ctx, [][]byte{payload, payload}, true)
	}}
	r, st := setup(t, ft)

	now := time.Now()
	j := Job{
		Channel: "mx",
		Title:   "Test Show",
		Start:   now,
		End:     now.Add(200 * time.Millisecond),
	}
	err := r.Run(context.Background(), j)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	recs, err := st.ListRecordings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d recordings", len(recs))
	}
	if recs[0].State != store.RecordingStateDone {
		t.Fatalf("state %s, want done", recs[0].State)
	}
	if !recs[0].SizeBytes.Valid || recs[0].SizeBytes.Int64 < int64(len(payload)) {
		t.Fatalf("size: %v", recs[0].SizeBytes)
	}

	info, err := os.Stat(recs[0].Path)
	if err != nil {
		t.Fatalf("file not present: %v", err)
	}
	if info.Size() < int64(len(payload)) {
		t.Fatalf("file too small: %d", info.Size())
	}
	if !strings.Contains(recs[0].Path, "Test_Show") {
		t.Fatalf("path slug wrong: %s", recs[0].Path)
	}
}

func TestRunner_StartupWatchdog_FailsRow(t *testing.T) {
	// Tuner returns a stream that never produces bytes.
	ft := &fakeTuner{makeStream: func(ctx context.Context, _ int, _ string) tuner.TsStream {
		return newScripted(ctx, nil, true)
	}}
	r, st := setup(t, ft)

	now := time.Now()
	err := r.Run(context.Background(), Job{
		Channel: "mx",
		Start:   now,
		End:     now.Add(5 * time.Second),
	})
	if err == nil || !strings.Contains(err.Error(), "startup watchdog") {
		t.Fatalf("expected startup watchdog failure, got %v", err)
	}

	recs, _ := st.ListRecordings(context.Background())
	if recs[0].State != store.RecordingStateFailed {
		t.Fatalf("row state %s, want failed", recs[0].State)
	}
	if !strings.Contains(recs[0].Error, "startup watchdog") {
		t.Fatalf("row error: %s", recs[0].Error)
	}
}

func TestRunner_StallWatchdog_FailsRow(t *testing.T) {
	// One chunk then hold forever.
	ft := &fakeTuner{makeStream: func(ctx context.Context, _ int, _ string) tuner.TsStream {
		return newScripted(ctx, [][]byte{bytes.Repeat([]byte{1}, 64)}, true)
	}}
	r, st := setup(t, ft)

	now := time.Now()
	err := r.Run(context.Background(), Job{
		Channel: "mx",
		Start:   now,
		End:     now.Add(10 * time.Second),
	})
	if err == nil || !strings.Contains(err.Error(), "stall watchdog") {
		t.Fatalf("expected stall failure, got %v", err)
	}
	recs, _ := st.ListRecordings(context.Background())
	if recs[0].State != store.RecordingStateFailed {
		t.Fatalf("row state %s, want failed", recs[0].State)
	}
}

func TestRunner_AcquireFails(t *testing.T) {
	ft := &fakeTuner{err: errors.New("tuner offline")}
	r, st := setup(t, ft)

	now := time.Now()
	err := r.Run(context.Background(), Job{
		Channel: "mx",
		Start:   now,
		End:     now.Add(time.Second),
	})
	if err == nil {
		t.Fatal("expected acquire failure")
	}
	recs, _ := st.ListRecordings(context.Background())
	if recs[0].State != store.RecordingStateFailed {
		t.Fatalf("row state %s, want failed", recs[0].State)
	}
	if !strings.Contains(recs[0].Error, "acquire") {
		t.Fatalf("row error: %s", recs[0].Error)
	}
}

func TestRunner_ContextCancelMidRecording(t *testing.T) {
	// Source emits constantly until ctx cancel.
	emit := atomic.Int32{}
	ft := &fakeTuner{makeStream: func(ctx context.Context, _ int, _ string) tuner.TsStream {
		return &steadyStream{ctx: ctx, counter: &emit}
	}}
	r, st := setup(t, ft)

	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now()
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	err := r.Run(ctx, Job{
		Channel: "mx",
		Start:   now,
		End:     now.Add(10 * time.Second),
	})
	if err == nil {
		t.Fatal("expected ctx cancel error")
	}
	recs, _ := st.ListRecordings(context.Background())
	// State is failed because we canceled mid-recording (the finalize
	// reflects what actually happened — not "done").
	if recs[0].State != store.RecordingStateFailed {
		t.Fatalf("row state %s", recs[0].State)
	}
}

func TestRunner_RespectsLeadIn(t *testing.T) {
	// Start time is 200ms in the future; recorder must wait.
	ft := &fakeTuner{makeStream: func(ctx context.Context, _ int, _ string) tuner.TsStream {
		return newScripted(ctx, [][]byte{{1, 2, 3, 4}}, true)
	}}
	r, st := setup(t, ft)

	begin := time.Now()
	start := begin.Add(200 * time.Millisecond)
	err := r.Run(context.Background(), Job{
		Channel: "mx",
		Start:   start,
		End:     start.Add(150 * time.Millisecond),
		Lead:    0,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := time.Since(begin); elapsed < 200*time.Millisecond {
		t.Fatalf("recorder did not wait for start; elapsed=%v", elapsed)
	}

	recs, _ := st.ListRecordings(context.Background())
	if recs[0].State != store.RecordingStateDone {
		t.Fatalf("state: %s", recs[0].State)
	}
}

// steadyStream emits a constant trickle of small chunks until ctx
// cancels. Used by the cancel-mid-recording test.
type steadyStream struct {
	ctx     context.Context
	counter *atomic.Int32
}

func (s *steadyStream) Read(p []byte) (int, error) {
	select {
	case <-s.ctx.Done():
		return 0, io.EOF
	default:
	}
	s.counter.Add(1)
	for i := 0; i < 64 && i < len(p); i++ {
		p[i] = 0x47
	}
	if len(p) < 64 {
		return len(p), nil
	}
	return 64, nil
}
func (s *steadyStream) Close() error { return nil }
