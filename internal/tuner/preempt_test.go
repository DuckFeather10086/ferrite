package tuner

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Reserve models the EPG path: it holds the adapter without tuning.
func TestPool_ReserveThenRelease(t *testing.T) {
	ft := &fakeTuner{makeStream: emptyHold}
	p := newPool(t, 1, ft)

	res, err := p.Reserve(context.Background(), PrioBackground)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if res.Adapter != 0 {
		t.Fatalf("adapter = %d, want 0", res.Adapter)
	}
	// Status must show the adapter as taken, or nothing can arbitrate.
	st := p.Status()
	if len(st) != 1 || !st[0].Reserved {
		t.Fatalf("status = %+v, want reserved", st)
	}

	res.Release()
	if st := p.Status(); st[0].Reserved {
		t.Fatalf("still reserved after Release: %+v", st)
	}
	// Release is idempotent.
	res.Release()

	if _, err := p.Acquire(context.Background(), "mx"); err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
}

// The bug this whole change exists for: an EPG scan must not keep live
// TV off the air. A live claim preempts the background reservation.
func TestPool_LivePreemptsBackgroundReservation(t *testing.T) {
	ft := &fakeTuner{makeStream: emptyHold}
	p := newPool(t, 1, ft)

	res, err := p.Reserve(context.Background(), PrioBackground)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// The holder's contract: watch Preempted, drop its work, Release.
	released := make(chan struct{})
	go func() {
		<-res.Preempted()
		res.Release()
		close(released)
	}()

	lease, err := p.Acquire(context.Background(), "mx")
	if err != nil {
		t.Fatalf("live Acquire should have preempted EPG, got %v", err)
	}
	defer lease.Release()

	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("reservation holder was never told to yield")
	}
	if got := ft.tuneCount.Load(); got != 1 {
		t.Fatalf("tuneCount = %d, want 1", got)
	}
}

// A reservation that never lets go must not wedge the adapter forever;
// the claim fails and the slot stays usable once the holder does yield.
func TestPool_PreemptTimesOutOnWedgedHolder(t *testing.T) {
	ft := &fakeTuner{makeStream: emptyHold}
	p := newPool(t, 1, ft)

	res, err := p.Reserve(context.Background(), PrioBackground)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// Don't release. A short deadline stands in for preemptTimeout.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := p.AcquireAt(ctx, "mx", PrioLive); err == nil {
		t.Fatal("Acquire should fail while the holder refuses to yield")
	}

	// The failed preemption must not leave the slot marked in-transition.
	res.Release()
	lease, err := p.Acquire(context.Background(), "mx")
	if err != nil {
		t.Fatalf("Acquire after holder released: %v", err)
	}
	lease.Release()
}

// Recordings outrank live viewing: with one tuner, a due recording
// takes the adapter and playback drops.
func TestPool_RecordPreemptsLive(t *testing.T) {
	ft := &fakeTuner{makeStream: emptyHold}
	p := newPool(t, 1, ft)

	live, err := p.Acquire(context.Background(), "mx")
	if err != nil {
		t.Fatalf("live Acquire: %v", err)
	}

	rec, err := p.AcquireAt(context.Background(), "nhk", PrioRecord)
	if err != nil {
		t.Fatalf("record Acquire should have preempted live, got %v", err)
	}
	defer rec.Release()

	select {
	case <-live.Preempted():
	case <-time.After(2 * time.Second):
		t.Fatal("live lease was never marked preempted")
	}
	// The evicted viewer's stream ends rather than hanging.
	select {
	case _, ok := <-live.Sub.Ch:
		if ok {
			t.Fatal("expected the evicted subscriber's channel to be closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("evicted subscriber channel stayed open")
	}
	live.Release()

	if st := p.Status(); st[0].Channel != "nhk" || st[0].Prio != "record" {
		t.Fatalf("status = %+v, want nhk at record priority", st)
	}
}

// Live does not evict live — otherwise two viewers would fight over the
// frontend forever. The second one is told the tuner is busy.
func TestPool_LiveDoesNotPreemptLive(t *testing.T) {
	ft := &fakeTuner{makeStream: emptyHold}
	p := newPool(t, 1, ft)

	live, err := p.Acquire(context.Background(), "mx")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer live.Release()

	_, err = p.Acquire(context.Background(), "nhk")
	if !errors.Is(err, ErrNoAdapter) {
		t.Fatalf("err = %v, want ErrNoAdapter", err)
	}
}

// EPG must not evict a recording — the priority ladder only goes one way.
func TestPool_BackgroundCannotPreemptRecording(t *testing.T) {
	ft := &fakeTuner{makeStream: emptyHold}
	p := newPool(t, 1, ft)

	rec, err := p.AcquireAt(context.Background(), "mx", PrioRecord)
	if err != nil {
		t.Fatalf("AcquireAt: %v", err)
	}
	defer rec.Release()

	if _, err := p.Reserve(context.Background(), PrioBackground); !errors.Is(err, ErrNoAdapter) {
		t.Fatalf("err = %v, want ErrNoAdapter", err)
	}
}

// A channel being recorded *and* watched keeps record priority: the
// session's effective priority is the highest still held, so releasing
// the live lease must not make it evictable at live priority.
func TestPool_SharedSessionKeepsHighestPriority(t *testing.T) {
	ft := &fakeTuner{makeStream: emptyHold}
	p := newPool(t, 1, ft)

	rec, err := p.AcquireAt(context.Background(), "mx", PrioRecord)
	if err != nil {
		t.Fatalf("record AcquireAt: %v", err)
	}
	defer rec.Release()

	live, err := p.Acquire(context.Background(), "mx")
	if err != nil {
		t.Fatalf("live Acquire on the same channel: %v", err)
	}
	if ft.tuneCount.Load() != 1 {
		t.Fatalf("tuneCount = %d, want 1 (session shared)", ft.tuneCount.Load())
	}
	live.Release()

	if st := p.Status(); st[0].Prio != "record" {
		t.Fatalf("prio = %q after the live lease left, want record", st[0].Prio)
	}
	if _, err := p.AcquireAt(context.Background(), "nhk", PrioLive); !errors.Is(err, ErrNoAdapter) {
		t.Fatalf("err = %v, want ErrNoAdapter (recording must survive)", err)
	}
}

func TestPool_CanServe(t *testing.T) {
	ft := &fakeTuner{makeStream: emptyHold}
	p := newPool(t, 1, ft)

	if !p.CanServe("mx", PrioLive) {
		t.Fatal("idle pool should serve anything")
	}

	live, err := p.Acquire(context.Background(), "mx")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer live.Release()

	// Same channel: shareable. Alias must resolve to the same session.
	if !p.CanServe("mx", PrioLive) || !p.CanServe("TOKYO MX1", PrioLive) {
		t.Fatal("same channel should be shareable")
	}
	// Different channel at equal priority: no.
	if p.CanServe("nhk", PrioLive) {
		t.Fatal("live should not claim to preempt live")
	}
	// Different channel at record priority: yes, by eviction.
	if !p.CanServe("nhk", PrioRecord) {
		t.Fatal("record should be able to preempt live")
	}
}

// Two adapters: EPG on one, live on the other, no eviction needed.
func TestPool_TwoAdaptersNoPreemptionNeeded(t *testing.T) {
	ft := &fakeTuner{makeStream: emptyHold}
	p := newPool(t, 2, ft)

	res, err := p.Reserve(context.Background(), PrioBackground)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	defer res.Release()

	lease, err := p.Acquire(context.Background(), "mx")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer lease.Release()

	if lease.Adapter == res.Adapter {
		t.Fatalf("live and EPG both landed on adapter %d", lease.Adapter)
	}
	select {
	case <-res.Preempted():
		t.Fatal("reservation was preempted despite a free adapter")
	default:
	}
}
