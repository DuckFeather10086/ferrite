package tuner

import (
	"context"
	"sync"

	"github.com/DuckFeather10086/isdbd/internal/fanout"
)

// Pool holds the per-adapter state. One adapter can serve multiple
// Leases as long as they all want the same channel (same frequency
// + service). Different-channel requests block or fail per policy.
type Pool struct {
	mu       sync.Mutex
	adapters map[int]*adapterState
	cli      *DvbrCLI
}

type adapterState struct {
	channel     string                 // currently tuned channel, "" if idle
	broadcaster *fanout.Broadcaster    // nil if idle
	refs        int                    // number of live Leases
}

// Lease is a subscription to a tuned service. Release exactly once
// when done; the underlying dvbr subprocess is torn down when refs
// hit zero.
type Lease struct {
	Sub     *fanout.Sub
	Release func()
}

// Acquire returns a Lease for channel on some adapter. If an adapter
// is already tuned to channel, the existing tune is shared. Else an
// idle adapter is picked and tuned.
func (p *Pool) Acquire(ctx context.Context, channel string) (*Lease, error) {
	panic("not implemented")
}

// Status reports current adapter usage for /api/status.
func (p *Pool) Status() []AdapterStatus {
	panic("not implemented")
}

type AdapterStatus struct {
	Adapter int    `json:"adapter"`
	Channel string `json:"channel"`
	Refs    int    `json:"refs"`
}
