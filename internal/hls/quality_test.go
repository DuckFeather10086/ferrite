package hls

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DuckFeather10086/ferrite/internal/tuner"
)

func twoTierManager(t *testing.T) (*Manager, *tuner.Pool, *fakeTuner) {
	t.Helper()
	ft := &fakeTuner{makeStream: func(ctx context.Context) tuner.TsStream {
		return &holdStream{ctx: ctx, body: bytes.Repeat([]byte{0x47}, 4096), done: make(chan struct{})}
	}}
	pool := newPool(t, ft)
	m := &Manager{
		Tuners:     pool,
		OutputRoot: t.TempDir(),
		FFmpegBin:  writeFakeFFmpeg(t),
		// No A/V probe: these tests are about how tiers share a tune, and
		// probing would spend the ffprobe timeout per open against a fake.
		ProbeSeconds: -1,
		Qualities: []Quality{
			{Name: "1080p", Label: "1080p", Bandwidth: 6_500_000},
			{Name: "720p", Label: "720p", Bandwidth: 3_000_000,
				OutputArgs: []string{"-c:v", "libx264", "-b:v", "3M"}},
		},
	}
	return m, pool, ft
}

// The premise of the whole feature: a second quality is another encode of
// the same tune, not another claim on the tuner. On a one-adapter box the
// alternative is that asking for 720p while someone watches 1080p fails
// with "no adapter available".
func TestTiersShareOneTune(t *testing.T) {
	m, pool, ft := twoTierManager(t)
	defer m.closeAll()

	hd, err := m.Open(context.Background(), "mx", "1080p")
	if err != nil {
		t.Fatal(err)
	}
	sd, err := m.Open(context.Background(), "mx", "720p")
	if err != nil {
		t.Fatalf("a second tier must join the tune, not fight for the adapter: %v", err)
	}

	if n := ft.tuneCount(); n != 1 {
		t.Fatalf("the frontend was tuned %d times for two tiers of one channel", n)
	}
	if hd.Dir == sd.Dir {
		t.Fatal("both tiers are writing to one directory; they use the same segment names")
	}
	if want := filepath.Join(m.OutputRoot, "mx", "720p"); sd.Dir != want {
		t.Errorf("720p writes to %q, want %q", sd.Dir, want)
	}
	if st := pool.Status(); st[0].Channel != "mx" {
		t.Fatalf("adapter should be on mx: %+v", st)
	}
}

// And the lease goes when the last tier does — not when the first one
// does, which would take the picture away from whoever is still watching.
func TestLeaseSurvivesUntilTheLastTierCloses(t *testing.T) {
	m, pool, _ := twoTierManager(t)

	if _, err := m.Open(context.Background(), "mx", "1080p"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Open(context.Background(), "mx", "720p"); err != nil {
		t.Fatal(err)
	}

	m.closeSession(sessionKey{channel: "mx", quality: "1080p"})
	if st := pool.Status(); st[0].Channel != "mx" {
		t.Fatalf("closing one tier released the tune the other is using: %+v", st)
	}

	m.closeSession(sessionKey{channel: "mx", quality: "720p"})
	// Release tears the pump down asynchronously; give it a moment.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st := pool.Status(); st[0].Channel == "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the adapter is still held after the last tier closed")
}

// Close is whole-channel: stopping live playback, or freeing the adapter
// for another channel, means every tier of it.
func TestCloseTakesEveryTierOfTheChannel(t *testing.T) {
	m, _, _ := twoTierManager(t)

	if _, err := m.Open(context.Background(), "mx", "1080p"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Open(context.Background(), "mx", "720p"); err != nil {
		t.Fatal(err)
	}
	m.Close("mx")

	m.mu.Lock()
	n := len(m.sessions)
	tunes := len(m.tunes)
	m.mu.Unlock()
	if n != 0 || tunes != 0 {
		t.Fatalf("Close left %d session(s) and %d tune(s) behind", n, tunes)
	}
}

// CloseOthers reports each channel once however many tiers it was running,
// because the caller is telling a viewer what it stopped.
func TestCloseOthersDeduplicatesChannels(t *testing.T) {
	m, _, _ := twoTierManager(t)
	defer m.closeAll()

	if _, err := m.Open(context.Background(), "nhk", "1080p"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Open(context.Background(), "nhk", "720p"); err != nil {
		t.Fatal(err)
	}

	closed := m.CloseOthers("mx")
	if len(closed) != 1 || closed[0] != "nhk" {
		t.Fatalf("CloseOthers = %v, want [nhk] once", closed)
	}
}

// A tier's ffmpeg gets its own encoder arguments, and the GOP is not one
// of them: every HLS segment has to start on an IDR frame, so the
// keyframe interval belongs to the segment length and not to the tier.
func TestTierArgumentsReachFFmpeg(t *testing.T) {
	m, _, _ := twoTierManager(t)
	defer m.closeAll()
	// A fake ffmpeg that records its own argv beside the playlist.
	m.FFmpegBin = writeArgvRecorder(t)

	s, err := m.Open(context.Background(), "mx", "720p")
	if err != nil {
		t.Fatal(err)
	}
	argv := readFile(t, filepath.Join(s.Dir, "argv"))

	for _, want := range []string{"-b:v 3M", "-g 30", "-hls_time 1", "-hls_list_size 12"} {
		if !contains(argv, want) {
			t.Errorf("argv is missing %q:\n%s", want, argv)
		}
	}
	// The default tier's encoder must not have leaked into this one.
	if contains(argv, "-b:v 6M") {
		t.Errorf("the default encode's bitrate reached a tier that set its own:\n%s", argv)
	}
	// Segments resolve under the tier's own path.
	if !contains(argv, "-hls_base_url mx/720p/") {
		t.Errorf("segment URIs would resolve outside the tier's directory:\n%s", argv)
	}
}

// A daemon that declares no tiers runs exactly one, and it is the encode
// it did before tiers existed.
func TestNoConfiguredTiersMeansTheDefaultOne(t *testing.T) {
	m := &Manager{}
	list := m.QualityList()
	if len(list) != 1 || list[0].Name != DefaultQualityName {
		t.Fatalf("QualityList() = %+v, want the single default tier", list)
	}
	if got := m.ResolveQuality("720p"); got.Name != DefaultQualityName {
		t.Errorf("an unknown tier should fall back to the default, got %q", got.Name)
	}
	if got := m.ResolveQuality(""); got.Name != DefaultQualityName {
		t.Errorf("an unnamed tier should be the default, got %q", got.Name)
	}
}

// The first configured tier is the default, so a client that does not ask
// — a bookmark, VLC — gets the one the config leads with.
func TestFirstConfiguredTierIsTheDefault(t *testing.T) {
	m, _, _ := twoTierManager(t)
	if got := m.ResolveQuality(""); got.Name != "1080p" {
		t.Fatalf("default tier is %q, want the first configured one", got.Name)
	}
	if got := m.ResolveQuality("720p"); got.Name != "720p" {
		t.Fatalf("ResolveQuality(720p) = %q", got.Name)
	}
}

// One caption decode per channel, however many tiers are running: the
// words are the same at every bitrate, and a decoder per tier would be N
// arib-caption children reading N subscriptions of one TS to produce N
// copies of the same cues.
//
// Each tier still gets its own rendition of those cues. It has to: a
// player matches a subtitle fragment to the video fragment it is playing,
// and two encodes started minutes apart number their segments from their
// own zero.
func TestOneCaptionDecodePerChannelManyRenditions(t *testing.T) {
	m, _, _ := twoTierManager(t)
	defer m.closeAll()
	m.CaptionBin = writeDrainer(t)
	m.FFprobeBin = writeDrainer(t)

	hd, err := m.Open(context.Background(), "mx", "1080p")
	if err != nil {
		t.Fatal(err)
	}
	sd, err := m.Open(context.Background(), "mx", "720p")
	if err != nil {
		t.Fatal(err)
	}
	if !hd.Captions || !sd.Captions {
		t.Fatal("both tiers should carry captions")
	}

	m.mu.Lock()
	tune := m.tunes["mx"]
	m.mu.Unlock()
	if tune == nil || tune.pipeline == nil {
		t.Fatal("no caption pipeline on the channel's tune")
	}
	if hd.tune != sd.tune {
		t.Fatal("the tiers are not sharing the channel's tune")
	}
	if hd.rendition == nil || sd.rendition == nil || hd.rendition == sd.rendition {
		t.Fatal("each tier needs its own rendition, aligned to its own segments")
	}
	if hd.rendition.Dir() == sd.rendition.Dir() {
		t.Fatal("both renditions are writing subs.m3u8 to the same directory")
	}

	// Closing one tier stops only its rendition; the decode belongs to the
	// channel and the other tier is still using it.
	m.closeSession(sessionKey{channel: "mx", quality: "1080p"})
	m.mu.Lock()
	stillThere := m.tunes["mx"] != nil && m.tunes["mx"].pipeline != nil
	m.mu.Unlock()
	if !stillThere {
		t.Fatal("closing one tier tore down the channel's caption decode")
	}
}

// writeDrainer is a child that reads its stdin and says nothing — enough
// for the wiring around a decoder to be exercised without one.
func writeDrainer(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "drainer")
	if err := os.WriteFile(path, []byte("#!/bin/sh\ncat > /dev/null\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeArgvRecorder is writeFakeFFmpeg plus a dump of its own arguments.
func writeArgvRecorder(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffmpeg")
	script := `#!/bin/sh
for last; do :; done
outdir="$(dirname "$last")"
mkdir -p "$outdir"
echo "$@" > "$outdir/argv"
echo '#EXTM3U' > "$last"
touch "$outdir/stream0.ts"
cat > /dev/null
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	// The child writes this as it starts; Open returns as soon as it is
	// spawned, so give it a moment to appear.
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			return string(data)
		}
		if time.Now().After(deadline) {
			t.Fatalf("read %s: %v", path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && bytes.Contains([]byte(haystack), []byte(needle))
}
