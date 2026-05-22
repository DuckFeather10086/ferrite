// Package epg refreshes the EPG store by periodically running
// `dvbr epg --schedule --json` per channel and upserting events.
//
// Each refresh acquires an adapter via tuner.Pool, so it serializes
// against live viewing / recording on that adapter. Schedule it for
// off-peak hours (default cron: 0 */6 * * *).
package epg

import "context"

type Refresher struct {
	// fields filled in when wired up
}

// Run blocks until ctx is canceled, ticking through the configured
// channels on the configured cron schedule.
func (r *Refresher) Run(ctx context.Context) error {
	panic("not implemented")
}
