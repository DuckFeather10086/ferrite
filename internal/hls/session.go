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
	"strconv"
	"sync"
	"time"

	"github.com/DuckFeather10086/isdbd/internal/fanout"
	"github.com/DuckFeather10086/isdbd/internal/proc"
	"github.com/DuckFeather10086/isdbd/internal/tuner"
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

	mu       sync.Mutex
	sessions map[string]*Session
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

	m.mu.Lock()
	if m.sessions == nil {
		m.sessions = make(map[string]*Session)
	}
	if s, ok := m.sessions[channel]; ok && !s.closed {
		s.lastSeen = time.Now()
		m.mu.Unlock()
		return s, nil
	}
	m.mu.Unlock()

	// New session. Acquire outside the lock — Pool.Acquire can block
	// on frontend lock.
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
	// frame, so HLS otherwise comes up with a constant A/V skew. Sample
	// the first audio + video PTS off the front of the stream and shift
	// audio by their difference (mirrors legacy live_hls.py). The probe
	// consumes a few seconds of lease.Sub; the main pump picks up where
	// it leaves off. Failure is non-fatal — we just start uncorrected.
	audioOffset := 0.0
	if m.FFprobeBin != "" && m.probeSeconds() > 0 {
		off, perr := probeAudioOffset(context.Background(), m.FFprobeBin, lease.Sub, m.probeSeconds())
		if perr != nil {
			slog.Warn("hls: A/V offset probe failed; starting without sync correction",
				"channel", canonical, "err", perr)
		} else {
			audioOffset = off + m.AudioOffsetBias
			slog.Info("hls: measured A/V offset",
				"channel", canonical, "offset_s", audioOffset)
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
		"-probesize", "10M", "-analyzeduration", "10M",
		"-i", "pipe:0",
		"-sn", "-dn",
	)
	if af := audioOffsetFilter(audioOffset); af != "" {
		args = append(args, "-af", af)
	}
	args = append(args,
		"-c:v", "copy", "-c:a", "aac", "-b:a", "192k",
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
	m.mu.Unlock()

	slog.Info("hls: session opened", "channel", canonical, "dir", dir)
	return s, nil
}

// Touch bumps last-seen without opening a new session. Returns nil
// if no session exists for channel.
func (m *Manager) Touch(channel string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[channel]; ok && !s.closed {
		s.lastSeen = time.Now()
		return s
	}
	return nil
}

// Close tears down a specific session. Idempotent.
func (m *Manager) Close(channel string) {
	m.mu.Lock()
	s, ok := m.sessions[channel]
	if !ok {
		m.mu.Unlock()
		return
	}
	delete(m.sessions, channel)
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
