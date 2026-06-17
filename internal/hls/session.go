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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/DuckFeather10086/isdbd/internal/fanout"
	"github.com/DuckFeather10086/isdbd/internal/proc"
	"github.com/DuckFeather10086/isdbd/internal/tuner"
)

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
	FFmpegArgs  []string      // extra args inserted before -i pipe:0
	IdleTimeout time.Duration // 0 → DefaultIdleTimeout

	mu       sync.Mutex
	sessions map[string]*Session
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

	// The playlist is served at /api/live/{channel}.m3u8 but segments
	// are served at /api/live/{channel}/{segment}. Prepend a relative
	// base url so each segment URI in the playlist resolves under the
	// channel subpath (and survives being mounted behind a path prefix).
	segBase := url.PathEscape(canonical) + "/"

	args := append([]string{}, m.FFmpegArgs...)
	args = append(args,
		"-hide_banner", "-loglevel", "warning", "-nostats",
		"-fflags", "+genpts+discardcorrupt",
		"-i", "pipe:0",
		"-c:v", "copy", "-c:a", "aac", "-b:a", "192k",
		"-sn", "-dn",
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

