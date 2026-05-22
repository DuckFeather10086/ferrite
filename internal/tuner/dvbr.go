// Package tuner manages DVB adapters and the dvbr subprocesses that
// drive them.
//
// dvbr.go wraps the dvbr CLI:
//   dvbr tune  -> stdout TS stream
//   dvbr scan  -> json
//   dvbr epg   -> json
//   dvbr fe-info
//
// The adapter lock contract: dvbr itself holds the flock on
// /tmp/dvbr-adapter{N}.lock. We do not duplicate it here. If isdbd
// ever needs to hold the lock itself, set DVBR_SKIP_ADAPTER_LOCK=1
// in the child's env (see legacy live_hls.py).
package tuner

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/DuckFeather10086/isdbd/internal/proc"
)

// DvbrCLI is a thin invoker for the dvbr binary. One instance per
// daemon (or per test); methods are safe to call concurrently as long
// as different invocations target different adapters.
type DvbrCLI struct {
	// BinPath is the absolute path to the dvbr executable.
	BinPath string
	// ChannelsFile is passed as --channels to every subcommand.
	ChannelsFile string
	// LockTimeoutMs bounds how long dvbr waits for the frontend to
	// lock before giving up. Zero falls back to dvbr's default (15000).
	LockTimeoutMs int
}

// TsStream is the read-side of a tuned TS pipe plus the teardown
// handle for the underlying subprocess.
type TsStream interface {
	io.ReadCloser
}

// Tune spawns `dvbr tune` against the given adapter and channel, and
// returns its stdout TS stream. Close the stream to tear the tune down.
//
// channel may be a name or alias as defined in the channels file —
// matching rules live in dvbr's config crate.
func (d *DvbrCLI) Tune(ctx context.Context, adapter int, channel string) (TsStream, error) {
	if d.BinPath == "" {
		return nil, fmt.Errorf("tuner: DvbrCLI.BinPath is empty")
	}
	if d.ChannelsFile == "" {
		return nil, fmt.Errorf("tuner: DvbrCLI.ChannelsFile is empty")
	}

	lockMs := d.LockTimeoutMs
	if lockMs <= 0 {
		lockMs = 25000
	}

	args := []string{
		"tune",
		"--adapter", strconv.Itoa(adapter),
		"--frontend", "0",
		"--demux", "0",
		"--channels", d.ChannelsFile,
		"--lock-timeout-ms", strconv.Itoa(lockMs),
		"--output", "-",
		channel,
	}

	p, err := proc.Spawn(ctx, d.BinPath, args...)
	if err != nil {
		return nil, fmt.Errorf("tuner: spawn dvbr tune: %w", err)
	}
	return &tuneStream{p: p}, nil
}

type tuneStream struct {
	p *proc.Process
}

func (t *tuneStream) Read(b []byte) (int, error) { return t.p.Stdout.Read(b) }
func (t *tuneStream) Close() error               { return t.p.Close() }
