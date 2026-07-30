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
	"sync"
	"time"

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

// Manager owns one HLS session per channel.
type Manager struct {
	Tuners      Acquirer
	OutputRoot  string        // sessions write under {OutputRoot}/{channel}/
	FFmpegBin   string        // path to ffmpeg
	FFprobeBin  string        // path to ffprobe; empty disables A/V offset probing
	FFmpegArgs  []string      // extra args inserted before -i pipe:0
	IdleTimeout time.Duration // 0 → DefaultIdleTimeout

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
	sessions map[string]*Session
	// opening tracks in-flight opens per channel so concurrent viewers
	// (e.g. a player retrying its manifest request mid-tune) join the
	// same tune instead of racing a second Acquire — the loser of that
	// race would overwrite the winner in sessions and orphan a running
	// ffmpeg + lease that the janitor can never reap.
	opening map[string]*openCall
	// lastOpen records the most recently opened/touched channel so the
	// /stream.m3u8 shortcut knows which session to serve.
	lastOpen string
}

// openCall is one in-flight session open; done is closed once s/err
// are set.
type openCall struct {
	done chan struct{}
	s    *Session
	err  error
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

// Session is one live ffmpeg pipeline for one channel.
type Session struct {
	Channel      string
	Dir          string // disk dir holding stream.m3u8 + segments
	PlaylistPath string

	mgr      *Manager
	lease    *tuner.Lease
	ff       *proc.Process
	lastSeen time.Time
	closed   bool
}

// Open returns the existing session for channel, or starts a new one.
// Always bumps the last-seen timestamp.
func (m *Manager) Open(ctx context.Context, channel string) (*Session, error) {
	if m.Tuners == nil || m.OutputRoot == "" || m.FFmpegBin == "" {
		return nil, errors.New("hls: Tuners/OutputRoot/FFmpegBin required")
	}
	channel = m.key(channel)

	m.mu.Lock()
	if m.sessions == nil {
		m.sessions = make(map[string]*Session)
	}
	if s, ok := m.sessions[channel]; ok && !s.closed {
		s.lastSeen = time.Now()
		m.lastOpen = channel
		m.mu.Unlock()
		return s, nil
	}
	if call, ok := m.opening[channel]; ok {
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
		m.opening = make(map[string]*openCall)
	}
	m.opening[channel] = call
	m.mu.Unlock()

	// A cold open runs the frontend lock timeout (~25s) plus the A/V
	// probe — longer than a typical player's manifest timeout. Detach
	// from the request context so an impatient client aborting its
	// request doesn't tear down the tune mid-flight; its retry joins
	// via m.opening, and a fully abandoned session is reaped by the
	// idle janitor.
	s, err := m.openSession(context.WithoutCancel(ctx), channel)

	m.mu.Lock()
	call.s, call.err = s, err
	delete(m.opening, channel)
	m.mu.Unlock()
	close(call.done)
	return s, err
}

// openSession does the actual acquire → probe → ffmpeg spawn. Calls
// are serialized per channel via m.opening.
func (m *Manager) openSession(ctx context.Context, channel string) (*Session, error) {
	// Acquire outside the manager lock — Pool.Acquire can block on
	// frontend lock.
	lease, err := m.Tuners.Acquire(ctx, channel)
	if err != nil {
		return nil, fmt.Errorf("hls: acquire %q: %w", channel, err)
	}
	canonical := lease.Channel

	dir := filepath.Join(m.OutputRoot, canonical)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		lease.Release()
		return nil, fmt.Errorf("hls: mkdir %s: %w", dir, err)
	}
	playlist := filepath.Join(dir, "stream.m3u8")

	// Clear any leftover segments from a previous run on the same dir.
	_ = clearStaleSegments(dir)

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
	audioOffset := 0.0
	if raw, ok := m.cachedOffset(canonical); ok {
		audioOffset = raw + m.AudioOffsetBias
		slog.Info("hls: reusing cached A/V offset",
			"channel", canonical, "offset_s", audioOffset)
	} else if m.FFprobeBin != "" && m.probeSeconds() > 0 {
		// The probe consumes a few seconds of lease.Sub; the main pump
		// picks up where it leaves off. Failure is non-fatal — we just
		// start uncorrected.
		off, perr := probeAudioOffset(context.Background(), m.FFprobeBin, lease.Sub, m.probeSeconds())
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

	// The playlist is served at /api/live/{channel}.m3u8 but segments
	// are served at /api/live/{channel}/{segment}. Prepend a relative
	// base url so each segment URI in the playlist resolves under the
	// channel subpath (and survives being mounted behind a path prefix).
	segBase := url.PathEscape(canonical) + "/"

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
		"-sn", "-dn",
	)
	if af := audioOffsetFilter(audioOffset); af != "" {
		args = append(args, "-af", af)
	}
	args = append(args,
		// ISDB-T video is MPEG-2 1080i — browsers have no MPEG-2
		// decoder, so `-c:v copy` produces a stream hls.js loads but
		// can never render (audio-less black frame, videoWidth 0).
		// Transcode to H.264: yadif deinterlaces the 30i source to
		// 30p, superfast+zerolatency runs ~2.5× realtime on an Intel
		// N100, and -g 60 keys every ~2s so segments (hls_time 2)
		// each start on an IDR frame.
		//
		// ISDB-T HD is coded 1440x1080 with *non-square* pixels
		// (SAR 4:3 → DAR 16:9). Passing that through relies on the
		// player honouring the SAR in the H.264 VUI, and hls.js/MSE
		// does not do so reliably — the picture comes out horizontally
		// squished. Normalize to square pixels instead, which yields
		// the standard 1920x1080 for HD and leaves an already-square
		// 1920x1080 or a 4:3 SD subchannel geometrically correct:
		//   scale width by SAR (rounded to even), keep height, SAR 1:1.
		// Deinterlace first — scaling interlaced fields would smear them.
		"-vf", "yadif=0,scale=trunc(iw*sar/2)*2:ih,setsar=1",
		"-c:v", "libx264", "-preset", "superfast", "-tune", "zerolatency",
		"-b:v", "6M", "-maxrate", "7M", "-bufsize", "12M",
		"-g", "60", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "192k",
		"-f", "hls",
		"-hls_time", "2",
		"-hls_list_size", "6",
		"-hls_flags", "delete_segments+omit_endlist",
		"-hls_base_url", segBase,
		playlist,
	)

	ff, err := proc.SpawnOpt(context.Background(),
		proc.SpawnOpts{Stdin: true}, m.FFmpegBin, args...)
	if err != nil {
		lease.Release()
		return nil, fmt.Errorf("hls: spawn ffmpeg: %w", err)
	}

	s := &Session{
		Channel:      canonical,
		Dir:          dir,
		PlaylistPath: playlist,

		mgr:      m,
		lease:    lease,
		ff:       ff,
		lastSeen: time.Now(),
	}

	go pumpToFFmpeg(lease.Sub, ff.Stdin)

	m.mu.Lock()
	m.sessions[canonical] = s
	m.lastOpen = canonical
	m.mu.Unlock()

	slog.Info("hls: session opened", "channel", canonical, "dir", dir)
	return s, nil
}

// Touch bumps last-seen without opening a new session. Returns nil
// if no session exists for channel.
func (m *Manager) Touch(channel string) *Session {
	channel = m.key(channel)
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[channel]; ok && !s.closed {
		s.lastSeen = time.Now()
		m.lastOpen = channel
		return s
	}
	return nil
}

// LastOpened returns the session for the most recently opened/touched
// channel, or nil if no session is active. This powers the /stream.m3u8
// shortcut for bookmark-based playback (VLC, iPad, etc.).
func (m *Manager) LastOpened() *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastOpen == "" {
		return nil
	}
	s, ok := m.sessions[m.lastOpen]
	if !ok || s.closed {
		return nil
	}
	return s
}

// CloseOthers tears down every session except channel, returning the
// channels it closed (canonical keys, sorted).
//
// This is what makes "change channel" work on a single adapter: two
// live sessions never outrank each other, so the tune holding the
// frontend has to be told to let go before the new one can start.
func (m *Manager) CloseOthers(channel string) []string {
	keep := m.key(channel)
	m.mu.Lock()
	var others []string
	for ch := range m.sessions {
		if ch != keep {
			others = append(others, ch)
		}
	}
	m.mu.Unlock()

	sort.Strings(others)
	for _, ch := range others {
		slog.Info("hls: closing session to free the adapter",
			"channel", ch, "switching_to", keep)
		m.Close(ch)
	}
	return others
}

// Close tears down a specific session. Idempotent.
func (m *Manager) Close(channel string) {
	channel = m.key(channel)
	m.mu.Lock()
	s, ok := m.sessions[channel]
	if !ok {
		m.mu.Unlock()
		return
	}
	delete(m.sessions, channel)
	if m.lastOpen == channel {
		m.lastOpen = ""
	}
	m.mu.Unlock()
	s.tearDown()
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
		}
	}
}

func (m *Manager) reapIdle(idle time.Duration) {
	now := time.Now()
	var stale []string
	m.mu.Lock()
	for ch, s := range m.sessions {
		if now.Sub(s.lastSeen) > idle {
			stale = append(stale, ch)
		}
	}
	m.mu.Unlock()
	for _, ch := range stale {
		slog.Info("hls: closing idle session", "channel", ch, "idle_for", idle.String())
		m.Close(ch)
	}
}

func (m *Manager) closeAll() {
	m.mu.Lock()
	chans := make([]string, 0, len(m.sessions))
	for ch := range m.sessions {
		chans = append(chans, ch)
	}
	m.mu.Unlock()
	for _, ch := range chans {
		m.Close(ch)
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
	if s.lease != nil {
		s.lease.Release()
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

// clearStaleSegments removes any leftover stream.m3u8 / .ts files
// in dir from a previous run. Mirrors live_hls.py's cleanup_hls.
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
		if name == "stream.m3u8" ||
			(len(name) > 6 && name[:6] == "stream" && filepath.Ext(name) == ".ts") {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
	return nil
}

// AdapterHint is exported for /api/status display.
func (s *Session) AdapterHint() int {
	if s.lease == nil {
		return -1
	}
	return s.lease.Adapter
}
