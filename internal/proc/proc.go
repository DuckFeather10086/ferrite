// Package proc wraps os/exec for the daemon's subprocess needs:
// process-group lifecycle (so a single kill takes down a whole
// dvbr|b25|ffmpeg pipeline), stderr piped into slog, and clean
// teardown on Close or context cancel.
//
// The legacy live_hls.py used setsid + killpg to manage the shell
// pipeline; this package is the Go equivalent. The startup /
// steady-state watchdog pattern (fail loudly if no bytes flow)
// belongs in the consumer package (hls, recorder), not here.
package proc

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// killGracePeriod is how long we wait after SIGTERM before escalating
// to SIGKILL. The dvbr DVR loop unwinds in well under a second; ffmpeg
// can take longer to flush. 2s is a comfortable upper bound.
const killGracePeriod = 2 * time.Second

// Process is a running subprocess whose stdout is exposed as an
// io.Reader and whose lifetime is bounded by Close (or the parent
// context's cancellation).
//
// Always defer Close, even after Stdout returns EOF — the underlying
// os/exec.Cmd.Wait must run to reap the zombie and release stderr.
//
// Implementation note: we hand the child its own os.Pipe write ends
// for stdout/stderr (instead of cmd.StdoutPipe()) so that cmd.Wait
// does not close the reader under our consumer. Wait's auto-close is
// racy when the child writes data then exits immediately — see
// https://pkg.go.dev/os/exec#Cmd.StdoutPipe ("incorrect to call Wait
// before all reads have completed").
type Process struct {
	// Stdout is the child's stdout. Reads return io.EOF when the
	// child exits and the pipe drains.
	Stdout io.Reader

	name     string
	cmd      *exec.Cmd
	pgid     int
	stdoutR  *os.File      // owned; closed in Close
	waited   chan struct{} // closed once cmd.Wait returns
	waitErr  error         // result of cmd.Wait; only valid after <-waited
	closed   bool          // guarded by closing logic; idempotent Close
}

// Spawn starts name with args in its own process group. Stderr is
// piped line-by-line into slog at warn level (tagged with the command
// name). The returned Process can be cancelled via ctx, Close, or
// will clean up naturally if the child exits on its own.
func Spawn(ctx context.Context, name string, args ...string) (*Process, error) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("proc.Spawn(%s): stdout pipe: %w", name, err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		return nil, fmt.Errorf("proc.Spawn(%s): stderr pipe: %w", name, err)
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	if err := cmd.Start(); err != nil {
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		return nil, fmt.Errorf("proc.Spawn(%s): start: %w", name, err)
	}

	// Drop the parent's references to the write ends; the only
	// remaining writers are the inherited fds inside the child, so
	// the readers will see EOF naturally when the child exits.
	stdoutW.Close()
	stderrW.Close()

	// Because Setpgid is set, the new pgid equals the child's pid.
	pgid := cmd.Process.Pid

	p := &Process{
		Stdout:  stdoutR,
		name:    name,
		cmd:     cmd,
		pgid:    pgid,
		stdoutR: stdoutR,
		waited:  make(chan struct{}),
	}

	// Drain stderr into slog. Long-line safe up to 1 MiB.
	go func() {
		defer stderrR.Close()
		scanner := bufio.NewScanner(stderrR)
		scanner.Buffer(make([]byte, 0, 4096), 1<<20)
		for scanner.Scan() {
			slog.Warn("subprocess stderr", "cmd", name, "line", scanner.Text())
		}
		if err := scanner.Err(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			slog.Debug("subprocess stderr scan error", "cmd", name, "err", err)
		}
	}()

	// Reap on exit.
	go func() {
		p.waitErr = cmd.Wait()
		close(p.waited)
	}()

	// Honor ctx cancellation by killing the process group.
	go func() {
		select {
		case <-ctx.Done():
			p.killGroup()
		case <-p.waited:
		}
	}()

	return p, nil
}

// Close tears the subprocess down: SIGTERM the process group, wait
// up to killGracePeriod, then SIGKILL if still alive. Always blocks
// until the child is reaped. Safe to call multiple times.
func (p *Process) Close() error {
	if p.closed {
		<-p.waited
		return p.exitError()
	}
	p.closed = true
	p.killGroup()
	<-p.waited
	p.stdoutR.Close()
	return p.exitError()
}

// Wait blocks until the subprocess exits on its own (or is killed by
// Close / ctx cancel). Returns the exit error if any. Distinct from
// Close in that it does NOT initiate teardown.
func (p *Process) Wait() error {
	<-p.waited
	return p.exitError()
}

func (p *Process) exitError() error {
	if p.waitErr == nil {
		return nil
	}
	// SIGTERM-induced exit is expected when we kill; surface it as
	// a sentinel rather than treating it as an unexpected failure.
	var exitErr *exec.ExitError
	if errors.As(p.waitErr, &exitErr) {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			sig := ws.Signal()
			if sig == syscall.SIGTERM || sig == syscall.SIGKILL {
				return nil
			}
		}
	}
	return p.waitErr
}

func (p *Process) killGroup() {
	// Negative pid → entire process group. Ignore ESRCH (already gone).
	_ = syscall.Kill(-p.pgid, syscall.SIGTERM)
	select {
	case <-p.waited:
		return
	case <-time.After(killGracePeriod):
	}
	slog.Warn("subprocess did not exit on SIGTERM; sending SIGKILL",
		"cmd", p.name, "pgid", p.pgid)
	_ = syscall.Kill(-p.pgid, syscall.SIGKILL)
}
