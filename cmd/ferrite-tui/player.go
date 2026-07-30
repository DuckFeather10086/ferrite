package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

// ErrNoDisplay means there is nowhere to open a video window.
//
// This is the ssh case: the TUI is designed to run on the machine you are
// sitting at, with the daemon elsewhere on the LAN. Run it over ssh instead
// and mpv would try to open a window on the *tuner box* — so when no display
// is present we refuse to spawn and surface the URL for the user to open
// wherever they actually are.
var ErrNoDisplay = errors.New("no display available for a video window")

// ErrPlaybackDisabled means --player none: the user opted out of local
// playback, so a switch should report the stream URL rather than an error.
var ErrPlaybackDisabled = errors.New("playback disabled (--player none)")

// Player runs at most one video player process at a time.
type Player struct {
	// Bin is the player executable ("mpv"). Empty disables playback, in
	// which case the TUI only reports the stream URL.
	Bin string
	// Args are inserted before the URL. Defaults to a low-latency profile.
	Args []string
	// Env is the environment consulted for a display; nil means os.Environ.
	Env []string

	mu      sync.Mutex
	cmd     *exec.Cmd
	channel string
}

// DefaultPlayerArgs keeps mpv close to live. ISDB HLS segments are 2s, so
// a small cache is enough and demuxer-lavf-o helps it start on a partial
// first segment instead of waiting for a clean one.
var DefaultPlayerArgs = []string{
	"--profile=low-latency",
	"--cache=yes",
	"--force-window=immediate",
}

// DefaultFileArgs play a finished recording. Deliberately *not* the
// low-latency profile: that exists to stay near the live edge and shrinks
// the cache mpv wants for seeking, which is the whole point of playing
// back a file.
var DefaultFileArgs = []string{
	"--cache=yes",
	"--force-window=immediate",
}

// HasDisplay reports whether a graphical session is available.
func (p *Player) HasDisplay() bool {
	env := p.Env
	if env == nil {
		env = os.Environ()
	}
	for _, key := range []string{"DISPLAY=", "WAYLAND_DISPLAY=", "MPV_FORCE_PLAYBACK="} {
		for _, kv := range env {
			if len(kv) > len(key) && kv[:len(key)] == key {
				return true
			}
		}
	}
	return false
}

// Playing reports the channel currently on screen, if any.
func (p *Player) Playing() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd == nil {
		return ""
	}
	return p.channel
}

// Play stops any current playback and starts channel from url.
func (p *Player) Play(channel, url string) error {
	return p.play(channel, url, p.argsFor(DefaultPlayerArgs))
}

// PlayFile plays a recording. label is what the UI shows as "playing";
// it is not a channel name, so nothing matches it against the channel
// list.
func (p *Player) PlayFile(label, url string) error {
	return p.play(label, url, p.argsFor(DefaultFileArgs))
}

func (p *Player) play(label, url string, playerArgs []string) error {
	if p.Bin == "" {
		return ErrPlaybackDisabled
	}
	if !p.HasDisplay() {
		return ErrNoDisplay
	}

	p.Stop()

	args := append([]string{}, playerArgs...)
	args = append(args, url)
	cmd := exec.Command(p.Bin, args...)
	// Own process group: killing the group takes any helper mpv spawned
	// with it, so a stale decoder can't keep holding the stream.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", p.Bin, err)
	}
	// Reap it, or a channel change per hour leaves a zombie per change.
	go func() { _ = cmd.Wait() }()

	p.mu.Lock()
	p.cmd, p.channel = cmd, label
	p.mu.Unlock()
	return nil
}

// Stop ends playback. Safe to call when nothing is playing.
func (p *Player) Stop() {
	p.mu.Lock()
	cmd, _ := p.cmd, p.channel
	p.cmd, p.channel = nil, ""
	p.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return
	}
	// Negative pid signals the whole group.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}

// argsFor prefers an explicit Args override; otherwise the profile that
// suits what is being played.
func (p *Player) argsFor(def []string) []string {
	if p.Args != nil {
		return p.Args
	}
	return def
}
