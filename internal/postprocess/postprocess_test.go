package postprocess

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DuckFeather10086/ferrite/internal/store"
)

// script writes an executable stand-in for one of the binaries the runner
// spawns, so a test can watch what it is asked to do without an encoder.
func script(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeFFmpeg writes something to the last argument — the output path — and
// records the whole command line for inspection.
func fakeFFmpeg(t *testing.T, dir string) string {
	return script(t, dir, "ffmpeg", `
for last; do :; done
echo "$@" > `+filepath.Join(dir, "ffmpeg.args")+`
printf 'an-mp4' > "$last"
`)
}

// fakeCaption prints a subtitle file for whichever form it was asked for.
func fakeCaption(t *testing.T, dir string) string {
	return script(t, dir, "arib-caption", `
cat > /dev/null
case "$1" in
  ass) echo "[Script Info]"; echo "PlayResX: 960"; echo "Dialogue: 0,0:00:00.00,0:00:02.00,Default,,0,0,0,,x" ;;
  vtt) echo "WEBVTT"; echo; echo "00:00:00.000 --> 00:00:02.000"; echo "x" ;;
esac
`)
}

func setup(t *testing.T) (*store.Store, *Runner, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	bin := t.TempDir()
	return st, &Runner{
		Store:       st,
		StorageRoot: dir,
		FFmpegBin:   fakeFFmpeg(t, bin),
		CaptionBin:  fakeCaption(t, bin),
	}, dir
}

// finished writes a recording that is done, with a file on disk.
func finished(t *testing.T, st *store.Store, dir, name string) (int64, string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(dir, "recordings", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("transport stream"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := st.CreateRecording(ctx, store.Recording{Channel: "NHK_G", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinalizeRecording(ctx, id, time.Now(), 16, store.RecordingStateDone, ""); err != nil {
		t.Fatal(err)
	}
	return id, path
}

func TestProducesTheMp4AndBothSidecars(t *testing.T) {
	st, r, dir := setup(t)
	id, src := finished(t, st, dir, "x.ts")

	r.runOne(context.Background(), id)

	for _, ext := range []string{".mp4", ".ass", ".vtt"} {
		if _, err := os.Stat(strings.TrimSuffix(src, ".ts") + ext); err != nil {
			t.Errorf("no %s beside the recording: %v", ext, err)
		}
	}
	// The source is what came off the air; nothing here may consume it.
	if _, err := os.Stat(src); err != nil {
		t.Errorf("the recording itself is gone: %v", err)
	}
	rec, _ := st.GetRecording(context.Background(), id)
	if rec.PostState != store.PostStateDone {
		t.Fatalf("state %q err %q", rec.PostState, rec.PostError)
	}
}

// With DeleteSource the .ts goes once the MP4 is there — and only then, and
// only if it is: the sidecars are decoded from the .ts, so a run that deletes
// it before writing them has thrown away the captions.
func TestDeleteSourceRemovesTheTsAndNothingElse(t *testing.T) {
	st, r, dir := setup(t)
	r.DeleteSource = true
	id, src := finished(t, st, dir, "x.ts")

	r.runOne(context.Background(), id)

	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("the .ts survived the post-pass: %v", err)
	}
	for _, ext := range []string{".mp4", ".ass", ".vtt"} {
		if _, err := os.Stat(strings.TrimSuffix(src, ".ts") + ext); err != nil {
			t.Errorf("no %s beside the recording: %v", ext, err)
		}
	}
	rec, _ := st.GetRecording(context.Background(), id)
	if rec.PostState != store.PostStateDone {
		t.Fatalf("state %q err %q", rec.PostState, rec.PostError)
	}
}

// A transcode that failed keeps the source: it is the only file of the set
// that cannot be made again, and the run that would replace it produced
// nothing.
func TestDeleteSourceKeepsTheTsWhenTheTranscodeFails(t *testing.T) {
	st, r, dir := setup(t)
	r.DeleteSource = true
	r.FFmpegBin = script(t, t.TempDir(), "ffmpeg", "exit 1\n")
	id, src := finished(t, st, dir, "x.ts")

	r.runOne(context.Background(), id)

	if _, err := os.Stat(src); err != nil {
		t.Fatalf("the .ts went with a failed transcode: %v", err)
	}
}

// The caption PID must never reach the muxer: ffmpeg has no ARIB decoder and
// refuses the whole output rather than skipping the stream.
func TestTheTranscodeDropsEverythingButOneVideoAndOneAudio(t *testing.T) {
	st, r, dir := setup(t)
	id, _ := finished(t, st, dir, "x.ts")
	r.runOne(context.Background(), id)

	args, err := os.ReadFile(filepath.Join(filepath.Dir(r.FFmpegBin), "ffmpeg.args"))
	if err != nil {
		t.Fatal(err)
	}
	// -f mp4 because the output is written as `.mp4.part`, and ffmpeg picks
	// its muxer from the extension: without it the encode never starts.
	for _, want := range []string{"-map 0:v:0", "-map 0:a:0", "-sn", "-dn", "+faststart", "-f mp4"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("ffmpeg was not given %q:\n%s", want, args)
		}
	}
}

// A recording of a channel whose skew was measured live gets the same
// correction, or the MP4 keeps ISDB's audio-ahead-of-video offset.
func TestTheMeasuredAudioSkewIsApplied(t *testing.T) {
	st, r, dir := setup(t)
	if err := st.PutAudioOffset(context.Background(), "NHK_G", 0.45); err != nil {
		t.Fatal(err)
	}
	id, _ := finished(t, st, dir, "x.ts")
	r.runOne(context.Background(), id)

	args, _ := os.ReadFile(filepath.Join(filepath.Dir(r.FFmpegBin), "ffmpeg.args"))
	if !strings.Contains(string(args), "asetpts=PTS+0.45/TB") {
		t.Fatalf("no audio shift in:\n%s", args)
	}
}

func TestAFailedTranscodeLeavesNoHalfFileAndSaysWhy(t *testing.T) {
	st, r, dir := setup(t)
	r.FFmpegBin = script(t, filepath.Dir(r.FFmpegBin), "ffmpeg-bad",
		"echo 'Unknown encoder h264_vaapi' >&2\nexit 1\n")
	id, src := finished(t, st, dir, "x.ts")

	r.runOne(context.Background(), id)

	if _, err := os.Stat(strings.TrimSuffix(src, ".ts") + ".mp4"); err == nil {
		t.Error("a failed transcode left an .mp4")
	}
	if _, err := os.Stat(strings.TrimSuffix(src, ".ts") + ".mp4.part"); err == nil {
		t.Error("a failed transcode left its .part file")
	}
	rec, _ := st.GetRecording(context.Background(), id)
	if rec.PostState != store.PostStateFailed || rec.PostError == "" {
		t.Fatalf("state %q err %q, want a failure with a reason", rec.PostState, rec.PostError)
	}
}

// Captions are the cheap half and the optional half: a programme with none,
// or a box with no decoder installed, still gets its MP4.
func TestNoCaptionDecoderStillProducesTheMp4(t *testing.T) {
	st, r, dir := setup(t)
	r.CaptionBin = script(t, filepath.Dir(r.FFmpegBin), "arib-missing", "exit 127\n")
	id, src := finished(t, st, dir, "x.ts")

	r.runOne(context.Background(), id)

	if _, err := os.Stat(strings.TrimSuffix(src, ".ts") + ".mp4"); err != nil {
		t.Errorf("no mp4: %v", err)
	}
	if _, err := os.Stat(strings.TrimSuffix(src, ".ts") + ".ass"); err == nil {
		t.Error("an empty sidecar was written")
	}
	rec, _ := st.GetRecording(context.Background(), id)
	if rec.PostState != store.PostStateDone {
		t.Fatalf("state %q, want done — the captions are not the job", rec.PostState)
	}
}

// The row's path is a filesystem path in a database, and this end of it
// deletes files.
func TestARowPointingOutsideTheStorageRootIsRefused(t *testing.T) {
	st, r, dir := setup(t)
	outside := filepath.Join(t.TempDir(), "elsewhere.ts")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id, err := st.CreateRecording(ctx, store.Recording{Channel: "NHK_G", Path: outside})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.FinalizeRecording(ctx, id, time.Now(), 1, store.RecordingStateDone, "")

	r.runOne(ctx, id)

	if _, err := os.Stat(strings.TrimSuffix(outside, ".ts") + ".mp4"); err == nil {
		t.Fatal("wrote a file outside the storage root")
	}
	rec, _ := st.GetRecording(ctx, id)
	if rec.PostState != store.PostStateFailed {
		t.Fatalf("state %q, want failed", rec.PostState)
	}
	_ = dir
}

// A missed recording is unrecoverable and a late transcode is not, so the
// post-pass waits rather than competing for the CPU the encoder needs.
func TestItWaitsWhileARecordingIsRunning(t *testing.T) {
	st, r, dir := setup(t)
	ctx := context.Background()
	done, src := finished(t, st, dir, "x.ts")
	// A second row left in state 'recording' is what an in-progress
	// recording looks like from here.
	if _, err := st.CreateRecording(ctx, store.Recording{
		Channel: "TBS1", Path: filepath.Join(dir, "recordings", "busy.ts"),
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	r.runOne(ctx, done)

	if _, err := os.Stat(strings.TrimSuffix(src, ".ts") + ".mp4"); err == nil {
		t.Fatal("transcoded while a recording was in progress")
	}
	rec, _ := st.GetRecording(context.Background(), done)
	if rec.PostState != store.PostStatePending {
		t.Fatalf("state %q, want it still queued for later", rec.PostState)
	}
}
