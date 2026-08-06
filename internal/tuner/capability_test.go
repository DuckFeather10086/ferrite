package tuner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/DuckFeather10086/ferrite/internal/config"
)

// mixedChannels is one terrestrial and one satellite service, each saying
// so the way channels.json does.
func mixedChannels() *config.Channels {
	return &config.Channels{
		Version: 1,
		Channels: []config.Channel{
			{Name: "mx", Tuning: map[string]string{
				"SERVICE_ID": "23608", "DELIVERY_SYSTEM": "ISDBT"}},
			{Name: "bs1", Tuning: map[string]string{
				"SERVICE_ID": "101", "DELIVERY_SYSTEM": "ISDBS"}},
			// No DELIVERY_SYSTEM at all: a hand-written record, which must
			// stay tunable rather than being filtered out of everything.
			{Name: "mystery", Tuning: map[string]string{"SERVICE_ID": "7"}},
		},
	}
}

// A satellite frontend must not be handed a terrestrial channel. On a
// mixed card (PT3: 2×T + 2×S) picking the wrong half does not fail
// quickly — it waits out the frontend lock timeout and reports a weak
// signal, which sends you up a ladder to look at the aerial.
func TestPool_SatelliteAdapterIsNotPickedForTerrestrial(t *testing.T) {
	ft := &fakeTuner{makeStream: emptyHold}
	p := NewPool(ft, mixedChannels(), []config.Adapter{
		{N: 0, Systems: []string{"ISDBS"}},
		{N: 1, Systems: []string{"ISDBT"}},
	}, 4)

	lease, err := p.Acquire(context.Background(), "mx")
	if err != nil {
		t.Fatalf("Acquire(mx): %v", err)
	}
	defer lease.Release()
	if lease.Adapter != 1 {
		t.Fatalf("terrestrial channel landed on adapter %d, want the ISDBT one (1)",
			lease.Adapter)
	}
}

// And with nothing that can receive it, the answer says so — a different
// error from "busy", because no amount of waiting will help.
func TestPool_NoCapableAdapter(t *testing.T) {
	ft := &fakeTuner{makeStream: emptyHold}
	p := NewPool(ft, mixedChannels(), config.ISDBTAdapters(0), 4)

	_, err := p.Acquire(context.Background(), "bs1")
	if !errors.Is(err, ErrNoCapableAdapter) {
		t.Fatalf("Acquire(bs1) = %v, want ErrNoCapableAdapter", err)
	}
	if errors.Is(err, ErrNoAdapter) {
		t.Fatal("must not also read as ErrNoAdapter: the caller would retry forever")
	}
	// The message has to name both halves of the mismatch, or it is no
	// better than the timeout it replaces.
	if !strings.Contains(err.Error(), "ISDBS") || !strings.Contains(err.Error(), "0:ISDBT") {
		t.Errorf("error should name what was asked for and what is installed: %v", err)
	}
	if p.CanServe("bs1", PrioLive) {
		t.Error("CanServe should reject a channel no frontend can receive")
	}
	if !p.CanServe("mx", PrioLive) {
		t.Error("CanServe should still accept a terrestrial channel")
	}
}

// A terrestrial claim must not evict a satellite tune it cannot replace:
// the viewer loses their picture and the claim fails anyway.
func TestPool_PreemptionRespectsCapability(t *testing.T) {
	ft := &fakeTuner{makeStream: emptyHold}
	p := NewPool(ft, mixedChannels(), []config.Adapter{
		{N: 0, Systems: []string{"ISDBS"}},
	}, 4)

	// Hold the satellite adapter at background priority, which a recording
	// would normally be free to evict.
	held, err := p.AcquireAt(context.Background(), "bs1", PrioBackground)
	if err != nil {
		t.Fatalf("AcquireAt(bs1): %v", err)
	}
	defer held.Release()

	if _, err := p.AcquireAt(context.Background(), "mx", PrioRecord); !errors.Is(err, ErrNoCapableAdapter) {
		t.Fatalf("AcquireAt(mx, record) = %v, want ErrNoCapableAdapter", err)
	}
	if st := p.Status(); st[0].Channel != "bs1" {
		t.Fatalf("the satellite tune should have survived: %+v", st)
	}
}

// A record that does not declare a delivery system is unconstrained, and a
// pool built without capability labels constrains nothing. Both are the
// pre-existing behaviour and both have to keep working.
func TestPool_UndeclaredMatchesAnything(t *testing.T) {
	ft := &fakeTuner{makeStream: emptyHold}

	p := NewPool(ft, mixedChannels(), []config.Adapter{
		{N: 0, Systems: []string{"ISDBS"}},
	}, 4)
	lease, err := p.Acquire(context.Background(), "mystery")
	if err != nil {
		t.Fatalf("a channel with no DELIVERY_SYSTEM should tune anywhere: %v", err)
	}
	lease.Release()

	unlabelled := NewPool(ft, mixedChannels(), []config.Adapter{{N: 0}}, 4)
	lease, err = unlabelled.Acquire(context.Background(), "bs1")
	if err != nil {
		t.Fatalf("an unlabelled adapter should accept anything: %v", err)
	}
	lease.Release()
}

// Reserve takes the system explicitly, since its caller tunes the adapter
// itself and the Pool cannot see what for.
func TestPool_ReserveFiltersByCapability(t *testing.T) {
	ft := &fakeTuner{makeStream: emptyHold}
	p := NewPool(ft, mixedChannels(), []config.Adapter{
		{N: 0, Systems: []string{"ISDBS"}},
		{N: 1, Systems: []string{"ISDBT"}},
	}, 4)

	res, err := p.Reserve(context.Background(), PrioBackground, "ISDBT")
	if err != nil {
		t.Fatalf("Reserve(ISDBT): %v", err)
	}
	if res.Adapter != 1 {
		t.Fatalf("reserved adapter %d, want the ISDBT one (1)", res.Adapter)
	}
	res.Release()

	if _, err := p.Reserve(context.Background(), PrioBackground, "DVBT2"); !errors.Is(err, ErrNoCapableAdapter) {
		t.Fatalf("Reserve(DVBT2) = %v, want ErrNoCapableAdapter", err)
	}
}
