package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHasDisplay(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		want bool
	}{
		{"x11", []string{"DISPLAY=:0"}, true},
		{"wayland", []string{"WAYLAND_DISPLAY=wayland-0"}, true},
		{"override", []string{"MPV_FORCE_PLAYBACK=1"}, true},
		{"headless", []string{"TERM=xterm", "SSH_TTY=/dev/pts/0"}, false},
		{"empty value is not a display", []string{"DISPLAY="}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Player{Env: tc.env}
			if got := p.HasDisplay(); got != tc.want {
				t.Fatalf("HasDisplay() = %v, want %v", got, tc.want)
			}
		})
	}
}

// Over ssh there is no local display, and spawning would open a window on
// the wrong machine. Play must decline in a way the caller can recognize.
func TestPlayWithoutDisplayIsRecognizable(t *testing.T) {
	p := &Player{Bin: "mpv", Env: []string{"TERM=xterm"}}
	err := p.Play("asahi", "http://tuner.lan:8010/api/live/asahi.m3u8")
	if !errors.Is(err, ErrNoDisplay) {
		t.Fatalf("err = %v, want ErrNoDisplay", err)
	}
	if p.Playing() != "" {
		t.Fatalf("Playing() = %q, want empty", p.Playing())
	}
}

func TestPlayDisabled(t *testing.T) {
	p := &Player{Bin: "", Env: []string{"DISPLAY=:0"}}
	if err := p.Play("asahi", "http://x/y.m3u8"); err == nil {
		t.Fatal("expected an error when playback is disabled")
	}
}

// writeFakePlayer drops a script that records its argv and then blocks, so a
// test can inspect what a real player would have been given.
func writeFakePlayer(t *testing.T, argsFile string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fakeplayer")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\nsleep 30\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPlayPassesArgsAndURL(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	p := &Player{
		Bin:  writeFakePlayer(t, argsFile),
		Args: []string{"--profile=low-latency"},
		Env:  []string{"DISPLAY=:0"},
	}
	t.Cleanup(p.Stop)

	url := "http://tuner.lan:8010/api/live/asahi.m3u8"
	if err := p.Play("asahi", url); err != nil {
		t.Fatal(err)
	}
	if got := p.Playing(); got != "asahi" {
		t.Fatalf("Playing() = %q", got)
	}

	deadline := time.Now().Add(3 * time.Second)
	var content []byte
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(argsFile); err == nil && len(b) > 0 {
			content = b
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	want := "--profile=low-latency\n" + url + "\n"
	if string(content) != want {
		t.Fatalf("argv = %q, want %q", content, want)
	}
}

// Switching channels must not leave the previous player running — two mpvs
// on one tuner would both pull the stream.
func TestPlayReplacesThePreviousProcess(t *testing.T) {
	first := filepath.Join(t.TempDir(), "first")
	p := &Player{Bin: writeFakePlayer(t, first), Args: []string{}, Env: []string{"DISPLAY=:0"}}
	t.Cleanup(p.Stop)

	if err := p.Play("asahi", "http://x/a.m3u8"); err != nil {
		t.Fatal(err)
	}
	if err := p.Play("TBS1", "http://x/b.m3u8"); err != nil {
		t.Fatal(err)
	}
	if got := p.Playing(); got != "TBS1" {
		t.Fatalf("Playing() = %q, want TBS1", got)
	}

	p.Stop()
	if got := p.Playing(); got != "" {
		t.Fatalf("Playing() = %q after Stop", got)
	}
	// Stop is idempotent — quitting after the player already exited is normal.
	p.Stop()
}
