package hls

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A recording at a tier is one encode, shared: the second viewer joins the
// first one's ffmpeg rather than starting a second over the same directory,
// which would have both writing the same segment names.
func TestVODSharesOneEncodePerTier(t *testing.T) {
	v, rec := newTestVOD(t)

	a, err := v.Open(context.Background(), 7, rec, "nhk", "480p")
	if err != nil {
		t.Fatal(err)
	}
	b, err := v.Open(context.Background(), 7, rec, "nhk", "480p")
	if err != nil {
		t.Fatal(err)
	}
	if a.Dir != b.Dir {
		t.Errorf("two viewers got two directories: %q and %q", a.Dir, b.Dir)
	}
	if n := countSessions(v); n != 1 {
		t.Errorf("%d sessions for one recording at one tier, want 1", n)
	}

	// A different tier of the same recording is a different encode, and its
	// own directory — see the live rule it mirrors.
	c, err := v.Open(context.Background(), 7, rec, "nhk", "720p")
	if err != nil {
		t.Fatal(err)
	}
	if c.Dir == a.Dir {
		t.Errorf("720p writes into 480p's directory %q", c.Dir)
	}
	if n := countSessions(v); n != 2 {
		t.Errorf("%d sessions for two tiers, want 2", n)
	}
}

// The encode is a cache of something reproducible, so an abandoned one is
// killed and its segments removed — a full-length tier is hundreds of
// megabytes, and nothing else ever deletes them.
func TestVODReapsIdleAndRemovesSegments(t *testing.T) {
	v, rec := newTestVOD(t)

	s, err := v.Open(context.Background(), 7, rec, "nhk", "480p")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Dir); err != nil {
		t.Fatalf("the session has no directory: %v", err)
	}

	// Not idle yet.
	v.reapIdle(time.Hour)
	if n := countSessions(v); n != 1 {
		t.Fatalf("a session touched a moment ago was reaped")
	}
	if v.Touch(7, "480p") == nil {
		t.Fatal("Touch does not find a running session")
	}

	v.reapIdle(-time.Second) // everything is now older than the timeout
	if n := countSessions(v); n != 0 {
		t.Errorf("%d sessions survived the janitor, want 0", n)
	}
	if _, err := os.Stat(s.Dir); !os.IsNotExist(err) {
		t.Errorf("the segments outlived the session: %v", err)
	}
	if v.Touch(7, "480p") != nil {
		t.Error("Touch still finds a reaped session")
	}
}

// Deleting a recording has to take any encode of it with it: an ffmpeg
// reading a file that is about to be unlinked keeps writing segments of a
// recording that no longer exists.
func TestVODCloseTakesEveryTier(t *testing.T) {
	v, rec := newTestVOD(t)

	for _, q := range []string{"480p", "720p"} {
		if _, err := v.Open(context.Background(), 7, rec, "nhk", q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := v.Open(context.Background(), 8, rec, "nhk", "480p"); err != nil {
		t.Fatal(err)
	}

	v.Close(7)
	if n := countSessions(v); n != 1 {
		t.Errorf("%d sessions after closing recording 7, want 1 (recording 8's)", n)
	}
	if _, err := os.Stat(filepath.Join(v.OutputRoot, "7")); !os.IsNotExist(err) {
		t.Errorf("recording 7's directory outlived the delete: %v", err)
	}
	if v.Touch(8, "480p") == nil {
		t.Error("closing one recording closed another's encode")
	}
}

// The tier decides the filter chain and the encoder; the GOP is appended
// after it and is not the tier's to set, exactly as for live — every HLS
// segment has to begin on an IDR frame. And the playlist is an EVENT one:
// a recording is something a viewer seeks around in, so segments are kept
// rather than rolled off the front.
func TestVODFFmpegArgs(t *testing.T) {
	v, rec := newTestVOD(t)

	s, err := v.Open(context.Background(), 7, rec, "nhk", "480p")
	if err != nil {
		t.Fatal(err)
	}
	argv := readFile(t, filepath.Join(s.Dir, "argv"))

	for _, want := range []string{
		"-i " + rec,     // the recording itself, not a pipe
		"-vf scale=480", // the tier's own chain
		"-g 60",         // segmentSeconds × outputFPS, appended after it
		"-hls_playlist_type event",
		"-hls_time 2",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("ffmpeg was not given %q:\n%s", want, argv)
		}
	}
	// Live's rolling window would delete the beginning of the programme out
	// from under a viewer who wanted to go back to it.
	if strings.Contains(argv, "delete_segments") {
		t.Errorf("a recording's segments are being deleted as it plays:\n%s", argv)
	}
}

// A recording whose .ts has been removed (transcode.delete_source) is still
// watchable at a tier: the post-pass MP4 stands in, and because that one is
// already A/V-corrected it must not be corrected a second time.
func TestVODFallsBackToTheMP4(t *testing.T) {
	v, rec := newTestVOD(t)
	mp4 := strings.TrimSuffix(rec, filepath.Ext(rec)) + ".mp4"
	if err := os.WriteFile(mp4, []byte("mp4"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(rec); err != nil {
		t.Fatal(err)
	}

	s, err := v.Open(context.Background(), 7, rec, "nhk", "480p")
	if err != nil {
		t.Fatal(err)
	}
	argv := readFile(t, filepath.Join(s.Dir, "argv"))
	if !strings.Contains(argv, "-i "+mp4) {
		t.Errorf("the MP4 did not stand in for the missing .ts:\n%s", argv)
	}

	// And with neither on disk it is an error rather than an ffmpeg that
	// fails a second later with nothing to say.
	if err := os.Remove(mp4); err != nil {
		t.Fatal(err)
	}
	v.Close(7)
	if _, err := v.Open(context.Background(), 7, rec, "nhk", "720p"); err == nil {
		t.Error("opening a recording with no file at all succeeded")
	}
}

// An encode that dies partway writes no ENDLIST — ffmpeg only writes one
// when it reaches the end of the file — and a player reads a playlist that
// has stopped growing as an encoder that is being slow, and polls it for
// ever. Ending it where the picture stopped is at least the truth.
func TestVODFailedEncodeEndsItsPlaylist(t *testing.T) {
	v, rec := newTestVOD(t)
	v.FFmpegBin = writeFailingFFmpeg(t)

	s, err := v.Open(context.Background(), 7, rec, "nhk", "480p")
	if err != nil {
		t.Fatal(err)
	}
	// Poll: the child's exit and the goroutine that answers it are both
	// asynchronous.
	until := time.Now().Add(2 * time.Second)
	for time.Now().Before(until) {
		if strings.Contains(readFile(t, s.PlaylistPath), "#EXT-X-ENDLIST") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("a failed transcode left its playlist open:\n%s", readFile(t, s.PlaylistPath))
}

// writeFailingFFmpeg writes a playlist and then dies, which is a transcode
// that fell over partway.
func writeFailingFFmpeg(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffmpeg")
	script := `#!/bin/sh
for last; do :; done
mkdir -p "$(dirname "$last")"
printf '#EXTM3U\n#EXT-X-PLAYLIST-TYPE:EVENT\n' > "$last"
exit 1
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestVOD(t *testing.T) (*VOD, string) {
	t.Helper()
	dir := t.TempDir()
	rec := filepath.Join(dir, "programme.ts")
	if err := os.WriteFile(rec, []byte("ts"), 0o644); err != nil {
		t.Fatal(err)
	}
	v := &VOD{
		OutputRoot: filepath.Join(dir, "vod"),
		FFmpegBin:  writeArgvRecorder(t),
		Qualities: []Quality{
			{Name: "720p", Label: "720p", OutputArgs: []string{"-vf", "scale=720", "-c:v", "libx264"}},
			{Name: "480p", Label: "480p", OutputArgs: []string{"-vf", "scale=480", "-c:v", "libx264"}},
		},
	}
	t.Cleanup(v.closeAll)
	return v, rec
}

func countSessions(v *VOD) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.sessions)
}
