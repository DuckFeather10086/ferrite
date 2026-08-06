package tuner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DuckFeather10086/ferrite/internal/config"
	"github.com/DuckFeather10086/ferrite/internal/fanout"
)

// ErrNoAdapter means every adapter is busy with work that outranks (or
// ties) the incoming claim. Callers should surface this as "tuner
// busy", not as a hardware failure.
var ErrNoAdapter = errors.New("tuner: no adapter available")

// ErrNoCapableAdapter means no adapter in the pool can receive the
// channel's delivery system at all — a BS channel on a terrestrial-only
// box. Distinct from ErrNoAdapter on purpose: that one is "come back in a
// minute", this one will never succeed, and conflating them turns a
// missing tuner into an unexplained lock timeout reported as a weak
// signal.
var ErrNoCapableAdapter = errors.New("tuner: no adapter supports this delivery system")

// preemptTimeout bounds how long a preemptor waits for the evicted
// holder to actually let go. A killed `dvbr` child is reaped in well
// under a second; anything near this bound means a wedged subprocess.
const preemptTimeout = 15 * time.Second

// Priority ranks competing claims on one adapter. A claim preempts an
// existing one only when it is *strictly* higher — equal priorities
// never evict each other, so two viewers on different channels get
// ErrNoAdapter rather than fighting over the frontend.
type Priority int

const (
	// PrioBackground is opportunistic work that must always yield:
	// EPG/EIT collection. Any live or recording claim evicts it.
	PrioBackground Priority = 0
	// PrioLive is someone watching right now.
	PrioLive Priority = 10
	// PrioRecord is a recording in flight. A missed recording can't be
	// recovered, so it outranks live viewing — with a single adapter,
	// a due recording takes the tuner and live playback drops.
	PrioRecord Priority = 20
)

func (p Priority) String() string {
	switch {
	case p >= PrioRecord:
		return "record"
	case p >= PrioLive:
		return "live"
	default:
		return "background"
	}
}

// Tuner is the seam between Pool and an actual tuner driver. Production
// uses *DvbrCLI; tests substitute a fake.
type Tuner interface {
	Tune(ctx context.Context, adapter int, channel string) (TsStream, error)
}

// Pool manages a fixed set of DVB adapters and is the *only* arbiter
// for them inside the daemon. Three kinds of consumer contend here:
//
//   - live HLS sessions          → AcquireAt(PrioLive)
//   - recordings                 → AcquireAt(PrioRecord)
//   - EPG scans (external dvbr)  → Reserve(PrioBackground)
//
// A successful Acquire returns a Lease reference-counted against an
// active "tune session" on one adapter — concurrent Acquires for the
// same channel share the underlying dvbr subprocess and TS fanout.
// When the last Lease is Released (or the source dies and the
// broadcaster closes), the session is torn down.
//
// Anything that drives the hardware out-of-process instead of through
// a fanout (currently only `dvbr epg`) must hold a Reservation, so its
// use of the adapter is visible to — and preemptible by — everything
// else.
type Pool struct {
	mu        sync.Mutex
	adapters  map[int]*adapterSlot
	order     []int // sorted adapter numbers: deterministic selection
	cli       Tuner
	channels  *config.Channels
	bufChunks int // per-subscriber chunk buffer
}

// NewPool builds a Pool over the given adapters. bufChunks bounds
// each subscriber's queue; a sensible default is 8 (≈ 768 KiB).
//
// Each adapter carries the delivery systems its frontend can tune (see
// config.Adapter); a claim is only ever offered adapters that can receive
// the channel it names. On a uniform terrestrial box — every adapter
// ["ISDBT"], every channel ISDBT — that filter passes everything and the
// selection is exactly what it was before.
func NewPool(cli Tuner, channels *config.Channels, adapters []config.Adapter, bufChunks int) *Pool {
	if bufChunks < 1 {
		bufChunks = 8
	}
	slots := make(map[int]*adapterSlot, len(adapters))
	order := make([]int, 0, len(adapters))
	for _, a := range adapters {
		if _, dup := slots[a.N]; dup {
			continue
		}
		slots[a.N] = &adapterSlot{adapter: a.N, systems: a.Systems}
		order = append(order, a.N)
	}
	sort.Ints(order)
	return &Pool{
		adapters:  slots,
		order:     order,
		cli:       cli,
		channels:  channels,
		bufChunks: bufChunks,
	}
}

// adapterSlot is the per-adapter state. Exactly one of session /
// reserved is non-nil while the adapter is in use.
//
// claimed marks a transition in flight (tuning, or evicting a previous
// holder). A claimed slot is invisible to idle-selection and to
// preemption so two racing claims can't both take it, but it stays
// visible to same-channel sharing: a second viewer arriving mid-tune
// should join that tune, not be told the tuner is busy.
type adapterSlot struct {
	adapter int
	// systems is what this frontend can tune, upper-cased. Empty means
	// unknown, which is read as "anything" — a pool built without
	// capability information has to keep behaving as it did.
	systems  []string
	session  *tuneSession
	reserved *Reservation
	claimed  bool
}

func (s *adapterSlot) free() bool { return s.session == nil && s.reserved == nil }

// supports reports whether this adapter can receive system. An empty
// system (a channel that does not say) or an unlabelled adapter matches
// everything: the filter exists to keep a BS channel off a terrestrial
// frontend, not to make an under-described setup unusable.
func (s *adapterSlot) supports(system string) bool {
	if system == "" || len(s.systems) == 0 {
		return true
	}
	for _, have := range s.systems {
		if have == system {
			return true
		}
	}
	return false
}

// prio reports the effective priority of the slot's current holder.
// Caller must hold Pool.mu.
func (s *adapterSlot) prio() Priority {
	switch {
	case s.reserved != nil:
		return s.reserved.prio
	case s.session != nil:
		return s.session.prio()
	default:
		return PrioBackground
	}
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
	// prios is the multiset of live holder priorities; the session's
	// effective priority is the highest one still held. A channel being
	// recorded *and* watched must not be evictable at live priority.
	prios   map[Priority]int
	preempt chan struct{} // closed when the session is being evicted
	once    sync.Once     // guards closing preempt
}

// prio returns the highest priority still held. Caller must hold Pool.mu.
func (s *tuneSession) prio() Priority {
	best := PrioBackground
	for p, n := range s.prios {
		if n > 0 && p > best {
			best = p
		}
	}
	return best
}

func (s *tuneSession) markPreempted() { s.once.Do(func() { close(s.preempt) }) }

// Lease is one consumer's handle to a tuneSession. Release exactly
// once when done; idempotent.
type Lease struct {
	Sub     *fanout.Sub
	Channel string
	Adapter int

	pool     *Pool
	session  *tuneSession
	prio     Priority
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
	l.pool.release(l.Adapter, l.session, l.Sub, l.prio)
}

// Subscribe adds a second consumer to the tune this lease already holds,
// without a second claim on the adapter.
//
// This is for work that reads the same bytes for a different purpose — the
// caption decode beside the HLS encode. Acquiring a second lease would also
// work (same channel joins the same tune) but has two costs that matter: the
// adapter reports two live claims where there is one viewer, and if the tune
// happened to have just died, the second Acquire would *start a new one*,
// leaving the first consumer attached to a dead broadcaster while a second
// dvb-rs fights the flock. A subscription cannot do either: it lives and dies
// with the tune this lease is on.
//
// Unsubscribe when done. Releasing the lease closes the broadcaster anyway, so
// a missed Unsubscribe leaks nothing beyond the lease's own lifetime.
func (l *Lease) Subscribe() *fanout.Sub {
	return l.session.broadcaster.Subscribe(l.pool.bufChunks)
}

// Unsubscribe drops a Sub taken with Subscribe.
func (l *Lease) Unsubscribe(sub *fanout.Sub) {
	if sub == nil {
		return
	}
	l.session.broadcaster.Unsubscribe(sub)
}

// Preempted is closed when a higher-priority claim evicts this lease's
// tune. Sub.Ch closes too, so a plain read loop already terminates;
// watch this when you need to tell "we were bumped" apart from "the
// tuner died" for reporting.
func (l *Lease) Preempted() <-chan struct{} { return l.session.preempt }

// Reservation is an exclusive hold on an adapter for a caller that
// drives the hardware itself — today only `dvbr epg`, which does its
// own tune and EIT tap rather than reading a fanout.
//
// The holder MUST watch Preempted and abandon its work (killing any
// child process) when it fires, then Release. The preemptor blocks
// until Release returns, which is what guarantees the child's flock on
// /tmp/dvbr-adapter{N}.lock is gone before the next tune starts.
type Reservation struct {
	Adapter int

	prio    Priority
	pool    *Pool
	slot    *adapterSlot
	preempt chan struct{}
	done    chan struct{}
	once    sync.Once
	relOnce sync.Once
}

// Preempted fires when something outranking this reservation needs the
// adapter.
func (r *Reservation) Preempted() <-chan struct{} { return r.preempt }

func (r *Reservation) markPreempted() { r.once.Do(func() { close(r.preempt) }) }

// Release hands the adapter back. Idempotent.
func (r *Reservation) Release() {
	if r == nil {
		return
	}
	r.relOnce.Do(func() {
		p := r.pool
		p.mu.Lock()
		if r.slot.reserved == r {
			r.slot.reserved = nil
		}
		p.mu.Unlock()
		close(r.done)
	})
}

// Acquire returns a Lease for channel at PrioLive.
func (p *Pool) Acquire(ctx context.Context, channel string) (*Lease, error) {
	return p.AcquireAt(ctx, channel, PrioLive)
}

// AcquireAt returns a Lease for channel at the given priority. If an
// adapter is already tuned to channel, the existing session is shared
// (and its effective priority rises to at least prio). Otherwise an
// idle adapter is tuned, or — failing that — a strictly lower-priority
// holder is evicted. Returns ErrNoAdapter when nothing can be freed,
// or an error when channel is unknown.
func (p *Pool) AcquireAt(ctx context.Context, channel string, prio Priority) (*Lease, error) {
	ch := p.channels.Find(channel)
	if ch == nil {
		return nil, fmt.Errorf("tuner: channel %q not found", channel)
	}
	canonical := ch.Name
	// What kind of frontend this channel needs. Applied below to idle
	// selection and to preemption, but *not* to same-channel sharing: an
	// adapter already tuned to it has settled the question.
	system := ch.DeliverySystem()

	p.mu.Lock()
	// 1. Same-channel share: any adapter already on it gets reused,
	//    including one still mid-tune.
	for _, adapter := range p.order {
		slot := p.adapters[adapter]
		if slot.session != nil && slot.session.canonical == canonical {
			sess := slot.session
			sub := sess.broadcaster.Subscribe(p.bufChunks)
			sess.refs++
			sess.prios[prio]++
			p.mu.Unlock()
			return &Lease{
				Sub: sub, Channel: canonical, Adapter: adapter,
				pool: p, session: sess, prio: prio,
			}, nil
		}
	}

	// 2. Idle adapter, else 3. evict a lower-priority holder — considering
	//    only adapters that can receive this channel.
	if !p.capableLocked(system) {
		p.mu.Unlock()
		return nil, p.noCapableAdapter(strconv.Quote(canonical), system)
	}
	pick := p.idleSlotLocked(system)
	if pick == nil {
		victim := p.victimLocked(prio, system)
		if victim == nil {
			p.mu.Unlock()
			return nil, ErrNoAdapter
		}
		victim.claimed = true
		p.mu.Unlock()

		err := p.evict(ctx, victim, prio, canonical)

		p.mu.Lock()
		if err != nil {
			victim.claimed = false
			p.mu.Unlock()
			return nil, fmt.Errorf("tuner: preempt adapter %d: %w", victim.adapter, err)
		}
		pick = victim
	}

	// Hold the slot while we tune (marking it claimed under p.mu) so
	// that two parallel claims can't both grab the same adapter. We
	// release p.mu only after the slot is wired.
	pumpCtx, pumpCancel := context.WithCancel(context.Background())
	sess := &tuneSession{
		channel:     channel,
		canonical:   canonical,
		broadcaster: fanout.New(),
		cancel:      pumpCancel,
		done:        make(chan struct{}),
		refs:        1,
		prios:       map[Priority]int{prio: 1},
		preempt:     make(chan struct{}),
	}
	pick.session = sess
	pick.claimed = true
	adapter := pick.adapter
	p.mu.Unlock()

	// Tune outside the lock — the underlying dvbr spawn can block on
	// frontend lock, which we don't want serializing other claims.
	stream, err := p.cli.Tune(pumpCtx, adapter, channel)
	if err != nil {
		pumpCancel()
		p.mu.Lock()
		if pick.session == sess {
			pick.session = nil
		}
		pick.claimed = false
		p.mu.Unlock()
		// Anyone who joined this session mid-tune (step 1) is waiting on
		// a broadcaster that will never pump — close it so their read
		// loops see EOF now instead of hanging until a watchdog fires.
		sess.broadcaster.Close()
		close(sess.done)
		return nil, fmt.Errorf("tuner: tune %q on adapter %d: %w", channel, adapter, err)
	}

	sub := sess.broadcaster.Subscribe(p.bufChunks)

	p.mu.Lock()
	pick.claimed = false
	p.mu.Unlock()

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
		pool: p, session: sess, prio: prio,
	}, nil
}

// Reserve claims an adapter without tuning it, for a caller that drives
// the hardware out-of-process. Evicts a strictly lower-priority holder
// if no adapter is idle. Release when done.
//
// system names the delivery system the caller is going to tune ("ISDBT",
// "ISDBS"); "" takes whatever is free. It has to be passed in rather than
// derived, because the point of a Reservation is that the caller — a
// `dvbr epg` pass, a blind frequency scan — does its own tuning, and the
// Pool cannot see what for. Handing back an adapter that cannot receive
// what the caller is about to ask of it is the failure this exists to
// prevent, and out-of-process it is the least debuggable version of it.
func (p *Pool) Reserve(ctx context.Context, prio Priority, system string) (*Reservation, error) {
	p.mu.Lock()
	if !p.capableLocked(system) {
		p.mu.Unlock()
		return nil, p.noCapableAdapter("this reservation", system)
	}
	pick := p.idleSlotLocked(system)
	if pick == nil {
		victim := p.victimLocked(prio, system)
		if victim == nil {
			p.mu.Unlock()
			return nil, ErrNoAdapter
		}
		victim.claimed = true
		p.mu.Unlock()

		err := p.evict(ctx, victim, prio, "reservation")

		p.mu.Lock()
		if err != nil {
			victim.claimed = false
			p.mu.Unlock()
			return nil, fmt.Errorf("tuner: preempt adapter %d: %w", victim.adapter, err)
		}
		pick = victim
	}
	res := &Reservation{
		Adapter: pick.adapter,
		prio:    prio,
		pool:    p,
		slot:    pick,
		preempt: make(chan struct{}),
		done:    make(chan struct{}),
	}
	pick.reserved = res
	pick.claimed = false
	p.mu.Unlock()

	slog.Info("tuner: adapter reserved", "adapter", res.Adapter, "prio", prio.String())
	return res, nil
}

// idleSlotLocked returns the lowest-numbered unused, untransitioning slot
// that can receive system. Caller must hold p.mu.
func (p *Pool) idleSlotLocked(system string) *adapterSlot {
	for _, adapter := range p.order {
		if slot := p.adapters[adapter]; slot.free() && !slot.claimed && slot.supports(system) {
			return slot
		}
	}
	return nil
}

// victimLocked picks the lowest-priority holder that prio outranks and
// that can receive system, or nil when there is none. Caller must hold
// p.mu.
//
// Capability is checked here as well as in idleSlotLocked, and it has to
// be: evicting a terrestrial tune to make room for a BS channel that the
// same frontend cannot receive costs a viewer their picture and gains
// nothing.
func (p *Pool) victimLocked(prio Priority, system string) *adapterSlot {
	var best *adapterSlot
	for _, adapter := range p.order {
		slot := p.adapters[adapter]
		if slot.claimed || slot.free() || !slot.supports(system) {
			continue
		}
		if slot.prio() >= prio {
			continue
		}
		if best == nil || slot.prio() < best.prio() {
			best = slot
		}
	}
	return best
}

// capableLocked reports whether any adapter at all can receive system,
// busy or not. Caller must hold p.mu.
func (p *Pool) capableLocked(system string) bool {
	for _, adapter := range p.order {
		if p.adapters[adapter].supports(system) {
			return true
		}
	}
	return false
}

// noCapableAdapter builds the error for a claim no frontend can serve,
// naming both what was asked for and what is installed — the whole point
// of the distinction is that the message tells you to buy a tuner rather
// than to check the aerial.
func (p *Pool) noCapableAdapter(what, system string) error {
	p.mu.Lock()
	have := make([]string, 0, len(p.order))
	for _, adapter := range p.order {
		slot := p.adapters[adapter]
		have = append(have, fmt.Sprintf("%d:%s", adapter, strings.Join(slot.systems, "+")))
	}
	p.mu.Unlock()
	return fmt.Errorf("%w: %s needs %s, adapters are [%s]",
		ErrNoCapableAdapter, what, system, strings.Join(have, " "))
}

// evict tears down slot's current holder and waits for it to let go.
// slot must already be marked claimed, and p.mu must NOT be held.
func (p *Pool) evict(ctx context.Context, slot *adapterSlot, by Priority, forWhat string) error {
	p.mu.Lock()
	sess, res := slot.session, slot.reserved
	p.mu.Unlock()

	var wait chan struct{}
	switch {
	case res != nil:
		res.markPreempted()
		wait = res.done
	case sess != nil:
		sess.markPreempted()
		sess.cancel()
		wait = sess.done
	default:
		return nil
	}

	held := "reservation"
	if sess != nil {
		held = sess.canonical
	}
	slog.Info("tuner: preempting adapter",
		"adapter", slot.adapter, "holding", held,
		"for", forWhat, "by", by.String())

	timer := time.NewTimer(preemptTimeout)
	defer timer.Stop()
	select {
	case <-wait:
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("holder did not yield within %s", preemptTimeout)
	}

	p.mu.Lock()
	if slot.session == sess {
		slot.session = nil
	}
	if slot.reserved == res {
		slot.reserved = nil
	}
	p.mu.Unlock()
	return nil
}

// release decrements the refcount on sess (held against adapter)
// and tears down the tune when refs hit zero.
func (p *Pool) release(adapter int, sess *tuneSession, sub *fanout.Sub, prio Priority) {
	sess.broadcaster.Unsubscribe(sub)

	p.mu.Lock()
	sess.refs--
	if sess.prios[prio] > 0 {
		sess.prios[prio]--
	}
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

// AdapterStatus reports current adapter usage. Snapshot, not
// live-updating.
type AdapterStatus struct {
	Adapter int `json:"adapter"`
	// Systems is what this frontend can tune. Reported because "no adapter
	// supports this delivery system" is otherwise a claim with nothing on
	// screen to check it against.
	Systems []string `json:"systems,omitempty"`
	Channel string   `json:"channel,omitempty"`
	Refs    int      `json:"refs"`
	// Prio is the effective priority of the current holder, as a
	// string ("live", "record", "background"); empty when idle.
	Prio string `json:"prio,omitempty"`
	// Reserved is true when an out-of-process consumer (EPG) holds the
	// adapter rather than a tune session.
	Reserved bool `json:"reserved,omitempty"`
}

func (p *Pool) Status() []AdapterStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]AdapterStatus, 0, len(p.order))
	for _, adapter := range p.order {
		slot := p.adapters[adapter]
		st := AdapterStatus{Adapter: adapter, Systems: slot.systems}
		switch {
		case slot.reserved != nil:
			st.Reserved = true
			st.Prio = slot.reserved.prio.String()
		case slot.session != nil:
			st.Channel = slot.session.canonical
			st.Refs = slot.session.refs
			st.Prio = slot.session.prio().String()
		}
		out = append(out, st)
	}
	return out
}

// CanServe reports whether a claim at prio for channel could be
// satisfied right now: an adapter is already on that channel, one is
// idle, or one holds something prio outranks. Advisory only — it races
// with concurrent claims — but good enough to reject a request with a
// clear "tuner busy" instead of accepting it and failing async.
func (p *Pool) CanServe(channel string, prio Priority) bool {
	canonical := channel
	var system string
	if ch := p.channels.Find(channel); ch != nil {
		canonical = ch.Name
		system = ch.DeliverySystem()
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	for _, adapter := range p.order {
		slot := p.adapters[adapter]
		if slot.session != nil && slot.session.canonical == canonical {
			return true
		}
	}
	return p.idleSlotLocked(system) != nil || p.victimLocked(prio, system) != nil
}
