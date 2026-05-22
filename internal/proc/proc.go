// Package proc wraps os/exec for the daemon's subprocess needs:
// process-group lifecycle (so a single kill takes down a whole
// dvbr|b25|ffmpeg pipeline), stderr piped into slog, and clean
// teardown on context cancel.
//
// The legacy live_hls.py used setsid + killpg to manage the shell
// pipeline; reproduce that here. The startup/steady-state watchdog
// pattern (fail loudly if no bytes flow) belongs in the consumer
// package (e.g. hls, recorder), not here.
package proc

import (
	"context"
	"io"
	"os/exec"
)

// Spawn starts cmd with its own process group and returns its stdout
// reader. Stderr is piped to slog at warn level. The returned cancel
// function SIGTERMs the entire process group.
func Spawn(ctx context.Context, name string, args ...string) (stdout io.ReadCloser, cancel func(), err error) {
	_ = exec.CommandContext // placeholder; real impl will SysProcAttr.Setpgid = true
	panic("not implemented")
}
