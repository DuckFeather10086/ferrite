// Package tuner manages DVB adapters and the dvb-rs subprocesses that
// drive them.
//
// dvbr.go wraps the dvb-rs CLI:
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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"

	"github.com/DuckFeather10086/ferrite/internal/proc"
)

// DvbrCLI is a thin invoker for the dvb-rs binary. One instance per
// daemon (or per test); methods are safe to call concurrently as long
// as different invocations target different adapters.
type DvbrCLI struct {
	// BinPath is the absolute path to the dvbr executable.
	BinPath string
	// B25Bin is the absolute path to the b25-rs descrambler. When set,
	// Tune chains dvbr's output through b25 so consumers receive
	// descrambled TS. Empty disables descrambling (raw TS — only useful
	// for free-to-air or when no B-CAS card is present).
	B25Bin string
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

// Tune spawns `dvb-rs tune` against the given adapter and channel, and
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

	dvbrProc, err := proc.Spawn(ctx, d.BinPath, args...)
	if err != nil {
		return nil, fmt.Errorf("tuner: spawn dvbr tune: %w", err)
	}

	// Free-to-air / no card: hand back dvbr's raw stdout directly.
	if d.B25Bin == "" {
		return &tuneStream{dvbr: dvbrProc}, nil
	}

	// Scrambled (the common ISDB-T case): chain dvbr → b25. b25 reads
	// encrypted TS on stdin and writes descrambled TS on stdout:
	//   b25 -v 0 - -
	// -v 0 silences the per-chunk progress line b25 prints to stderr
	// (which would otherwise flood slog); end-of-stream warnings such
	// as "unpurchased ECM" still surface.
	b25Proc, err := proc.SpawnOpt(ctx, proc.SpawnOpts{Stdin: true}, d.B25Bin, "-v", "0", "-", "-")
	if err != nil {
		_ = dvbrProc.Close()
		return nil, fmt.Errorf("tuner: spawn b25: %w", err)
	}

	// Pump dvbr's stdout into b25's stdin. When dvbr exits (source EOF
	// or teardown) we close b25's stdin so it flushes its final packets
	// and exits cleanly. The goroutine always terminates: io.Copy
	// returns once either side of the pipe is closed by teardown.
	go func() {
		_, copyErr := io.Copy(b25Proc.Stdin, dvbrProc.Stdout)
		_ = b25Proc.Stdin.Close()
		if copyErr != nil &&
			!errors.Is(copyErr, os.ErrClosed) &&
			!errors.Is(copyErr, io.ErrClosedPipe) {
			slog.Debug("tuner: dvb-rs→b25-rs copy ended", "adapter", adapter, "err", copyErr)
		}
	}()

	return &tuneStream{dvbr: dvbrProc, b25: b25Proc}, nil
}

// tuneStream is the read side of a tuned pipeline. When b25 is non-nil
// the readable stream is b25's descrambled stdout; otherwise it is
// dvbr's raw stdout. Close tears down the whole pipeline.
type tuneStream struct {
	dvbr *proc.Process
	b25  *proc.Process // nil when descrambling is disabled
}

func (t *tuneStream) Read(b []byte) (int, error) {
	if t.b25 != nil {
		return t.b25.Stdout.Read(b)
	}
	return t.dvbr.Stdout.Read(b)
}

// Close tears down both subprocesses. b25 (downstream) is closed first
// so a blocked reader unblocks promptly; dvbr (upstream) follows. Each
// is its own process group, so ordering only affects teardown latency,
// not correctness.
func (t *tuneStream) Close() error {
	var err error
	if t.b25 != nil {
		err = t.b25.Close()
	}
	if e := t.dvbr.Close(); e != nil && err == nil {
		err = e
	}
	return err
}
