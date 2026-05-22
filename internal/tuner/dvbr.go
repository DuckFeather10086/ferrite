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
// ever needs to hold the lock itself, set DVBR_SKIP_ADAPTER_LOCK=1 in
// the child's env (see legacy live_hls.py).
package tuner

import "context"

type DvbrCLI struct {
	BinPath      string
	ChannelsFile string
}

// Tune spawns `dvbr tune` and returns a reader over its stdout TS.
// Caller must close the reader to release the subprocess.
func (d *DvbrCLI) Tune(ctx context.Context, adapter int, channel string) (TsStream, error) {
	panic("not implemented")
}

// EPG runs `dvbr epg --schedule --json <channel>` and returns the
// raw JSON bytes for the epg ingester to parse.
func (d *DvbrCLI) EPG(ctx context.Context, adapter int, channel string) ([]byte, error) {
	panic("not implemented")
}

// TsStream is the byte source plus a teardown handle.
type TsStream interface {
	Read(p []byte) (int, error)
	Close() error
}
