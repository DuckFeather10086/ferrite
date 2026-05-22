// Package scheduler ticks every minute, finds schedules whose
// (start_utc - lead_s) <= now, and dispatches them to recorder.Runner.
//
// Uses github.com/robfig/cron/v3 (to be added when implementing).
package scheduler

import "context"

type Scheduler struct {
	// runner *recorder.Runner, store *store.Store
}

func (s *Scheduler) Run(ctx context.Context) error {
	panic("not implemented")
}
