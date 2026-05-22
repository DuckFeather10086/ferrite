// Package recorder runs a single recording job: acquires a tuner
// Lease, drains chunks from the fanout subscription into a file
// (raw .ts or muxed .mp4 via ffmpeg), and updates the store row.
//
// Failure modes that must surface to the recordings.state row:
//   - tuner.Pool.Acquire timeout / failure
//   - no chunks received within STARTUP_TIMEOUT (port watchdog
//     from legacy live_hls.py)
//   - chunk stream closed early (source EOF before end_utc)
//   - disk write error
package recorder

import (
	"context"
	"time"
)

type Job struct {
	ScheduleID int64
	Channel    string
	Title      string
	Start      time.Time
	End        time.Time
	Lead       time.Duration
	Trail      time.Duration
	OutputPath string
}

type Runner struct {
	// tuners *tuner.Pool, store *store.Store, ...
}

// Run executes j synchronously. ctx cancel aborts the recording and
// finalizes the file. Returns nil on success, non-nil on failure
// (state row already updated).
func (r *Runner) Run(ctx context.Context, j Job) error {
	panic("not implemented")
}
