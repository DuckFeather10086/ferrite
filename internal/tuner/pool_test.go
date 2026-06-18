package tuner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DuckFeather10086/ferrite/internal/config"
)

// fakeTuner returns scripted TsStreams instead of spawning dvbr.
// Each Tune call increments a counter so tests can assert on
// share-vs-respawn behavior.
type fakeTuner struct {
	tuneCount  atomic.Int32
	makeStream func(ctx context.Context, adapter int, channel string) TsStream
}

func (f *fakeTuner) Tune(ctx context.Context, adapter int, channel string) (TsStream, error) {
	f.tuneCount.Add(1)
	return f.makeStream(ctx, adapter, channel), nil
}

// holdStream serves a body then blocks until ctx is canceled
// (Pool.release path) or Close is called (Pump's defer path). Either
// unblock returns io.EOF — what a real EOF on stdout looks like.
type holdStream struct {
	ctx  context.Context
	body []byte
	off  int
	done chan struct{}
	once sync.Once
}

func newHoldStream(ctx context.Context, body []byte) *holdStream {
	return &holdStream{ctx: ctx, body: body, done: make(chan struct{})}
}

func (h *holdStream) Read(p []byte) (int, error) {
	if h.off < len(h.body) {
		n := copy(p, h.body[h.off:])
		h.off += n
		return n, nil
	}
	select {
	case <-h.ctx.Done():
	case <-h.done:
	}
	return 0, io.EOF
}

func (h *holdStream) Close() error {
	h.once.Do(func() { close(h.done) })
	return nil
}

func newPool(t *testing.T, tunerCount int, ft *fakeTuner) *Pool {
	t.Helper()
	channels := &config.Channels{
		Version: 1,
		Channels: []config.Channel{
			{Name: "mx", Aliases: []string{"TOKYO MX1"},
				Tuning: map[string]string{"SERVICE_ID": "23608"}},
			{Name: "nhk", Tuning: map[string]string{"SERVICE_ID": "1024"}},
			{Name: "tbs", Tuning: map[string]string{"SERVICE_ID": "2048"}},
		},
	}
	adapters := make([]int, tunerCount)
	for i := range adapters {
		adapters[i] = i
	}
	return NewPool(ft, channels, adapters, 4)
}

func emptyHold(ctx context.Context, _ int, _ string) TsStream {
	return newHoldStream(ctx, nil)
}

func TestPool_UnknownChannel(t *testing.T) {
	ft := &fakeTuner{makeStream: emptyHold}
	p := newPool(t, 1, ft)
	_, err := p.Acquire(context.Background(), "nope")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("got %v", err)
	}
}

func TestPool_AliasResolves(t *testing.T) {
	ft := &fakeTuner{makeStream: emptyHold}
	p := newPool(t, 1, ft)
	l, err := p.Acquire(context.Background(), "TOKYO MX1")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if l.Channel != "mx" {
		t.Fatalf("alias not canonicalized: got %s", l.Channel)
	}
}

func TestPool_SameChannelSharesOneTune(t *testing.T) {
	ft := &fakeTuner{makeStream: func(ctx context.Context, _ int, _ string) TsStream {
		return newHoldStream(ctx, bytes.Repeat([]byte{0x47}, 1<<16))
	}}
	p := newPool(t, 2, ft)

	l1, err := p.Acquire(context.Background(), "mx")
	if err != nil {
		t.Fatal(err)
	}
	defer l1.Release()

	l2, err := p.Acquire(context.Background(), "mx")
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Release()

	if ft.tuneCount.Load() != 1 {
		t.Fatalf("expected 1 tune for shared channel, got %d", ft.tuneCount.Load())
	}
	if l1.Adapter != l2.Adapter {
		t.Fatalf("shared session should pin to one adapter, got %d vs %d",
			l1.Adapter, l2.Adapter)
	}
}

func TestPool_DifferentChannelsUseDifferentAdapters(t *testing.T) {
	ft := &fakeTuner{makeStream: emptyHold}
	p := newPool(t, 2, ft)

	l1, _ := p.Acquire(context.Background(), "mx")
	l2, _ := p.Acquire(context.Background(), "nhk")
	defer l1.Release()
	defer l2.Release()

	if l1.Adapter == l2.Adapter {
		t.Fatalf("different channels picked same adapter %d", l1.Adapter)
	}
	if ft.tuneCount.Load() != 2 {
		t.Fatalf("expected 2 tunes, got %d", ft.tuneCount.Load())
	}
}

func TestPool_NoAdapterAvailable(t *testing.T) {
	ft := &fakeTuner{makeStream: emptyHold}
	p := newPool(t, 1, ft)
	l, err := p.Acquire(context.Background(), "mx")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()

	_, err = p.Acquire(context.Background(), "nhk")
	if err == nil || !strings.Contains(err.Error(), "no adapter") {
		t.Fatalf("got %v", err)
	}
}

func TestPool_ReleaseReturnsAdapterAfterLast(t *testing.T) {
	ft := &fakeTuner{makeStream: emptyHold}
	p := newPool(t, 1, ft)

	l1, _ := p.Acquire(context.Background(), "mx")
	l2, _ := p.Acquire(context.Background(), "mx")
	l1.Release()
	// Still one lease on adapter 0 → cannot tune nhk.
	if _, err := p.Acquire(context.Background(), "nhk"); err == nil {
		t.Fatal("expected no-adapter-available while a lease still held")
	}
	l2.Release()
	// Now the adapter is free.
	l3, err := p.Acquire(context.Background(), "nhk")
	if err != nil {
		t.Fatalf("after final release, Acquire failed: %v", err)
	}
	defer l3.Release()
}

func TestPool_SourceEOFFreesAdapter(t *testing.T) {
	// Stream that returns its body and then EOF immediately.
	ft := &fakeTuner{makeStream: func(_ context.Context, _ int, _ string) TsStream {
		return &eofStream{body: bytes.Repeat([]byte{0x47}, 1024)}
	}}
	p := newPool(t, 1, ft)

	l, err := p.Acquire(context.Background(), "mx")
	if err != nil {
		t.Fatal(err)
	}
	// Drain the sub so the broadcaster's Pump can complete and clear
	// the slot.
	for c := range l.Sub.Ch {
		c.Release()
	}

	// Give the pump's defer a beat to clear the slot.
	deadline := time.Now().Add(2 * time.Second)
	for {
		status := p.Status()
		if status[0].Channel == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot not cleared after source EOF: %+v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Adapter is reusable for a different channel even though l
	// is technically still un-Released.
	l2, err := p.Acquire(context.Background(), "nhk")
	if err != nil {
		t.Fatalf("Acquire after source EOF failed: %v", err)
	}
	defer l2.Release()

	// Release of the original is still safe / idempotent.
	l.Release()
	l.Release()
}

func TestPool_TuneErrorClearsSlot(t *testing.T) {
	pool := NewPool(failingTuner{}, &config.Channels{
		Version: 1,
		Channels: []config.Channel{{Name: "mx",
			Tuning: map[string]string{"SERVICE_ID": "1"}}},
	}, []int{0}, 4)

	_, err := pool.Acquire(context.Background(), "mx")
	if err == nil {
		t.Fatal("expected tune failure")
	}
	// Slot should be cleared so a retry can use the adapter.
	st := pool.Status()
	if st[0].Channel != "" {
		t.Fatalf("slot not cleared after tune failure: %+v", st)
	}
}

func TestPool_StatusReportsRefsAndChannel(t *testing.T) {
	ft := &fakeTuner{makeStream: emptyHold}
	p := newPool(t, 2, ft)
	l1, _ := p.Acquire(context.Background(), "mx")
	defer l1.Release()
	l2, _ := p.Acquire(context.Background(), "mx")
	defer l2.Release()

	st := p.Status()
	var mxRefs int
	for _, a := range st {
		if a.Channel == "mx" {
			mxRefs = a.Refs
		}
	}
	if mxRefs != 2 {
		t.Fatalf("expected 2 refs on mx, got %d (full status: %+v)", mxRefs, st)
	}
}

// ── helpers ────────────────────────────────────────────────────────

type eofStream struct {
	body []byte
	off  int
}

func (e *eofStream) Read(p []byte) (int, error) {
	if e.off >= len(e.body) {
		return 0, io.EOF
	}
	n := copy(p, e.body[e.off:])
	e.off += n
	return n, nil
}
func (e *eofStream) Close() error { return nil }

type failingTuner struct{}

func (failingTuner) Tune(context.Context, int, string) (TsStream, error) {
	return nil, errors.New("tune boom")
}
