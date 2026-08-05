// Package postprocess turns a finished recording into something that can be
// watched somewhere other than mpv.
//
// A recording is what came off the air: MPEG-2 video in a transport stream,
// with its captions on an ARIB PID. No browser decodes either. So once the
// tuner has let go of a recording, this converts it to H.264 in an MP4 that a
// <video> element opens directly, and writes the subtitle sidecars beside it —
// `.ass` for a player that can place ARIB's own positions and colours, `.vtt`
// for anything that just wants the words.
//
// Three properties matter more than speed:
//
//   - It never competes with the tuner. Jobs run one at a time, at a lowered
//     priority, and not at all while a recording is in progress. A missed
//     recording is unrecoverable; a transcode that waits an hour is not.
//   - It is restartable. The queue is a column in the database, written in the
//     same statement that marks a recording done, so a daemon that dies
//     mid-transcode finds the job again on the next start.
//   - It never half-writes. Everything is produced under a `.part` name and
//     renamed once the child exits cleanly, so a file that exists is a file
//     that finished.
package postprocess

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/DuckFeather10086/ferrite/internal/proc"
	"github.com/DuckFeather10086/ferrite/internal/store"
)

// DefaultTranscodeArgs is the encode when the config names none: software
// H.264, the same settings the live HLS session uses.
//
// Software on purpose. A hardware encoder is two to five times cheaper and
// this box has one, but which one — and the filter chain that goes with it —
// differs per machine, and a default that assumes the wrong one fails at
// runtime rather than at startup. The config says what this machine has; see
// configs/isdbd.example.toml.
var DefaultTranscodeArgs = []string{
	"-vf", "yadif=0,scale=trunc(iw*sar/2)*2:ih,setsar=1",
	"-c:v", "libx264", "-preset", "superfast",
	"-b:v", "6M", "-maxrate", "7M", "-bufsize", "12M",
	"-pix_fmt", "yuv420p",
	"-c:a", "aac", "-b:a", "192k",
}

const (
	// How long the output may go without growing before the child is
	// presumed wedged. Ports the recorder's stall watchdog: a transcode that
	// produces nothing has to fail loudly, not hold the queue forever.
	stallTimeout = 5 * time.Minute
	stallTick    = 30 * time.Second

	// How long to wait before looking again when the tuner is busy.
	busyRetry = 2 * time.Minute

	// Niceness for the transcode. It runs beside live HLS, which is
	// soft-realtime and must win.
	niceness = 10
)

// Runner is the post-pass worker. One per daemon; [Runner.Run] is its loop.
type Runner struct {
	Store       *store.Store
	StorageRoot string
	FFmpegBin   string
	// CaptionBin is arib-caption. Empty means no subtitle sidecars — the
	// transcode still happens.
	CaptionBin string
	// InputArgs go before ffmpeg's -i (hardware decode setup, mostly);
	// TranscodeArgs are the filters and the encoder.
	InputArgs     []string
	TranscodeArgs []string
	// AudioOffsetBias is added to the cached A/V skew, as in internal/hls.
	AudioOffsetBias float64
	// DeleteSource removes the .ts once the MP4 is written. Off by default:
	// the .ts is the original, and everything here can be regenerated from
	// it but nothing can regenerate it.
	DeleteSource bool

	queue chan int64
}

// Enqueue asks for a recording to be processed. It never blocks: a full queue
// is not a reason to hold up whatever finished a recording, and the sweep at
// the next start will find anything dropped.
func (r *Runner) Enqueue(id int64) {
	if r.queue == nil {
		return
	}
	select {
	case r.queue <- id:
	default:
		slog.Warn("postprocess: queue full; leaving it for the next sweep", "id", id)
	}
}

// Run works the queue until ctx is done. Call it once, in a goroutine.
func (r *Runner) Run(ctx context.Context) error {
	if r.Store == nil || r.FFmpegBin == "" {
		return errors.New("postprocess: Store and FFmpegBin are required")
	}
	if r.queue == nil {
		r.queue = make(chan int64, 64)
	}
	slog.Info("postprocess: started",
		"encoder", strings.Join(r.transcodeArgs(), " "),
		"captions", r.CaptionBin != "")

	r.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case id := <-r.queue:
			r.runOne(ctx, id)
		}
	}
}

// sweep queues everything the last run of the daemon left unfinished.
func (r *Runner) sweep(ctx context.Context) {
	pending, err := r.Store.RecordingsToPostprocess(ctx)
	if err != nil {
		slog.Warn("postprocess: cannot read the queue", "err", err)
		return
	}
	if len(pending) == 0 {
		return
	}
	slog.Info("postprocess: picking up where the last run left off", "count", len(pending))
	for _, rec := range pending {
		r.Enqueue(rec.ID)
	}
}

// runOne processes one recording, waiting for the tuner to be free first.
func (r *Runner) runOne(ctx context.Context, id int64) {
	if !r.waitForIdle(ctx) {
		// Shutting down. The row stays 'pending', so the next start retries.
		r.Enqueue(id)
		return
	}

	rec, err := r.Store.GetRecording(ctx, id)
	if err != nil {
		slog.Warn("postprocess: cannot read the recording", "id", id, "err", err)
		return
	}
	if rec == nil || rec.State != store.RecordingStateDone {
		return
	}

	started := time.Now()
	_ = r.Store.SetPostState(ctx, id, store.PostStateRunning, "")
	err = r.process(ctx, *rec)

	switch {
	case err == nil:
		_ = r.Store.SetPostState(ctx, id, store.PostStateDone, "")
		slog.Info("postprocess: done", "id", id,
			"took", time.Since(started).Round(time.Second))
	case ctx.Err() != nil:
		// Shutdown, not failure. Put it back to pending so the next start
		// does it rather than a human having to ask.
		_ = r.Store.SetPostState(context.Background(), id, store.PostStatePending, "")
		slog.Info("postprocess: interrupted by shutdown", "id", id)
	default:
		_ = r.Store.SetPostState(context.Background(), id, store.PostStateFailed, err.Error())
		slog.Warn("postprocess: failed", "id", id, "err", err)
	}
}

// waitForIdle blocks until no recording is in progress. Returns false if ctx
// ended first.
func (r *Runner) waitForIdle(ctx context.Context) bool {
	for {
		busy, err := r.recordingInProgress(ctx)
		if err != nil {
			// Can't tell — assume busy rather than compete with a recording.
			slog.Warn("postprocess: cannot tell whether a recording is running", "err", err)
			busy = true
		}
		if !busy {
			return true
		}
		slog.Info("postprocess: a recording is running; waiting", "retry_in", busyRetry)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(busyRetry):
		}
	}
}

func (r *Runner) recordingInProgress(ctx context.Context) (bool, error) {
	all, err := r.Store.ListRecordings(ctx)
	if err != nil {
		return false, err
	}
	for _, rec := range all {
		if rec.State == store.RecordingStateRecording {
			return true, nil
		}
	}
	return false, nil
}

// process does the work for one recording: sidecars first, then the transcode.
func (r *Runner) process(ctx context.Context, rec store.Recording) error {
	src, err := r.sourcePath(rec)
	if err != nil {
		return err
	}
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("source file: %w", err)
	}

	// Subtitles first. They take seconds against the transcode's minutes, and
	// a failure here should not cost the MP4 — a recording with no captions
	// in it is ordinary, and so is one whose decoder is not installed.
	if r.CaptionBin != "" {
		for _, form := range []string{"ass", "vtt"} {
			if err := r.sidecar(ctx, src, form); err != nil {
				slog.Warn("postprocess: no subtitle sidecar written",
					"id", rec.ID, "form", form, "err", err)
			}
		}
	}

	return r.transcode(ctx, rec, src)
}

// sidecar writes one subtitle file next to the recording.
//
// `arib-caption <form>` reads a transport stream on stdin and writes the whole
// subtitle file to stdout. Its default time base is the earliest PTS in the
// file — the same zero a player uses — so one sidecar is correct against both
// the .ts and the MP4 made from it.
func (r *Runner) sidecar(ctx context.Context, src, form string) error {
	out := replaceExt(src, "."+form)
	part := out + ".part"
	defer os.Remove(part)

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	dst, err := os.Create(part)
	if err != nil {
		return err
	}
	defer dst.Close()

	child, err := proc.SpawnOpt(ctx, proc.SpawnOpts{Stdin: true}, r.CaptionBin, form)
	if err != nil {
		return fmt.Errorf("spawn %s: %w", r.CaptionBin, err)
	}
	defer child.Close()

	go func() {
		defer child.Stdin.Close()
		_, _ = io.Copy(child.Stdin, in)
	}()
	if _, err := io.Copy(dst, child.Stdout); err != nil {
		return err
	}
	if err := child.Wait(); err != nil {
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}

	// A recording of a programme with no captions is ordinary, and an empty
	// sidecar is worse than none: a player that finds one offers a subtitle
	// track with nothing in it.
	if !hasCues(part, form) {
		return errors.New("no captions in this recording")
	}
	return os.Rename(part, out)
}

// hasCues reports whether a sidecar holds anything beyond its own header.
//
// By the marker rather than by size, because the two headers are nothing
// alike: an ASS script's is several hundred bytes of style declarations, a
// WebVTT's is the word WEBVTT.
func hasCues(path, form string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	marker := " --> " // a WebVTT cue's timing line
	if form == "ass" {
		marker = "Dialogue:"
	}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.Contains(sc.Text(), marker) {
			return true
		}
	}
	return false
}

// transcode writes the MP4 a browser can play.
func (r *Runner) transcode(ctx context.Context, rec store.Recording, src string) error {
	out := replaceExt(src, ".mp4")
	part := out + ".part"

	args := []string{"-hide_banner", "-loglevel", "error", "-nostats", "-y",
		// The head of a recording is mid-GOP by construction: the recorder
		// starts writing whenever the lease delivers, not on an IDR.
		"-fflags", "+genpts+discardcorrupt"}
	args = append(args, r.InputArgs...)
	args = append(args, "-i", src,
		// One video and one audio stream. A service can carry a second
		// audio track, and the caption PID must not reach the muxer at all:
		// ffmpeg has no ARIB caption decoder and refuses the whole output.
		"-map", "0:v:0", "-map", "0:a:0", "-sn", "-dn")
	if af := audioOffsetFilter(r.audioOffset(ctx, rec.Channel)); af != "" {
		args = append(args, "-af", af)
	}
	args = append(args, r.transcodeArgs()...)
	// Progressive download: without this the index is at the end of the file
	// and a browser has to fetch all of it before showing a frame.
	args = append(args, "-movflags", "+faststart")
	// And name the muxer, because the file being written is `.mp4.part` —
	// ffmpeg picks the format from the extension and there is no such thing
	// as a .part format, so without this it refuses to open the output at all.
	args = append(args, "-f", "mp4", part)

	slog.Info("postprocess: transcoding", "id", rec.ID, "src", src)
	child, err := proc.Spawn(ctx, r.FFmpegBin, args...)
	if err != nil {
		os.Remove(part)
		return fmt.Errorf("spawn ffmpeg: %w", err)
	}
	renice(child)

	if err := waitOrStall(ctx, child, part); err != nil {
		child.Close()
		os.Remove(part)
		return err
	}
	if fi, err := os.Stat(part); err != nil || fi.Size() == 0 {
		os.Remove(part)
		return errors.New("ffmpeg wrote nothing")
	}
	if err := os.Rename(part, out); err != nil {
		return err
	}

	if r.DeleteSource {
		if err := os.Remove(src); err != nil {
			slog.Warn("postprocess: cannot remove the source", "id", rec.ID, "err", err)
		}
	}
	return nil
}

// renice drops the transcode below everything else on the box.
//
// It shares a CPU with live HLS, which is soft-realtime: an encode that keeps
// up with the broadcast or the picture stutters. Being wrong here is cheap in
// one direction and expensive in the other, so the transcode always yields.
func renice(child *proc.Process) {
	if err := syscall.Setpriority(syscall.PRIO_PGRP, child.Pgid(), niceness); err != nil {
		slog.Warn("postprocess: cannot lower the transcode's priority", "err", err)
	}
}

// waitOrStall waits for the child, failing it if the output stops growing.
func waitOrStall(ctx context.Context, child *proc.Process, out string) error {
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()

	ticker := time.NewTicker(stallTick)
	defer ticker.Stop()
	var (
		lastSize int64 = -1
		lastGrew       = time.Now()
	)
	for {
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			size := int64(0)
			if fi, err := os.Stat(out); err == nil {
				size = fi.Size()
			}
			if size != lastSize {
				lastSize, lastGrew = size, time.Now()
				continue
			}
			if time.Since(lastGrew) > stallTimeout {
				return fmt.Errorf("stall watchdog: output did not grow for %s", stallTimeout)
			}
		}
	}
}

// audioOffset is the correction the live path measured for this channel.
//
// ISDB muxes interleave audio ahead of the first decodable video frame, and
// the skew is a property of the broadcaster's mux rather than of the moment —
// so the measurement live HLS already paid an ffprobe pass for applies here
// too. Nothing is measured here: a channel that has never been watched or
// recorded live simply gets no correction, which is what it got before.
func (r *Runner) audioOffset(ctx context.Context, channel string) float64 {
	off, ok, err := r.Store.AudioOffsetFor(ctx, channel)
	if err != nil || !ok {
		return 0
	}
	return off.OffsetS + r.AudioOffsetBias
}

func (r *Runner) transcodeArgs() []string {
	if len(r.TranscodeArgs) > 0 {
		return r.TranscodeArgs
	}
	return DefaultTranscodeArgs
}

// sourcePath resolves the row's file and refuses anything outside the storage
// root — the same guard the API applies, for the same reason: the column is a
// filesystem path in a database, and this end of it deletes files.
func (r *Runner) sourcePath(rec store.Recording) (string, error) {
	if rec.Path == "" {
		return "", errors.New("recording has no file")
	}
	p, err := filepath.Abs(rec.Path)
	if err != nil {
		return "", err
	}
	if r.StorageRoot == "" {
		return p, nil
	}
	root, err := filepath.Abs(r.StorageRoot)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("recording path %q is outside the storage root", rec.Path)
	}
	return p, nil
}

// replaceExt swaps a path's extension, which is how every derived file is
// named. Deriving rather than storing means the .mp4 and the sidecars inherit
// the .ts's storage-root check instead of needing three of their own.
func replaceExt(path, ext string) string {
	return strings.TrimSuffix(path, filepath.Ext(path)) + ext
}

// audioOffsetFilter mirrors internal/hls: shift audio by the measured skew.
func audioOffsetFilter(offset float64) string {
	if math.Abs(offset) < 0.02 {
		return ""
	}
	op := "+"
	if offset < 0 {
		op = "-"
	}
	return fmt.Sprintf("asetpts=PTS%s%g/TB", op, math.Abs(offset))
}
