// Package hls serves live HLS for one channel by acquiring a tuner
// Lease, piping fanout chunks into an ffmpeg subprocess, and
// exposing the resulting m3u8 + segments over HTTP.
//
// Sessions are reference-counted by channel: the first viewer (HTTP
// touch) opens the session — Acquire lease → spawn ffmpeg → start
// chunk-pump goroutine. Subsequent touches bump a last-seen
// timestamp. A janitor goroutine closes any session that has not
// been touched for IdleTimeout (default 60s), which releases the
// tuner Lease back to the Pool.
//
// The startup + steady-state watchdog from live_hls.py lives in
// recorder; HLS has its own equivalent: if ffmpeg exits unexpectedly
// the session is torn down on next touch and the caller gets an
// error rather than a stale playlist.
package hls

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DuckFeather10086/ferrite/internal/caption"
	"github.com/DuckFeather10086/ferrite/internal/fanout"
	"github.com/DuckFeather10086/ferrite/internal/proc"
	"github.com/DuckFeather10086/ferrite/internal/store"
	"github.com/DuckFeather10086/ferrite/internal/tuner"
)

// defaultProbeSeconds is the ffprobe sampling window used to measure the
// initial audio/video PTS offset when Manager.ProbeSeconds is unset.
const defaultProbeSeconds = 5.0

// minAudioOffset is the smallest |offset| (seconds) worth correcting;
// below this the skew is imperceptible and not worth forcing the filter.
const minAudioOffset = 0.01

// DefaultIdleTimeout is how long a session stays open without a
// /api/live/{channel}.m3u8 touch before the janitor closes it.
const DefaultIdleTimeout = 60 * time.Second

// The segmentation of the live stream. These three go together and are
// the whole of the latency budget on this side of the wire:
//
//   - segmentSeconds is the floor on how stale the live edge can be —
//     nothing reaches a player until ffmpeg closes the segment it is in.
//   - outputFPS is the frame rate *after* yadif, which turns ISDB-T's
//     30i into 30p. It is here only to compute the GOP.
//   - playlistSegments is how far back a player can reach, in segments.
//     segmentSeconds × playlistSegments ≈ 12s of window.
//
// The GOP handed to the encoder is segmentSeconds × outputFPS, and that
// relationship is a hard constraint rather than a tuning choice: an HLS
// segment must begin with an IDR frame, so a GOP that does not divide
// the segment length makes ffmpeg cut late, at the next keyframe, and
// segments drift away from the duration the playlist advertises.
//
// The length is also what decides how often the browser rebuilds a caption.
// A WebVTT cue has to be cut to the segment window it is published in, or a
// caption spanning a boundary is drawn twice — so a caption spanning N
// segments arrives as N cues with different times, and a player, which dedups
// on start *and end and text*, tears the box down and builds a new one at every
// boundary. That rebuild is visible as a flicker, and its rate is the segment
// count and nothing else: no alignment, no cleverness about where to cut.
// Halving the segment doubles it. So the length trades latency against that,
// and playlistSegments moves the other way to hold the reachable window at ~12s.
const (
	segmentSeconds   = 2
	outputFPS        = 30
	playlistSegments = 6
)

// Acquirer is the tuner.Pool seam (tests pass a fake).
type Acquirer interface {
	Acquire(ctx context.Context, channel string) (*tuner.Lease, error)
}

// OffsetStore persists measured A/V skew per channel. *store.Store
// implements it; tests use a map.
type OffsetStore interface {
	AudioOffsetFor(ctx context.Context, channel string) (store.AudioOffset, bool, error)
	PutAudioOffset(ctx context.Context, channel string, offsetS float64) error
}

// DefaultOffsetMaxAge bounds how long a cached A/V measurement is
// trusted. Long, because the skew comes from the broadcaster's encoder
// and effectively never moves; the cap only exists so an equipment
// change eventually gets picked up on its own.
const DefaultOffsetMaxAge = 30 * 24 * time.Hour

// Manager owns one HLS session per channel and quality.
type Manager struct {
	Tuners      Acquirer
	OutputRoot  string        // sessions write under {OutputRoot}/{channel}/{quality}/
	FFmpegBin   string        // path to ffmpeg
	FFprobeBin  string        // path to ffprobe; empty disables A/V offset probing
	FFmpegArgs  []string      // extra args inserted before -i pipe:0
	IdleTimeout time.Duration // 0 → DefaultIdleTimeout

	// Qualities are the encoding tiers on offer, in the order a UI should
	// show them; the first is the default. Empty means the single
	// DefaultQuality, which is the encode this daemon did before tiers
	// existed. See quality.go for why this is on demand and not ABR.
	Qualities []Quality

	// CaptionBin is the arib-caption executable. When set, each session also
	// decodes the tune's caption PID into the subtitle renditions beside its
	// segments — WebVTT for the player to draw, and the structured form the
	// browser's own ARIB overlay draws. Empty means no captions; the picture
	// is identical either way.
	//
	// It no longer needs FFprobeBin: internal/caption reads a segment's first
	// video PTS itself rather than spawning a probe for it.
	CaptionBin string

	// A/V sync (see config.Daemon). ProbeSeconds is the ffprobe sampling
	// window (0 → defaultProbeSeconds; <0 disables); AudioOffsetBias is
	// added to the measured offset.
	ProbeSeconds    float64
	AudioOffsetBias float64

	// Canonical maps a channel name or alias to the channel's canonical
	// name. Sessions are keyed by the result, so /api/live/mx.m3u8 and
	// /api/live/TOKYO%20MX1.m3u8 address one session instead of two
	// aliases spawning two ffmpegs over the same output dir. Nil uses
	// names verbatim.
	Canonical func(string) string

	// Offsets caches measured A/V skew across tunes. The ffprobe pass is
	// the largest single component of channel-change latency (~5s), and
	// the skew is a property of the broadcaster's mux rather than of the
	// moment — so it is measured once per channel and reused. Nil probes
	// every time (the pre-cache behaviour).
	Offsets OffsetStore

	// OffsetMaxAge re-probes a cached measurement older than this.
	// 0 → DefaultOffsetMaxAge; negative → never expire.
	OffsetMaxAge time.Duration

	mu       sync.Mutex
	sessions map[sessionKey]*Session
	// opening tracks in-flight opens per channel+quality so concurrent
	// viewers (e.g. a player retrying its manifest request mid-tune) join
	// the same tune instead of racing a second Acquire — the loser of that
	// race would overwrite the winner in sessions and orphan a running
	// ffmpeg + lease that the janitor can never reap.
	opening map[sessionKey]*openCall
	// tunes is the per-channel state every quality of that channel shares:
	// the lease, and the caption decode.
	tunes map[string]*channelTune
	// lastOpen records the most recently opened/touched session so the
	// /stream.m3u8 shortcut knows which one to serve.
	lastOpen sessionKey
}

// sessionKey identifies one encode: a channel at a quality. Two tiers of
// one channel are two sessions over one tune, which is the whole point —
// see channelTune.
type sessionKey struct {
	channel string
	quality string
}

func (k sessionKey) String() string { return k.channel + "/" + k.quality }

// openCall is one in-flight session open; done is closed once s/err
// are set.
type openCall struct {
	done chan struct{}
	s    *Session
	err  error
}

// channelTune is what every quality of one channel shares: the tuner
// lease, and the caption decode reading it.
//
// Both are properties of the *broadcast*, not of an encode. One lease
// because two tiers are one claim on the frontend, released when the last
// tier's last viewer leaves; one caption decode because the words are the
// same at every bitrate, and decoding them per tier would mean N
// arib-caption children reading N subscriptions of the same TS to produce
// N copies of the same cues. Each tier still gets its own *rendition* of
// those cues — a player matches subtitle fragments to the video fragments
// it is playing, and each encode numbers its own segments.
type channelTune struct {
	channel string
	lease   *tuner.Lease

	// refs counts the sessions holding this tune. The lease is released
	// when it reaches zero, which is the last viewer of the last tier.
	refs int

	// subTaken marks lease.Sub as handed to a session. The lease comes
	// with one subscription already made; the first session uses it and
	// every session after that takes its own, so nothing has to drain a
	// subscription no one is reading.
	subTaken bool

	// The caption decode, when one is running.
	pipeline  *caption.Pipeline
	capSub    *fanout.Sub
	capCancel context.CancelFunc
}

// key normalizes a requested channel name to its session key.
func (m *Manager) key(channel string) string {
	if m.Canonical == nil {
		return channel
	}
	if c := m.Canonical(channel); c != "" {
		return c
	}
	return channel
}

// cachedOffset returns the stored raw measurement for channel when one
// exists and is fresh enough to trust. Cache trouble is never fatal:
// a miss just means we measure again.
func (m *Manager) cachedOffset(channel string) (float64, bool) {
	if m.Offsets == nil {
		return 0, false
	}
	rec, ok, err := m.Offsets.AudioOffsetFor(context.Background(), channel)
	if err != nil {
		slog.Warn("hls: reading cached A/V offset failed; will re-probe",
			"channel", channel, "err", err)
		return 0, false
	}
	if !ok {
		return 0, false
	}
	maxAge := m.OffsetMaxAge
	if maxAge == 0 {
		maxAge = DefaultOffsetMaxAge
	}
	if maxAge > 0 && rec.Age() > maxAge {
		slog.Info("hls: cached A/V offset is stale; re-probing",
			"channel", channel, "age", rec.Age().Round(time.Hour), "max_age", maxAge)
		return 0, false
	}
	return rec.OffsetS, true
}

func (m *Manager) storeOffset(channel string, rawOffset float64) {
	if m.Offsets == nil {
		return
	}
	if err := m.Offsets.PutAudioOffset(context.Background(), channel, rawOffset); err != nil {
		slog.Warn("hls: caching A/V offset failed",
			"channel", channel, "err", err)
	}
}

func (m *Manager) probeSeconds() float64 {
	if m.ProbeSeconds == 0 {
		return defaultProbeSeconds
	}
	return m.ProbeSeconds
}

// occupancy describes the sessions this manager is holding, for the log line
// that accompanies a failed acquire. The pool knows about recordings and EPG
// too, but a live session on another channel is the case a viewer can act on —
// and the one the switch endpoint exists to clear.
func (m *Manager) occupancy() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sessions) == 0 && len(m.opening) == 0 {
		return "no live sessions (a recording or EPG pass has it)"
	}
	var parts []string
	for key, s := range m.sessions {
		parts = append(parts, fmt.Sprintf("%s(idle %s)", key, s.IdleFor().Round(time.Second)))
	}
	for key := range m.opening {
		parts = append(parts, key.String()+"(opening)")
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

// Session is one live ffmpeg pipeline: one channel at one quality.
type Session struct {
	Channel      string
	Quality      string
	Dir          string // disk dir holding stream.m3u8 + segments
	PlaylistPath string

	// Captions reports that this session decodes the tune's caption PID, so a
	// WebVTT rendition is coming even if it is not on disk yet. The first one
	// is published a tick after ffmpeg's first segment — later than the master
	// playlist is composed, and a player never re-fetches a master. Whoever
	// composes it waits on this rather than on the file alone.
	Captions bool

	mgr      *Manager
	tune     *channelTune
	sub      *fanout.Sub
	ownsSub  bool // sub was taken with Subscribe, so it has to be given back
	ff       *proc.Process
	lastSeen time.Time
	// What sub.Dropped read at the last sweep, so a report is the chunks lost
	// since rather than the running total. Guarded by the Manager's mu.
	dropsSeen uint64
	closed    bool

	// This session's slice of the channel's caption decode, when there is
	// one: its own subs.m3u8, mirroring its own segments.
	rendition *caption.Rendition
}

// key is how the manager indexes this session.
func (s *Session) key() sessionKey {
	return sessionKey{channel: s.Channel, quality: s.Quality}
}

// Open returns the existing session for channel at quality, or starts a
// new one. An empty or unknown quality gets the default tier. Always
// bumps the last-seen timestamp.
func (m *Manager) Open(ctx context.Context, channel, quality string) (*Session, error) {
	if m.Tuners == nil || m.OutputRoot == "" || m.FFmpegBin == "" {
		return nil, errors.New("hls: Tuners/OutputRoot/FFmpegBin required")
	}
	q := m.ResolveQuality(quality)
	key := sessionKey{channel: m.key(channel), quality: q.Name}

	m.mu.Lock()
	if m.sessions == nil {
		m.sessions = make(map[sessionKey]*Session)
	}
	if s, ok := m.sessions[key]; ok && !s.closed {
		s.lastSeen = time.Now()
		m.lastOpen = key
		m.mu.Unlock()
		return s, nil
	}
	if call, ok := m.opening[key]; ok {
		m.mu.Unlock()
		select {
		case <-call.done:
			return call.s, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &openCall{done: make(chan struct{})}
	if m.opening == nil {
		m.opening = make(map[sessionKey]*openCall)
	}
	m.opening[key] = call
	m.mu.Unlock()

	// A cold open runs the frontend lock timeout (~25s) plus the A/V
	// probe — longer than a typical player's manifest timeout. Detach
	// from the request context so an impatient client aborting its
	// request doesn't tear down the tune mid-flight; its retry joins
	// via m.opening, and a fully abandoned session is reaped by the
	// idle janitor.
	s, err := m.openSession(context.WithoutCancel(ctx), key.channel, q)

	m.mu.Lock()
	call.s, call.err = s, err
	delete(m.opening, key)
	m.mu.Unlock()
	close(call.done)
	return s, err
}

// tuneFor returns the channel's shared tune, acquiring the lease if this
// is its first session. The returned tune has already been ref'd; the
// caller must releaseTune on any failure path.
//
// Acquire happens outside m.mu because it can block on the frontend lock,
// so two qualities of one channel opening at once can both find no tune
// and both acquire. The Pool shares the underlying dvbr for a
// same-channel Acquire, so the loser's lease is a second reference to the
// same tune rather than a second tune — it is released immediately and
// the winner's is used.
func (m *Manager) tuneFor(ctx context.Context, channel string) (*channelTune, error) {
	m.mu.Lock()
	if t, ok := m.tunes[channel]; ok {
		t.refs++
		m.mu.Unlock()
		return t, nil
	}
	m.mu.Unlock()

	lease, err := m.Tuners.Acquire(ctx, channel)
	if err != nil {
		// Say who has the adapter. This failure is the one a viewer sees as
		// "cannot play this channel", and until now it reached the browser
		// without leaving a trace in the log — so there was no way to tell a
		// recording from another viewer from a stale player.
		slog.Warn("hls: cannot acquire the adapter",
			"channel", channel, "err", err, "occupancy", m.occupancy())
		return nil, fmt.Errorf("hls: acquire %q: %w", channel, err)
	}

	m.mu.Lock()
	if t, ok := m.tunes[lease.Channel]; ok {
		// Lost the race. Hand the duplicate lease straight back.
		t.refs++
		m.mu.Unlock()
		lease.Release()
		return t, nil
	}
	t := &channelTune{channel: lease.Channel, lease: lease, refs: 1}
	if m.tunes == nil {
		m.tunes = make(map[string]*channelTune)
	}
	m.tunes[lease.Channel] = t
	m.mu.Unlock()
	return t, nil
}

// releaseTune drops one session's reference. The lease — and the caption
// decode on it — go when the last one does, which is the last viewer of
// the last quality.
func (m *Manager) releaseTune(t *channelTune) {
	m.mu.Lock()
	t.refs--
	if t.refs > 0 {
		m.mu.Unlock()
		return
	}
	if m.tunes[t.channel] == t {
		delete(m.tunes, t.channel)
	}
	m.mu.Unlock()

	// Stop the caption decode before releasing the lease: cancelling kills
	// the child, then the subscription goes, then the tune.
	if t.capCancel != nil {
		t.capCancel()
	}
	if t.capSub != nil {
		t.lease.Unsubscribe(t.capSub)
	}
	t.lease.Release()
}

// openSession does the actual acquire → probe → ffmpeg spawn for one
// channel at one quality. Calls are serialized per channel+quality via
// m.opening.
func (m *Manager) openSession(ctx context.Context, channel string, q Quality) (*Session, error) {
	// The lease is the channel's, not this quality's — a second tier joins
	// the tune the first one is already on.
	tune, err := m.tuneFor(ctx, channel)
	if err != nil {
		return nil, err
	}
	canonical := tune.channel

	// A directory per quality, under the channel's. Two encodes of one
	// channel cannot share one: they write the same segment names.
	dir := filepath.Join(m.OutputRoot, canonical, q.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		m.releaseTune(tune)
		return nil, fmt.Errorf("hls: mkdir %s: %w", dir, err)
	}
	playlist := filepath.Join(dir, "stream.m3u8")

	// Clear any leftover segments from a previous run on the same dir. The
	// master playlist used to be written here and is now composed per request
	// by the API; remove a stale one so nothing serves a manifest describing a
	// stream that no longer exists.
	_ = clearStaleSegments(dir)
	_ = os.Remove(filepath.Join(dir, "master.m3u8"))

	// Each session reads the tune through its own subscription. The lease
	// arrives with one already made, so the first tier uses that and the
	// rest take their own — otherwise lease.Sub would sit unread and the
	// broadcaster would keep filling and dropping a queue nobody drains.
	sub, ownsSub := m.subscribe(tune)

	// ISDB-T muxes interleave audio ahead of the first decodable video
	// frame, so HLS otherwise comes up with a constant A/V skew. Shift
	// audio by the difference between the first audio and video PTS
	// (mirrors legacy live_hls.py).
	//
	// Measuring costs an ffprobe pass over the head of the stream — the
	// dominant term in channel-change latency — so a cached measurement
	// is reused when there is one. Only the raw measurement is cached;
	// AudioOffsetBias is applied here so config changes take effect
	// without re-probing.
	//
	// The skew is the broadcaster's, so it is the same at every quality:
	// the second tier of a channel finds the first tier's measurement in
	// the cache and starts without probing.
	audioOffset := 0.0
	if raw, ok := m.cachedOffset(canonical); ok {
		audioOffset = raw + m.AudioOffsetBias
		slog.Info("hls: reusing cached A/V offset",
			"channel", canonical, "offset_s", audioOffset)
	} else if m.FFprobeBin != "" && m.probeSeconds() > 0 {
		// The probe consumes a few seconds of this session's subscription;
		// the main pump picks up where it leaves off. Failure is
		// non-fatal — we just start uncorrected.
		off, perr := probeAudioOffset(context.Background(), m.FFprobeBin, sub, m.probeSeconds())
		if perr != nil {
			slog.Warn("hls: A/V offset probe failed; starting without sync correction",
				"channel", canonical, "err", perr)
		} else {
			audioOffset = off + m.AudioOffsetBias
			slog.Info("hls: measured A/V offset",
				"channel", canonical, "offset_s", audioOffset)
			m.storeOffset(canonical, off)
		}
	}

	// The playlist is served at /api/live/{channel}.m3u8 but segments are
	// served at /api/live/{channel}/{quality}/{segment}. Prepend a relative
	// base url so each segment URI resolves under the quality subpath (and
	// survives being mounted behind a path prefix).
	segBase := url.PathEscape(canonical) + "/" + url.PathEscape(q.Name) + "/"

	args := append([]string{}, m.FFmpegArgs...)
	args = append(args,
		// -loglevel error: ISDB AAC/partial-GOP input makes ffmpeg emit a
		// nonstop stream of benign "warning" lines; at error level only
		// real failures reach proc's stderr→slog (which is also rate-
		// limited). Keeps a long-running session from flooding the log.
		"-hide_banner", "-loglevel", "error", "-nostats",
		"-fflags", "+genpts+discardcorrupt",
		// analyzeduration is in MICROseconds: the old 10M meant ffmpeg
		// spent up to 10 *seconds* in find_stream_info before emitting
		// anything, which measured as ~9.7s of the ~14s channel change
		// (the tune itself is ~2.3s). The service PID tap carries one
		// program with a known shape — mpeg2video + AAC (+ caption) —
		// so a 1s window is plenty to identify it.
		"-probesize", "5M", "-analyzeduration", "1M",
		"-i", "pipe:0",
		// -sn -dn: ffmpeg has no ARIB caption decoder, so the caption and
		// data PIDs are dropped here and decoded separately (internal/caption).
		"-sn", "-dn",
		// Keep the broadcast timestamps on the output. The subtitle rendition
		// times its cues by the caption PTS, and a player reconciles the two by
		// subtracting the video's first PTS — which only works if that PTS is
		// still the broadcast's. Without this the video restarts at zero and
		// every cue lands hours away.
		"-copyts",
		// And -copyts on its own does not keep them: the MPEG-TS muxer inside
		// the HLS muxer adds `muxdelay` to every timestamp it writes, so the
		// segments came out stamped exactly 1.4000s ahead of the broadcast
		// (measured on ffmpeg 6.1.1 by finding a PES's own bytes in the source
		// and reading back the two PTS: +1.4000s on every one of them, 0.0000s
		// with this flag). The picture and the audio move together, so nothing
		// about the stream looks wrong — but the captions do not move with them,
		// being timed off the caption PID's own PTS, and a viewer sees every
		// subtitle a second and a half before the shot it belongs to.
		// `-muxpreload 0` is not part of it; this is the whole of it.
		"-muxdelay", "0",
	)
	if af := audioOffsetFilter(audioOffset); af != "" {
		args = append(args, "-af", af)
	}
	// The tier's own filter chain and encoder (see quality.go), then the
	// GOP, which is not the tier's to choose.
	args = append(args, q.outputArgs()...)
	args = append(args, gopArgs()...)
	args = append(args,
		"-f", "hls",
		// Segment length is the floor on live latency — a player cannot
		// show a segment ffmpeg has not finished writing — so it is the
		// first thing to spend on getting from ~8s down to ~3s. The rest
		// of that budget is the player's: see liveSyncDurationCount in
		// web/src/components/VideoPlayer.tsx.
		//
		// -g is *not* independently tunable: every segment has to start on
		// an IDR frame, so the GOP must divide the segment exactly
		// (segmentSeconds × outputFPS, appended above). Change one and
		// change the other, or ffmpeg cuts at the next keyframe instead
		// and segment durations drift off the advertised #EXTINF.
		"-hls_time", strconv.Itoa(segmentSeconds),
		// Twice the count for half the length: the window a player can
		// seek back into (and recover a dropped segment from) stays the
		// same ~12s it was at 2s × 6.
		"-hls_list_size", strconv.Itoa(playlistSegments),
		"-hls_flags", "delete_segments+omit_endlist",
		"-hls_base_url", segBase,
		playlist,
	)

	ff, err := proc.SpawnOpt(context.Background(),
		proc.SpawnOpts{Stdin: true}, m.FFmpegBin, args...)
	if err != nil {
		m.unsubscribe(tune, sub, ownsSub)
		m.releaseTune(tune)
		return nil, fmt.Errorf("hls: spawn ffmpeg: %w", err)
	}

	s := &Session{
		Channel:      canonical,
		Quality:      q.Name,
		Dir:          dir,
		PlaylistPath: playlist,

		mgr:      m,
		tune:     tune,
		sub:      sub,
		ownsSub:  ownsSub,
		ff:       ff,
		lastSeen: time.Now(),
	}

	go pumpToFFmpeg(sub, ff.Stdin)

	// Captions read the same bytes as the encode, so they take another
	// subscription to the *tune* rather than a second claim on the adapter:
	// one viewer should look like one live claim, and a second Acquire could
	// tune again if this tune had just died. One decode per channel, however
	// many qualities are running — this session only adds a rendition of the
	// cues, cut to its own segments.
	if r := m.attachCaptions(tune, dir, playlist); r != nil {
		s.rendition = r
		s.Captions = true
	}

	m.mu.Lock()
	m.sessions[s.key()] = s
	m.lastOpen = s.key()
	m.mu.Unlock()

	slog.Info("hls: session opened",
		"channel", canonical, "quality", q.Name, "dir", dir, "captions", s.Captions)
	return s, nil
}

// subscribe hands out this session's view of the tune. Reports whether
// the subscription is the session's own (and so has to be returned) or
// the one that came with the lease (which goes with it).
func (m *Manager) subscribe(t *channelTune) (*fanout.Sub, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !t.subTaken {
		t.subTaken = true
		return t.lease.Sub, false
	}
	return t.lease.Subscribe(), true
}

func (m *Manager) unsubscribe(t *channelTune, sub *fanout.Sub, owns bool) {
	if !owns {
		m.mu.Lock()
		t.subTaken = false
		m.mu.Unlock()
		return
	}
	t.lease.Unsubscribe(sub)
}

// attachCaptions gives this session a subtitle rendition, starting the
// channel's decoder if it is not already running. Returns nil when
// captions are not configured.
func (m *Manager) attachCaptions(t *channelTune, dir, playlist string) *caption.Rendition {
	// ffprobe is no longer part of this: internal/caption reads a segment's
	// first video PTS itself, which is what stopped the caption pipeline from
	// making the segmentation irregular. So captions turn on with the decoder
	// alone.
	if m.CaptionBin == "" {
		return nil
	}
	m.mu.Lock()
	first := t.pipeline == nil
	if first {
		t.capSub = t.lease.Subscribe()
		t.pipeline = &caption.Pipeline{
			Bin:     m.CaptionBin,
			Channel: t.channel,
			Sub:     t.capSub,
			// Poll twice per segment. A rendition mirrors its video
			// playlist, so polling slower than ffmpeg writes segments
			// leaves the captions a segment behind the picture.
			Refresh: time.Duration(segmentSeconds) * time.Second / 2,
		}
	}
	pipeline := t.pipeline
	m.mu.Unlock()

	rendition := pipeline.Attach(dir, playlist)
	if first {
		capCtx, cancel := context.WithCancel(context.Background())
		t.capCancel = cancel
		channel := t.channel
		go func() {
			if err := pipeline.Run(capCtx); err != nil && capCtx.Err() == nil {
				slog.Warn("hls: caption pipeline stopped", "channel", channel, "err", err)
			}
		}()
	}
	return rendition
}

// Touch bumps last-seen without opening a new session. Returns nil if no
// session exists for channel at quality.
func (m *Manager) Touch(channel, quality string) *Session {
	key := sessionKey{channel: m.key(channel), quality: m.ResolveQuality(quality).Name}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[key]; ok && !s.closed {
		s.lastSeen = time.Now()
		m.lastOpen = key
		return s
	}
	return nil
}

// LastOpened returns the most recently opened/touched session, or nil if
// none is active. This powers the /stream.m3u8 shortcut for bookmark-based
// playback (VLC, iPad, etc.).
func (m *Manager) LastOpened() *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastOpen == (sessionKey{}) {
		return nil
	}
	s, ok := m.sessions[m.lastOpen]
	if !ok || s.closed {
		return nil
	}
	return s
}

// CloseOthers tears down every session on a channel other than this one,
// returning the channel names it closed (sorted, deduplicated).
//
// This is what makes "change channel" work on a single adapter: two live
// sessions never outrank each other, so the tune holding the frontend has
// to be told to let go before the new one can start. Quality is not part
// of it — every tier of the outgoing channel goes, and every tier of the
// incoming one stays.
func (m *Manager) CloseOthers(channel string) []string {
	keep := m.key(channel)
	m.mu.Lock()
	var others []string
	for key := range m.sessions {
		if key.channel != keep {
			others = append(others, key.channel)
		}
	}
	m.mu.Unlock()

	sort.Strings(others)
	others = dedupe(others)
	for _, ch := range others {
		slog.Info("hls: closing session to free the adapter",
			"channel", ch, "switching_to", keep)
		m.Close(ch)
	}
	return others
}

// Close tears down every session on a channel — all of its qualities, and
// with the last of them the tune. Idempotent.
//
// Whole-channel, because that is what every caller means: stopping live
// playback, or freeing the adapter for a different channel. A viewer
// changing quality is not this; that is opening the other tier and
// letting the janitor reap the one nobody is watching.
func (m *Manager) Close(channel string) {
	canonical := m.key(channel)
	m.mu.Lock()
	var doomed []*Session
	for key, s := range m.sessions {
		if key.channel != canonical {
			continue
		}
		doomed = append(doomed, s)
		delete(m.sessions, key)
		if m.lastOpen == key {
			m.lastOpen = sessionKey{}
		}
	}
	m.mu.Unlock()
	for _, s := range doomed {
		s.tearDown()
	}
}

// closeSession tears down one tier, leaving the channel's other tiers —
// and the tune, if any remain — alone. This is the janitor's handle: a
// quality nobody is watching should go without taking the picture away
// from whoever is watching another one.
func (m *Manager) closeSession(key sessionKey) {
	m.mu.Lock()
	s, ok := m.sessions[key]
	if !ok {
		m.mu.Unlock()
		return
	}
	delete(m.sessions, key)
	if m.lastOpen == key {
		m.lastOpen = sessionKey{}
	}
	m.mu.Unlock()
	s.tearDown()
}

func dedupe(sorted []string) []string {
	out := sorted[:0]
	for i, v := range sorted {
		if i == 0 || sorted[i-1] != v {
			out = append(out, v)
		}
	}
	return out
}

// Run starts the janitor that closes idle sessions. Blocks until ctx
// canceled.
func (m *Manager) Run(ctx context.Context) error {
	idle := m.IdleTimeout
	if idle == 0 {
		idle = DefaultIdleTimeout
	}
	t := time.NewTicker(idle / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.closeAll()
			return nil
		case <-t.C:
			m.reapIdle(idle)
			m.reportDrops()
		}
	}
}

// reportDrops says out loud when the encoder is being fed less than the
// broadcast sent.
//
// fanout's policy is drop, not block — a stuck consumer must never stall the
// tune — so when ffmpeg cannot read as fast as the adapter delivers, chunks are
// discarded *for that subscriber alone* and nothing else notices. What the
// encoder then sees is a transport stream with holes in it: `ac-tex damaged`
// out of mpeg2video, `invalid band type` out of the AAC decoder, and segments
// that come out at 0.37s or 1.74s instead of the 1.001s the playlist advertises,
// because a lost frame moves the keyframe the muxer was going to cut on.
//
// It was invisible until it was looked for: a recording taken off the *same*
// tune at the same time had 307,018 packets and not one continuity break, which
// is what says the aerial and the descrambler are fine and the loss is ours.
// Reported per tier, since a second encode is what tends to push the first one
// over.
func (m *Manager) reportDrops() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, s := range m.sessions {
		if s.sub == nil || s.sub.Dropped == nil {
			continue
		}
		total := s.sub.Dropped.Load()
		if total == s.dropsSeen {
			continue
		}
		lost := total - s.dropsSeen
		s.dropsSeen = total
		slog.Warn("hls: the encoder is not keeping up and broadcast data is being dropped",
			"channel", key.channel, "quality", key.quality,
			"chunks_lost", lost, "chunks_lost_total", total)
	}
}

func (m *Manager) reapIdle(idle time.Duration) {
	now := time.Now()
	var stale []sessionKey
	m.mu.Lock()
	for key, s := range m.sessions {
		if now.Sub(s.lastSeen) > idle {
			stale = append(stale, key)
		}
	}
	m.mu.Unlock()
	// Per tier, not per channel: a viewer who switched from 1080p to 720p
	// leaves the tier they left to go idle, and reaping it must not stop
	// the one they are watching.
	for _, key := range stale {
		slog.Info("hls: closing idle session",
			"channel", key.channel, "quality", key.quality, "idle_for", idle.String())
		m.closeSession(key)
	}
}

func (m *Manager) closeAll() {
	m.mu.Lock()
	keys := make([]sessionKey, 0, len(m.sessions))
	for key := range m.sessions {
		keys = append(keys, key)
	}
	m.mu.Unlock()
	for _, key := range keys {
		m.closeSession(key)
	}
}

func (s *Session) tearDown() {
	if s.closed {
		return
	}
	s.closed = true
	if s.ff != nil {
		_ = s.ff.Close()
	}
	// Stop publishing this tier's subtitles. The decode itself belongs to
	// the channel and outlives this session if another quality is still
	// running; releaseTune stops it when the last one goes.
	if s.rendition != nil && s.tune != nil && s.tune.pipeline != nil {
		s.tune.pipeline.Detach(s.rendition)
	}
	if s.tune != nil {
		s.mgr.unsubscribe(s.tune, s.sub, s.ownsSub)
		s.mgr.releaseTune(s.tune)
	}
}

// LastSeen returns the timestamp of the last Open/Touch — for tests.
func (s *Session) LastSeen() time.Time { return s.lastSeen }

// IdleFor reports how long since the session was last touched.
func (s *Session) IdleFor() time.Duration { return time.Since(s.lastSeen) }

// pumpToFFmpeg drains chunks from the fanout subscription into w
// (typically ffmpeg's stdin). Releases chunks back to the pool as
// it goes. Returns when sub.Ch closes or w errors.
func pumpToFFmpeg(sub *fanout.Sub, w io.WriteCloser) {
	if w == nil {
		// Drain anyway so the broadcaster's sub buffer doesn't fill.
		for c := range sub.Ch {
			c.Release()
		}
		return
	}
	defer w.Close()
	for c := range sub.Ch {
		_, err := w.Write(c.Data)
		c.Release()
		if err != nil {
			// ffmpeg gone or stdin broken — drain remaining and exit.
			for c := range sub.Ch {
				c.Release()
			}
			return
		}
	}
}

// probeAudioOffset measures the initial video−audio PTS offset (seconds)
// by feeding the front of the stream into ffprobe. A positive result
// means audio leads video and should be delayed. Consumes chunks from
// sub until ffprobe has read its sampling window and exits; the caller
// resumes reading sub afterwards.
func probeAudioOffset(ctx context.Context, ffprobeBin string, sub *fanout.Sub, probeSeconds float64) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration((probeSeconds+10)*float64(time.Second)))
	defer cancel()

	args := []string{
		"-v", "quiet",
		"-read_intervals", fmt.Sprintf("%%+%g", probeSeconds),
		"-show_entries", "stream=index,codec_type",
		"-show_entries", "packet=stream_index,pts_time",
		"-of", "json",
		"-i", "pipe:0",
	}
	p, err := proc.SpawnOpt(ctx, proc.SpawnOpts{Stdin: true}, ffprobeBin, args...)
	if err != nil {
		return 0, fmt.Errorf("spawn ffprobe: %w", err)
	}
	defer p.Close()

	// Feed the stream into ffprobe until it has read its interval and
	// exits (closing its stdin → our Write fails). Release every chunk.
	fed := make(chan struct{})
	go func() {
		defer close(fed)
		for c := range sub.Ch {
			_, werr := p.Stdin.Write(c.Data)
			c.Release()
			if werr != nil {
				return
			}
		}
	}()

	out, _ := io.ReadAll(p.Stdout)
	_ = p.Close() // ensure ffprobe is gone before we hand the Sub back
	<-fed         // ensure the feed goroutine stopped reading sub.Ch

	return parseAudioOffsetJSON(out)
}

// parseAudioOffsetJSON pulls the first audio and video packet PTS out of
// ffprobe's JSON and returns video−audio (seconds). Split out for tests.
func parseAudioOffsetJSON(data []byte) (float64, error) {
	var po struct {
		Streams []struct {
			Index     int    `json:"index"`
			CodecType string `json:"codec_type"`
		} `json:"streams"`
		Packets []struct {
			StreamIndex int    `json:"stream_index"`
			PtsTime     string `json:"pts_time"`
		} `json:"packets"`
	}
	if err := json.Unmarshal(data, &po); err != nil {
		return 0, fmt.Errorf("parse ffprobe json: %w", err)
	}
	codecByIdx := make(map[int]string, len(po.Streams))
	for _, s := range po.Streams {
		codecByIdx[s.Index] = s.CodecType
	}
	var vPTS, aPTS float64
	haveV, haveA := false, false
	for _, pk := range po.Packets {
		if pk.PtsTime == "" {
			continue
		}
		t, err := strconv.ParseFloat(pk.PtsTime, 64)
		if err != nil {
			continue
		}
		switch codecByIdx[pk.StreamIndex] {
		case "video":
			if !haveV {
				vPTS, haveV = t, true
			}
		case "audio":
			if !haveA {
				aPTS, haveA = t, true
			}
		}
		if haveV && haveA {
			break
		}
	}
	if !haveV || !haveA {
		return 0, fmt.Errorf("could not detect both first audio and video PTS")
	}
	return vPTS - aPTS, nil
}

// audioOffsetFilter builds the ffmpeg -af value that shifts audio PTS by
// offset seconds (positive delays audio). Returns "" for a negligible
// offset. Mirrors live_hls.py's asetpts expression.
func audioOffsetFilter(offset float64) string {
	if math.Abs(offset) < minAudioOffset {
		return ""
	}
	op := "+"
	if offset < 0 {
		op = "-"
	}
	return fmt.Sprintf("asetpts=PTS%s%g/TB", op, math.Abs(offset))
}

// clearStaleSegments removes any leftover stream.m3u8 / .ts files in dir from a
// previous run, and the subtitle rendition beside them. Mirrors live_hls.py's
// cleanup_hls.
//
// The rendition has to go too: its cue times and media sequence belong to the
// dead run, and serving it to this one would put captions minutes out of step
// with the picture for the second or so before the first publish overwrites it.
// Its absence is what the master playlist waits on, so nothing announces it
// early either.
//
// Pointing hls_root at a tmpfs does not make this redundant, it only narrows
// what it is for. A reboot now starts from an empty directory, so this no
// longer has to clean up after a crash — but the leftovers it exists to remove
// are mostly from *this* boot: the same channel's directory is reused every
// time a session on it is opened, and the previous session's segments are
// still sitting in it.
func clearStaleSegments(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := filepath.Ext(name)
		if name == "stream.m3u8" ||
			(len(name) > 6 && name[:6] == "stream" && ext == ".ts") ||
			name == caption.SubsPlaylist ||
			(strings.HasPrefix(name, "sub") && (ext == ".vtt" || ext == ".json")) {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
	return nil
}

// AdapterHint is exported for /api/status display.
func (s *Session) AdapterHint() int {
	if s.tune == nil || s.tune.lease == nil {
		return -1
	}
	return s.tune.lease.Adapter
}
