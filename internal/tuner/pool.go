package tuner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/DuckFeather10086/isdbd/internal/config"
	"github.com/DuckFeather10086/isdbd/internal/fanout"
)

// Tuner is the seam between Pool and an actual tuner driver. Production
// uses *DvbrCLI; tests substitute a fake.
type Tuner interface {
	Tune(ctx context.Context, adapter int, channel string) (TsStream, error)
}

// Pool manages a fixed set of DVB adapters. A successful Acquire
// returns a Lease that is reference-counted against an active
// "tune session" on one adapter — multiple concurrent Acquires for
// the same channel share the underlying dvbr subprocess and TS
// fanout. When the last Lease is Released (or the source dies and
// the broadcaster closes), the session is torn down.
type Pool struct {
	mu        sync.Mutex
	adapters  map[int]*adapterSlot
	cli       Tuner
	channels  *config.Channels
	bufChunks int // per-subscriber chunk buffer
}

// NewPool builds a Pool over the given adapters. bufChunks bounds
// each subscriber's queue; a sensible default is 8 (≈ 768 KiB).
func NewPool(cli Tuner, channels *config.Channels, adapters []int, bufChunks int) *Pool {
	if bufChunks < 1 {
		bufChunks = 8
	}
	slots := make(map[int]*adapterSlot, len(adapters))
	for _, a := range adapters {
		slots[a] = &adapterSlot{adapter: a}
	}
	return &Pool{
		adapters:  slots,
		cli:       cli,
		channels:  channels,
		bufChunks: bufChunks,
	}
}

// adapterSlot is the per-adapter state. session is non-nil exactly
// when the adapter currently holds a tune.
type adapterSlot struct {
	adapter int
	session *tuneSession
}

// tuneSession is one running dvbr → b25 → fanout pipeline on one
// adapter. refs is bumped by Acquire and decremented by Lease.Release.
type tuneSession struct {
	channel     string
	canonical   string // matched config.Channel.Name
	broadcaster *fanout.Broadcaster
	cancel      context.CancelFunc
	done        chan struct{} // closed when the pump goroutine exits
	refs        int
}

// Lease is one consumer's handle to a tuneSession. Release exactly
// once when done; idempotent.
type Lease struct {
	Sub       *fanout.Sub
	Channel   string
	Adapter   int

	pool     *Pool
	session  *tuneSession
	released bool
}

// Release decrements the underlying session's refcount. When it
// reaches zero, the dvbr subprocess and fanout pump are torn down
// and the adapter becomes available for the next Acquire.
func (l *Lease) Release() {
	if l == nil || l.released {
		return
	}
	l.released = true
	l.pool.release(l.Adapter, l.session, l.Sub)
}

// Acquire returns a Lease for channel. If an adapter is already
// tuned to channel, the existing session is shared. Otherwise an
// idle adapter is tuned. Returns an error when channel is unknown
// or no adapter is available.
func (p *Pool) Acquire(ctx context.Context, channel string) (*Lease, error) {
	ch := p.channels.Find(channel)
	if ch == nil {
		return nil, fmt.Errorf("tuner: channel %q not found", channel)
	}
	canonical := ch.Name

	p.mu.Lock()
	// 1. Same-channel share: any adapter already on it gets reused.
	for adapter, slot := range p.adapters {
		if slot.session != nil && slot.session.canonical == canonical {
			sub := slot.session.broadcaster.Subscribe(p.bufChunks)
			slot.session.refs++
			sess := slot.session
			p.mu.Unlock()
			return &Lease{
				Sub: sub, Channel: canonical, Adapter: adapter,
				pool: p, session: sess,
			}, nil
		}
	}
	// 2. Pick an idle adapter.
	var pick *adapterSlot
	for _, slot := range p.adapters {
		if slot.session == nil {
			pick = slot
			break
		}
	}
	if pick == nil {
		p.mu.Unlock()
		return nil, errors.New("tuner: no adapter available")
	}

	// Hold the slot while we tune (under p.mu) so that two parallel
	// Acquires for different channels can't both grab the same
	// adapter. We release p.mu only after the slot is wired.
	pumpCtx, pumpCancel := context.WithCancel(context.Background())
	sess := &tuneSession{
		channel:     channel,
		canonical:   canonical,
		broadcaster: fanout.New(),
		cancel:      pumpCancel,
		done:        make(chan struct{}),
		refs:        1,
	}
	pick.session = sess
	adapter := pick.adapter
	p.mu.Unlock()

	// Tune outside the lock — the underlying dvbr spawn can block on
	// frontend lock, which we don't want serializing other Acquires.
	stream, err := p.cli.Tune(pumpCtx, adapter, channel)
	if err != nil {
		pumpCancel()
		p.mu.Lock()
		pick.session = nil
		p.mu.Unlock()
		close(sess.done)
		return nil, fmt.Errorf("tuner: tune %q on adapter %d: %w", channel, adapter, err)
	}

	sub := sess.broadcaster.Subscribe(p.bufChunks)

	go func() {
		defer close(sess.done)
		defer stream.Close()
		if err := sess.broadcaster.Pump(stream); err != nil {
			slog.Warn("tuner: pump exited with error",
				"channel", canonical, "adapter", adapter, "err", err)
		} else {
			slog.Info("tuner: pump exited (source EOF)",
				"channel", canonical, "adapter", adapter)
		}
		// Source died (or we were canceled). Mark the adapter idle
		// so a future Acquire can re-tune it. Existing leases keep
		// their (now-closed) Sub channels until they Release.
		p.mu.Lock()
		if pick.session == sess {
			pick.session = nil
		}
		p.mu.Unlock()
	}()

	return &Lease{
		Sub: sub, Channel: canonical, Adapter: adapter,
		pool: p, session: sess,
	}, nil
}

// release decrements the refcount on sess (held against adapter)
// and tears down the tune when refs hit zero.
func (p *Pool) release(adapter int, sess *tuneSession, sub *fanout.Sub) {
	sess.broadcaster.Unsubscribe(sub)

	p.mu.Lock()
	sess.refs--
	if sess.refs > 0 {
		p.mu.Unlock()
		return
	}
	// Last lease released. Cancel the pump (which closes the source
	// and the broadcaster). Wait for the goroutine to clear the slot.
	cancel := sess.cancel
	done := sess.done
	p.mu.Unlock()

	cancel()
	<-done
}

// Status reports current adapter usage. Snapshot, not live-updating.
type AdapterStatus struct {
	Adapter int    `json:"adapter"`
	Channel string `json:"channel,omitempty"`
	Refs    int    `json:"refs"`
}

func (p *Pool) Status() []AdapterStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]AdapterStatus, 0, len(p.adapters))
	for adapter, slot := range p.adapters {
		st := AdapterStatus{Adapter: adapter}
		if slot.session != nil {
			st.Channel = slot.session.canonical
			st.Refs = slot.session.refs
		}
		out = append(out, st)
	}
	return out
}
