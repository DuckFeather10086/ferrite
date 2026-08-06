package hls

import (
	"bytes"
	"context"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DuckFeather10086/ferrite/internal/config"
	"github.com/DuckFeather10086/ferrite/internal/tuner"
)

// fakeTuner returns a stream of constant chunks until ctx cancels.
type fakeTuner struct {
	makeStream func(ctx context.Context) tuner.TsStream
	tunes      atomic.Int64
}

func (f *fakeTuner) Tune(ctx context.Context, _ int, _ string) (tuner.TsStream, error) {
	f.tunes.Add(1)
	return f.makeStream(ctx), nil
}

// tuneCount is how many times the frontend was actually tuned — the
// number that says whether tiers are sharing a tune or fighting for one.
func (f *fakeTuner) tuneCount() int64 { return f.tunes.Load() }

type holdStream struct {
	ctx   context.Context
	off   int
	body  []byte
	done  chan struct{}
	once  sync.Once
	tally *atomic.Int64
}

func (h *holdStream) Read(p []byte) (int, error) {
	// Emit body once, then loop emitting small constant chunks until
	// ctx cancels or Close is called. We want the HLS pump to have
	// something to forward to the fake ffmpeg.
	if h.off < len(h.body) {
		n := copy(p, h.body[h.off:])
		h.off += n
		if h.tally != nil {
			h.tally.Add(int64(n))
		}
		return n, nil
	}
	select {
	case <-h.ctx.Done():
	case <-h.done:
	default:
		// Trickle.
		n := len(p)
		if n > 32 {
			n = 32
		}
		for i := 0; i < n; i++ {
			p[i] = 0x47
		}
		if h.tally != nil {
			h.tally.Add(int64(n))
		}
		return n, nil
	}
	return 0, io.EOF
}
func (h *holdStream) Close() error {
	h.once.Do(func() { close(h.done) })
	return nil
}

func newPool(t *testing.T, ft *fakeTuner) *tuner.Pool {
	t.Helper()
	channels := &config.Channels{
		Version: 1,
		Channels: []config.Channel{
			{Name: "mx", Tuning: map[string]string{"SERVICE_ID": "23608"}},
			{Name: "nhk", Tuning: map[string]string{"SERVICE_ID": "1024"}},
		},
	}
	return tuner.NewPool(ft, channels, config.ISDBTAdapters(0), 4)
}

// writeFakeFFmpeg drops a shell-script fake-ffmpeg into dir and
// returns its path. The fake drains stdin, writes a stream.m3u8 to
// the path that's the last argv, and touches one segment.
func writeFakeFFmpeg(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ffmpeg")
	script := `#!/bin/sh
# Iterate to get the last arg (the m3u8 path the HLS Manager passes last).
for last; do :; done
outdir="$(dirname "$last")"
mkdir -p "$outdir"
# Write the playlist and a segment up front — real ffmpeg only emits
# these once it sees enough input, but the test only cares that the
# pipeline plumbing works, not that ffmpeg parsing is accurate.
echo '#EXTM3U' > "$last"
touch "$outdir/stream0.ts"
# Drain stdin into a sink so the pump doesn't block forever waiting
# for backpressure relief.
cat > "$outdir/stdin.bin"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestManager_OpenCreatesPlaylist(t *testing.T) {
	ft := &fakeTuner{makeStream: func(ctx context.Context) tuner.TsStream {
		return &holdStream{ctx: ctx, body: bytes.Repeat([]byte{0x47}, 4096), done: make(chan struct{})}
	}}
	m := &Manager{
		Tuners:     newPool(t, ft),
		OutputRoot: t.TempDir(),
		FFmpegBin:  writeFakeFFmpeg(t),
	}
	s, err := m.Open(context.Background(), "mx", "")
	if err != nil {
		t.Fatal(err)
	}
	// Wait for fake-ffmpeg to finish + write the playlist.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(s.PlaylistPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(s.PlaylistPath); err != nil {
		t.Fatalf("playlist not created: %v", err)
	}
	if s.Channel != "mx" {
		t.Fatalf("channel: %s", s.Channel)
	}
	m.Close("mx")
}

func TestManager_OpenSecondTimeReturnsSameSession(t *testing.T) {
	ft := &fakeTuner{makeStream: func(ctx context.Context) tuner.TsStream {
		return &holdStream{ctx: ctx, done: make(chan struct{})}
	}}
	m := &Manager{
		Tuners:     newPool(t, ft),
		OutputRoot: t.TempDir(),
		FFmpegBin:  writeFakeFFmpeg(t),
	}
	s1, err := m.Open(context.Background(), "mx", "")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := m.Open(context.Background(), "mx", "")
	if err != nil {
		t.Fatal(err)
	}
	if s1 != s2 {
		t.Fatal("second Open should return same session")
	}
	m.Close("mx")
}

func TestManager_TouchUpdatesLastSeen(t *testing.T) {
	ft := &fakeTuner{makeStream: func(ctx context.Context) tuner.TsStream {
		return &holdStream{ctx: ctx, done: make(chan struct{})}
	}}
	m := &Manager{
		Tuners:     newPool(t, ft),
		OutputRoot: t.TempDir(),
		FFmpegBin:  writeFakeFFmpeg(t),
	}
	s, _ := m.Open(context.Background(), "mx", "")
	defer m.Close("mx")
	before := s.lastSeen
	time.Sleep(50 * time.Millisecond)
	if m.Touch("mx", "") == nil {
		t.Fatal("Touch returned nil for existing session")
	}
	if !s.lastSeen.After(before) {
		t.Fatalf("lastSeen not advanced: %v vs %v", before, s.lastSeen)
	}
}

func TestManager_TouchReturnsNilForUnknownChannel(t *testing.T) {
	m := &Manager{
		Tuners: newPool(t, &fakeTuner{makeStream: func(ctx context.Context) tuner.TsStream {
			return &holdStream{ctx: ctx, done: make(chan struct{})}
		}}),
		OutputRoot: t.TempDir(),
		FFmpegBin:  writeFakeFFmpeg(t),
	}
	if m.Touch("ghost", "") != nil {
		t.Fatal("expected nil for unknown channel")
	}
}

func TestManager_JanitorClosesIdleSessions(t *testing.T) {
	ft := &fakeTuner{makeStream: func(ctx context.Context) tuner.TsStream {
		return &holdStream{ctx: ctx, done: make(chan struct{})}
	}}
	m := &Manager{
		Tuners:      newPool(t, ft),
		OutputRoot:  t.TempDir(),
		FFmpegBin:   writeFakeFFmpeg(t),
		IdleTimeout: 200 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	s, err := m.Open(context.Background(), "mx", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = s

	// Wait > IdleTimeout + tick interval.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		_, exists := m.sessions[sessionKey{channel: "mx", quality: DefaultQualityName}]
		m.mu.Unlock()
		if !exists {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("janitor did not close idle session")
}

func TestParseAudioOffsetJSON(t *testing.T) {
	// Audio leads video by 0.4s → offset video−audio = +0.4 (delay audio).
	good := []byte(`{
		"packets": [
			{"stream_index": 1, "pts_time": "1.000000"},
			{"stream_index": 0, "pts_time": "1.400000"},
			{"stream_index": 1, "pts_time": "1.020000"}
		],
		"streams": [
			{"index": 0, "codec_type": "video"},
			{"index": 1, "codec_type": "audio"}
		]
	}`)
	off, err := parseAudioOffsetJSON(good)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(off-0.4) > 1e-6 {
		t.Fatalf("offset: want 0.4, got %v", off)
	}

	// Only one stream type present → error (can't compute an offset).
	bad := []byte(`{
		"packets": [{"stream_index": 1, "pts_time": "1.0"}],
		"streams": [{"index": 1, "codec_type": "audio"}]
	}`)
	if _, err := parseAudioOffsetJSON(bad); err == nil {
		t.Fatal("expected error when video PTS missing")
	}
}

func TestAudioOffsetFilter(t *testing.T) {
	cases := []struct {
		offset float64
		want   string
	}{
		{0.4, "asetpts=PTS+0.4/TB"},    // delay audio
		{-0.25, "asetpts=PTS-0.25/TB"}, // advance audio
		{0, ""},                        // negligible → no filter
		{0.005, ""},                    // below minAudioOffset
	}
	for _, c := range cases {
		if got := audioOffsetFilter(c.offset); got != c.want {
			t.Errorf("audioOffsetFilter(%v) = %q, want %q", c.offset, got, c.want)
		}
	}
}

func TestManager_AcquireFailurePropagates(t *testing.T) {
	m := &Manager{
		Tuners:     &failingPool{},
		OutputRoot: t.TempDir(),
		FFmpegBin:  writeFakeFFmpeg(t),
	}
	_, err := m.Open(context.Background(), "mx", "")
	if err == nil {
		t.Fatal("expected acquire error")
	}
}

type failingPool struct{}

func (failingPool) Acquire(context.Context, string) (*tuner.Lease, error) {
	return nil, io.ErrUnexpectedEOF
}
