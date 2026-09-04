package hls

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DuckFeather10086/ferrite/internal/tuner"
)

// writeSlowFFmpeg is writeFakeFFmpeg that never produces a playlist, so a
// session opened with it stays in the state a real cold tune is in for its
// first few seconds: holding the adapter, with nothing on disk yet.
func writeSlowFFmpeg(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffmpeg")
	script := "#!/bin/sh\ncat > /dev/null\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Closing a session has to wake whoever was waiting on its playlist. Without
// this the API sits out its whole 45s timeout for a file whose ffmpeg is
// already dead, and answers "stream did not start" for a channel the viewer
// left long ago.
func TestSessionDoneFiresOnClose(t *testing.T) {
	ft := &fakeTuner{makeStream: func(ctx context.Context) tuner.TsStream {
		return &holdStream{ctx: ctx, done: make(chan struct{})}
	}}
	m := &Manager{
		Tuners:     newPool(t, ft),
		OutputRoot: t.TempDir(),
		FFmpegBin:  writeSlowFFmpeg(t),
	}
	s, err := m.Open(context.Background(), "mx", "")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
		t.Fatal("a live session reports itself done")
	default:
	}

	m.Close("mx")

	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done did not fire when the session was closed")
	}
}

// The failure this whole path exists for: an open that has taken the adapter
// but not yet reached ffmpeg is invisible to m.sessions, so a channel change
// arriving in that window used to find nothing to close and get ErrNoAdapter.
// One adapter, so if the second Open succeeds the first must have let go.
func TestCloseOthersCancelsAnOpenStillInFlight(t *testing.T) {
	ft := &fakeTuner{makeStream: func(ctx context.Context) tuner.TsStream {
		return &holdStream{ctx: ctx, done: make(chan struct{})}
	}}
	pool := newPool(t, ft)
	m := &Manager{
		Tuners:     pool,
		OutputRoot: t.TempDir(),
		FFmpegBin:  writeSlowFFmpeg(t),
		// A probe that never returns on its own is what a cold channel's
		// five-second ffprobe looks like from here: the open is holding the
		// adapter and has not registered a session.
		FFprobeBin:   writeHangingFFprobe(t),
		ProbeSeconds: 30,
	}

	type result struct {
		s   *Session
		err error
	}
	first := make(chan result, 1)
	go func() {
		s, err := m.Open(context.Background(), "mx", "")
		first <- result{s, err}
	}()

	// Wait until the first open is actually holding the adapter, which is the
	// state that used to be unrecoverable.
	waitFor(t, "the first open to take the adapter", func() bool {
		for _, st := range pool.Status() {
			if st.Channel == "mx" {
				return true
			}
		}
		return false
	})

	closed := m.CloseOthers("nhk")
	if len(closed) != 1 || closed[0] != "mx" {
		t.Fatalf("CloseOthers should have reported cancelling mx, got %v", closed)
	}

	select {
	case r := <-first:
		if !errors.Is(r.err, ErrSuperseded) {
			t.Fatalf("the cancelled open should report ErrSuperseded, got %v", r.err)
		}
		if r.s != nil {
			t.Fatal("a cancelled open must not hand back a session")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the cancelled open never returned")
	}

	// And the adapter is genuinely free: CloseOthers waits for the cancelled
	// open to let go, so the channel the viewer actually pressed can have it.
	if st := pool.Status(); st[0].Channel != "" {
		t.Fatalf("adapter still held by %q after the open was cancelled", st[0].Channel)
	}
}

// writeHangingFFprobe stands in for the A/V offset measurement on a channel
// with no cached one: it reads its input and never exits, so the open sits in
// probeAudioOffset until its context is cancelled.
func writeHangingFFprobe(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffprobe")
	script := "#!/bin/sh\ncat > /dev/null\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
