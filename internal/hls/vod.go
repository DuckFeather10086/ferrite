package hls

// Live quality tiers, with a file where the tuner was.
//
// A recording is served as its post-pass MP4, which is 1080p at ~6 Mbit/s —
// the right answer over a LAN and the wrong one on a phone, over Tailscale,
// or on anything metered. So the same choice the Live page offers is offered
// here, and it is the same mechanism: nothing is encoded until a viewer asks
// for a tier, one ffmpeg per (recording, tier) serves everyone watching it,
// and the tier table is the one `[live.quality.*]` in the config.
//
// Three things differ from live, all of them because the source is a file
// that already exists rather than a broadcast arriving in real time:
//
//   - The playlist is EXT-X-PLAYLIST-TYPE:EVENT, not a rolling window. A
//     recording is something a viewer seeks around in, so segments are kept
//     and the playlist only grows; ffmpeg writes the ENDLIST when the encode
//     reaches the end of the file.
//   - The encode runs as fast as the box can go rather than at 1×, so it
//     overtakes the viewer almost immediately: measured 8.2× realtime for
//     480p from a 1080i MPEG-2 .ts on this N100's iGPU (10.4× from the MP4).
//     A one-hour recording is fully seekable about seven minutes in, and
//     playable from the first second. The only thing a viewer can ask for
//     and not get is a seek *ahead* of the encoder in those first minutes.
//   - There is no lease, no adapter, and no caption decode. A recording's
//     subtitles were written beside it by the post-pass and are served as
//     they always were, so a tier changes the picture and nothing else.
//
// The output is a cache and is treated as one: a session that nobody has
// asked anything of for VODIdleTimeout is killed and its directory removed,
// because half a gigabyte per recording per tier is not something to leave
// lying around for an encode that takes minutes to reproduce.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/DuckFeather10086/ferrite/internal/proc"
)

// DefaultVODIdleTimeout is how long a recording's encode survives without a
// request for its playlist or one of its segments.
//
// Much longer than the live one (60s), and the difference is what a viewer
// does with each. A live session is touched by a player polling a playlist
// that never ends; a VOD session is touched only while the player is
// *fetching* — and once hls.js has its forward buffer, or the viewer pauses,
// nothing is requested at all. Reaping that as idle would delete the encode
// out from under a paused programme, and getting it back means starting the
// transcode again from the beginning.
const DefaultVODIdleTimeout = 10 * time.Minute

// vodNiceness is where a recording's transcode sits relative to everything
// else. Same reasoning as the post-pass's: this shares a box with live HLS,
// which is soft-realtime and has no headroom to give, while this one is
// already running several times faster than it is being watched. Yielding
// costs it nothing that a viewer can see.
const vodNiceness = 10

// VOD runs one on-demand transcode per (recording, quality).
type VOD struct {
	// OutputRoot holds {OutputRoot}/{id}/{quality}/. Must be a directory of
	// its own — Run empties it at startup — and on real storage rather than
	// the tmpfs the live segments use: a full-length tier is hundreds of
	// megabytes, which is fine on a disk and is not fine in RAM.
	OutputRoot string
	FFmpegBin  string
	// FFmpegArgs go before -i, and are the same list live uses: on this box
	// the VAAPI decode setup, which is a property of the machine rather than
	// of a tier.
	FFmpegArgs []string
	// Qualities is the tier table, shared with live. Empty means the one
	// built-in tier, which for a recording is the same encode the post-pass
	// already did — see QualityList, which is what a UI should offer.
	Qualities []Quality
	// IdleTimeout: 0 → DefaultVODIdleTimeout.
	IdleTimeout time.Duration

	// Offsets is the same A/V skew cache live and the post-pass read. It
	// applies to a source .ts only; the MP4 has the correction baked in.
	Offsets         OffsetStore
	AudioOffsetBias float64

	mu       sync.Mutex
	sessions map[vodKey]*vodSession
	opening  map[vodKey]*vodOpen
}

// vodKey identifies one encode: a recording at a quality.
type vodKey struct {
	id      int64
	quality string
}

func (k vodKey) String() string { return strconv.FormatInt(k.id, 10) + "/" + k.quality }

type vodOpen struct {
	done chan struct{}
	s    *vodSession
	err  error
}

// vodSession is one running (or finished) transcode.
type vodSession struct {
	ID           int64
	Quality      string
	Dir          string
	PlaylistPath string

	ff       *proc.Process
	lastSeen time.Time
	// closed is atomic because the goroutine watching ffmpeg reads it while
	// a teardown on another goroutine is setting it — and the Swap is what
	// makes tearDown idempotent.
	closed atomic.Bool
}

// VODSession is what a caller needs to serve this encode's files.
type VODSession struct {
	ID           int64
	Quality      string
	Dir          string
	PlaylistPath string
}

func (s *vodSession) public() *VODSession {
	return &VODSession{ID: s.ID, Quality: s.Quality, Dir: s.Dir, PlaylistPath: s.PlaylistPath}
}

// QualityList reports the tiers a recording can be re-encoded to, in config
// order. Note what is *not* here: the recording's own file, which is a
// separate offer a UI makes ("Source") and needs nothing from this package —
// it is served straight off the disk, seeks anywhere, and costs no encode.
func (v *VOD) QualityList() []QualityInfo {
	return qualityInfos(v.Qualities)
}

// ResolveQuality maps a requested tier name to a configured one, falling back
// to the first — the same rule live uses, and for the same reason: a stale
// bookmark should get a picture rather than a 404.
func (v *VOD) ResolveQuality(name string) Quality {
	return resolveQuality(v.Qualities, name)
}

// Open starts (or joins) the encode of recording id at quality, and bumps the
// session's last-seen either way.
//
// recPath is the recording's own file, already resolved and checked against
// the storage root by the caller — this package never reads a path out of a
// database. channel names whose A/V skew to correct.
func (v *VOD) Open(ctx context.Context, id int64, recPath, channel, quality string) (*VODSession, error) {
	if v.OutputRoot == "" || v.FFmpegBin == "" {
		return nil, errors.New("hls: vod OutputRoot/FFmpegBin required")
	}
	q := v.ResolveQuality(quality)
	key := vodKey{id: id, quality: q.Name}

	v.mu.Lock()
	if v.sessions == nil {
		v.sessions = make(map[vodKey]*vodSession)
	}
	if s, ok := v.sessions[key]; ok && !s.closed.Load() {
		s.lastSeen = time.Now()
		v.mu.Unlock()
		return s.public(), nil
	}
	// Joining an open in flight rather than racing it: hls.js asks for the
	// playlist again while the first request is still waiting for ffmpeg to
	// write it, and two encodes over one directory write the same segment
	// names.
	if call, ok := v.opening[key]; ok {
		v.mu.Unlock()
		select {
		case <-call.done:
			if call.err != nil {
				return nil, call.err
			}
			return call.s.public(), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &vodOpen{done: make(chan struct{})}
	if v.opening == nil {
		v.opening = make(map[vodKey]*vodOpen)
	}
	v.opening[key] = call
	v.mu.Unlock()

	// Detached from the request: an impatient player aborting its first
	// playlist request must not kill the encode its retry is about to join.
	s, err := v.openSession(context.WithoutCancel(ctx), id, recPath, channel, q)

	v.mu.Lock()
	call.s, call.err = s, err
	delete(v.opening, key)
	if err == nil {
		v.sessions[key] = s
	}
	v.mu.Unlock()
	close(call.done)
	if err != nil {
		return nil, err
	}
	return s.public(), nil
}

func (v *VOD) openSession(ctx context.Context, id int64, recPath, channel string, q Quality) (*vodSession, error) {
	src, corrected, err := vodSource(recPath)
	if err != nil {
		return nil, err
	}

	// Start from an empty directory: whatever an earlier session left is a
	// playlist listing segments *this* encode has not written yet, which a
	// player would fetch and then wait on forever.
	dir := filepath.Join(v.OutputRoot, strconv.FormatInt(id, 10), q.Name)
	if err := os.RemoveAll(dir); err != nil {
		return nil, fmt.Errorf("hls: clear %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("hls: mkdir %s: %w", dir, err)
	}
	playlist := filepath.Join(dir, VODPlaylist)

	args := []string{"-hide_banner", "-loglevel", "error", "-nostats",
		// The head of a recording is mid-GOP by construction — the recorder
		// starts writing when the lease delivers, not on an IDR — which is
		// the same reason the post-pass sets these.
		"-fflags", "+genpts+discardcorrupt"}
	args = append(args, v.FFmpegArgs...)
	args = append(args, "-i", src,
		// One video and one audio stream, and no caption PID: ffmpeg has no
		// ARIB caption decoder and refuses the whole output if one reaches
		// the muxer. The subtitles are already on disk beside the recording.
		"-map", "0:v:0", "-map", "0:a:0", "-sn", "-dn")
	if !corrected {
		if af := audioOffsetFilter(v.audioOffset(channel)); af != "" {
			args = append(args, "-af", af)
		}
	}
	// The tier's own filter chain and encoder, then the GOP — which is not
	// the tier's to choose here either, for the same reason: every segment
	// has to begin on an IDR frame.
	args = append(args, q.outputArgs()...)
	args = append(args, gopArgs()...)
	args = append(args,
		"-f", "hls",
		"-hls_time", strconv.Itoa(segmentSeconds),
		// EVENT is the difference from live. The playlist grows and nothing
		// is deleted from it, so a viewer can seek back to the beginning of
		// the programme; ffmpeg appends the ENDLIST when it reaches the end
		// of the file, and the player stops polling. (`event` also implies
		// hls_list_size 0 — the two cannot disagree.)
		"-hls_playlist_type", "event",
		playlist,
	)

	ff, err := proc.Spawn(ctx, v.FFmpegBin, args...)
	if err != nil {
		return nil, fmt.Errorf("hls: spawn ffmpeg: %w", err)
	}
	if err := syscall.Setpriority(syscall.PRIO_PGRP, ff.Pgid(), vodNiceness); err != nil {
		slog.Warn("hls: cannot lower the recording transcode's priority", "id", id, "err", err)
	}

	s := &vodSession{
		ID:           id,
		Quality:      q.Name,
		Dir:          dir,
		PlaylistPath: playlist,
		ff:           ff,
		lastSeen:     time.Now(),
	}
	go watchVODEncode(s, q.Name)

	slog.Info("hls: recording transcode started",
		"id", id, "quality", q.Name, "src", filepath.Base(src), "dir", dir)
	return s, nil
}

// watchVODEncode closes off the playlist of an encode that died.
//
// ffmpeg writes the ENDLIST itself when it reaches the end of the recording,
// and that is what tells a player the programme is over. A transcode that
// fails partway — a truncated .ts, a GPU that went away — writes no such
// thing, and what the player has is a playlist that has simply stopped
// growing, which is indistinguishable from an encoder that is being slow:
// it polls, forever, at the point where the picture stopped. Ending it there
// is at least the truth, and it is what the viewer already has.
func watchVODEncode(s *vodSession, quality string) {
	err := s.ff.Wait()
	// A teardown kills the child, so its non-zero exit is ours and the
	// directory is on its way out anyway.
	if err == nil || s.closed.Load() {
		return
	}
	slog.Warn("hls: a recording transcode failed; ending its playlist where it stopped",
		"id", s.ID, "quality", quality, "err", err)
	f, ferr := os.OpenFile(s.PlaylistPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if ferr != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString("#EXT-X-ENDLIST\n")
}

// VODPlaylist is the media playlist ffmpeg writes, and the name the segments
// are derived from (video0.ts, video1.ts, …). It is also the last path
// component a client asks for, which is what lets the segment URIs stay
// relative: they resolve inside the tier's own directory.
const VODPlaylist = "video.m3u8"

// vodSource picks what to encode, and reports whether that file already has
// the A/V correction in it.
//
// The recording's own .ts is preferred: it is what the tier filter chains are
// written for — ISDB-T's interlaced MPEG-2, which is exactly what live feeds
// them — and it is there whether or not the post-pass has run. When it has
// been removed (transcode.delete_source), the MP4 stands in; that one is
// progressive and already A/V-corrected, so it must not be corrected twice.
func vodSource(recPath string) (path string, corrected bool, err error) {
	if recPath == "" {
		return "", false, errors.New("hls: recording has no file")
	}
	if _, err := os.Stat(recPath); err == nil {
		return recPath, false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", false, err
	}
	mp4 := strings.TrimSuffix(recPath, filepath.Ext(recPath)) + ".mp4"
	if _, err := os.Stat(mp4); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, fmt.Errorf("hls: neither %s nor %s is on disk",
				filepath.Base(recPath), filepath.Base(mp4))
		}
		return "", false, err
	}
	return mp4, true, nil
}

// audioOffset is the cached measurement for this channel, or none. Nothing is
// measured here: an unwatched channel simply gets no correction, which is what
// the post-pass does with the same question.
func (v *VOD) audioOffset(channel string) float64 {
	if v.Offsets == nil || channel == "" {
		return 0
	}
	rec, ok, err := v.Offsets.AudioOffsetFor(context.Background(), channel)
	if err != nil || !ok {
		return 0
	}
	return rec.OffsetS + v.AudioOffsetBias
}

// Touch bumps last-seen without starting anything. Returns nil when no
// session is running, which is what a segment request for a reaped encode
// gets — a 404 the player recovers from by reloading the playlist.
func (v *VOD) Touch(id int64, quality string) *VODSession {
	key := vodKey{id: id, quality: v.ResolveQuality(quality).Name}
	v.mu.Lock()
	defer v.mu.Unlock()
	if s, ok := v.sessions[key]; ok && !s.closed.Load() {
		s.lastSeen = time.Now()
		return s.public()
	}
	return nil
}

// Close tears down every tier of one recording and removes its directory.
// Idempotent, and the handle DELETE needs: a recording being deleted must not
// leave an encode of it running.
func (v *VOD) Close(id int64) {
	v.mu.Lock()
	var doomed []*vodSession
	for key, s := range v.sessions {
		if key.id != id {
			continue
		}
		doomed = append(doomed, s)
		delete(v.sessions, key)
	}
	v.mu.Unlock()
	for _, s := range doomed {
		s.tearDown()
	}
	if v.OutputRoot != "" {
		_ = os.RemoveAll(filepath.Join(v.OutputRoot, strconv.FormatInt(id, 10)))
	}
}

// Run empties the output root and then reaps idle sessions until ctx is
// canceled.
//
// Emptying at startup is not tidiness: a session's ffmpeg does not outlive
// the daemon, so anything already there is the debris of a crash — a
// half-written playlist whose segments will never arrive, which a player
// would sit and wait for.
func (v *VOD) Run(ctx context.Context) error {
	if v.OutputRoot != "" {
		if err := os.RemoveAll(v.OutputRoot); err != nil {
			slog.Warn("hls: cannot clear the recording transcode root",
				"dir", v.OutputRoot, "err", err)
		}
	}
	idle := v.IdleTimeout
	if idle == 0 {
		idle = DefaultVODIdleTimeout
	}
	t := time.NewTicker(idle / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			v.closeAll()
			return nil
		case <-t.C:
			v.reapIdle(idle)
		}
	}
}

func (v *VOD) reapIdle(idle time.Duration) {
	now := time.Now()
	var stale []vodKey
	v.mu.Lock()
	for key, s := range v.sessions {
		if now.Sub(s.lastSeen) > idle {
			stale = append(stale, key)
		}
	}
	v.mu.Unlock()
	for _, key := range stale {
		slog.Info("hls: closing an idle recording transcode",
			"id", key.id, "quality", key.quality, "idle_for", idle.String())
		v.closeSession(key)
	}
}

func (v *VOD) closeSession(key vodKey) {
	v.mu.Lock()
	s, ok := v.sessions[key]
	if ok {
		delete(v.sessions, key)
	}
	v.mu.Unlock()
	if ok {
		s.tearDown()
	}
}

func (v *VOD) closeAll() {
	v.mu.Lock()
	all := make([]*vodSession, 0, len(v.sessions))
	for key, s := range v.sessions {
		all = append(all, s)
		delete(v.sessions, key)
	}
	v.mu.Unlock()
	for _, s := range all {
		s.tearDown()
	}
}

func (s *vodSession) tearDown() {
	if s.closed.Swap(true) {
		return
	}
	if s.ff != nil {
		_ = s.ff.Close()
	}
	// The segments go with it. They are a cache of something reproducible,
	// and a full tier of a feature film is most of a gigabyte.
	if s.Dir != "" {
		_ = os.RemoveAll(s.Dir)
		// And the recording's directory above it, which fails harmlessly
		// while another tier of the same recording is still in there.
		_ = os.Remove(filepath.Dir(s.Dir))
	}
}
