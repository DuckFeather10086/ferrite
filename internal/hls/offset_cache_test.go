package hls

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/DuckFeather10086/ferrite/internal/store"
	"github.com/DuckFeather10086/ferrite/internal/tuner"
)

// memOffsets is an in-memory OffsetStore that counts writes.
type memOffsets struct {
	mu     sync.Mutex
	rows   map[string]store.AudioOffset
	puts   int
	getErr error
}

func newMemOffsets() *memOffsets {
	return &memOffsets{rows: map[string]store.AudioOffset{}}
}

func (m *memOffsets) AudioOffsetFor(_ context.Context, channel string) (store.AudioOffset, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return store.AudioOffset{}, false, m.getErr
	}
	rec, ok := m.rows[channel]
	return rec, ok, nil
}

func (m *memOffsets) PutAudioOffset(_ context.Context, channel string, offsetS float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.puts++
	m.rows[channel] = store.AudioOffset{
		Channel: channel, OffsetS: offsetS, Measured: time.Now().UTC(),
	}
	return nil
}

func (m *memOffsets) putAged(channel string, offsetS float64, age time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[channel] = store.AudioOffset{
		Channel: channel, OffsetS: offsetS,
		Measured: time.Now().UTC().Add(-age),
	}
}

// writeProbeCounter drops a fake ffprobe that appends a line to a
// counter file each time it runs, so a test can prove the probe was (or
// wasn't) executed. Its JSON output is deliberately unusable, which
// makes probeAudioOffset fail — the offset then stays 0, and that is
// enough to distinguish "probed" from "used the cache".
func writeProbeCounter(t *testing.T, counter string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffprobe")
	script := "#!/bin/sh\necho ran >> " + counter + "\necho '{}'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func probeRuns(t *testing.T, counter string) int {
	t.Helper()
	b, err := os.ReadFile(counter)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Count(b, []byte("\n"))
}

func newOffsetManager(t *testing.T, offsets OffsetStore, ffprobe string) *Manager {
	t.Helper()
	ft := &fakeTuner{makeStream: func(ctx context.Context) tuner.TsStream {
		return &holdStream{ctx: ctx, body: bytes.Repeat([]byte{0x47}, 4096), done: make(chan struct{})}
	}}
	return &Manager{
		Tuners:     newPool(t, ft),
		OutputRoot: t.TempDir(),
		FFmpegBin:  writeFakeFFmpeg(t),
		FFprobeBin: ffprobe,
		Offsets:    offsets,
	}
}

// A cached measurement must skip the ffprobe pass entirely — that pass
// is the bulk of channel-change latency.
func TestManager_CachedOffsetSkipsProbe(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "runs")
	offsets := newMemOffsets()
	offsets.putAged("mx", 0.25, time.Hour)

	m := newOffsetManager(t, offsets, writeProbeCounter(t, counter))
	if _, err := m.Open(context.Background(), "mx", ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close("mx") })

	if n := probeRuns(t, counter); n != 0 {
		t.Fatalf("ffprobe ran %d times, want 0 (cache hit)", n)
	}
	if offsets.puts != 0 {
		t.Fatalf("puts = %d, want 0 (nothing new measured)", offsets.puts)
	}
}

// A cache miss probes and persists the raw measurement.
func TestManager_ProbeResultIsCached(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "runs")
	offsets := newMemOffsets()

	// A probe that reports a usable offset: video at 0.5, audio at 0.2.
	probe := filepath.Join(t.TempDir(), "ffprobe")
	script := `#!/bin/sh
echo ran >> ` + counter + `
cat <<'JSON'
{"streams":[{"index":0,"codec_type":"video"},{"index":1,"codec_type":"audio"}],
 "packets":[{"stream_index":0,"pts_time":"0.500000"},{"stream_index":1,"pts_time":"0.200000"}]}
JSON
`
	if err := os.WriteFile(probe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	m := newOffsetManager(t, offsets, probe)
	if _, err := m.Open(context.Background(), "mx", ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close("mx") })

	if n := probeRuns(t, counter); n != 1 {
		t.Fatalf("ffprobe ran %d times, want 1", n)
	}
	rec, ok, _ := offsets.AudioOffsetFor(context.Background(), "mx")
	if !ok {
		t.Fatal("measurement was not cached")
	}
	if diff := rec.OffsetS - 0.3; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("cached offset = %v, want 0.3 (video 0.5 − audio 0.2)", rec.OffsetS)
	}
}

// AudioOffsetBias is applied on top of the cached raw value, so tuning
// it in config takes effect without invalidating the cache.
func TestManager_BiasIsNotBakedIntoCache(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "runs")
	offsets := newMemOffsets()
	offsets.putAged("mx", 0.25, time.Hour)

	m := newOffsetManager(t, offsets, writeProbeCounter(t, counter))
	m.AudioOffsetBias = 0.1
	if _, err := m.Open(context.Background(), "mx", ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close("mx") })

	rec, _, _ := offsets.AudioOffsetFor(context.Background(), "mx")
	if rec.OffsetS != 0.25 {
		t.Fatalf("cached value = %v, want the raw 0.25 without the bias", rec.OffsetS)
	}
}

// A measurement past OffsetMaxAge is re-probed.
func TestManager_StaleOffsetIsReprobed(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "runs")
	offsets := newMemOffsets()
	offsets.putAged("mx", 0.25, 48*time.Hour)

	m := newOffsetManager(t, offsets, writeProbeCounter(t, counter))
	m.OffsetMaxAge = time.Hour
	if _, err := m.Open(context.Background(), "mx", ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close("mx") })

	if n := probeRuns(t, counter); n != 1 {
		t.Fatalf("ffprobe ran %d times, want 1 (stale cache)", n)
	}
}

// A negative OffsetMaxAge means "never expire".
func TestManager_NegativeMaxAgeNeverExpires(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "runs")
	offsets := newMemOffsets()
	offsets.putAged("mx", 0.25, 365*24*time.Hour)

	m := newOffsetManager(t, offsets, writeProbeCounter(t, counter))
	m.OffsetMaxAge = -1
	if _, err := m.Open(context.Background(), "mx", ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close("mx") })

	if n := probeRuns(t, counter); n != 0 {
		t.Fatalf("ffprobe ran %d times, want 0", n)
	}
}

// A broken cache must not break playback — it degrades to probing.
func TestManager_CacheReadErrorFallsBackToProbe(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "runs")
	offsets := newMemOffsets()
	offsets.getErr = context.DeadlineExceeded

	m := newOffsetManager(t, offsets, writeProbeCounter(t, counter))
	if _, err := m.Open(context.Background(), "mx", ""); err != nil {
		t.Fatalf("Open should survive a cache read failure: %v", err)
	}
	t.Cleanup(func() { m.Close("mx") })

	if n := probeRuns(t, counter); n != 1 {
		t.Fatalf("ffprobe ran %d times, want 1 (fallback)", n)
	}
}
