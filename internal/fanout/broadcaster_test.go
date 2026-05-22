package fanout

import (
	"bytes"
	"crypto/rand"
	"io"
	"sync"
	"testing"
	"time"
)

func TestBroadcaster_NoSubscribersDoesNotBlock(t *testing.T) {
	b := New()
	src := bytes.NewReader(bytes.Repeat([]byte{0x47}, ChunkSize*4))
	if err := b.Pump(src); err != nil {
		t.Fatalf("Pump: %v", err)
	}
}

func TestBroadcaster_SingleSubscriberReceivesEverything(t *testing.T) {
	b := New()
	sub := b.Subscribe(16)

	payload := randBytes(ChunkSize * 3)
	src := bytes.NewReader(payload)

	done := make(chan struct{})
	var got bytes.Buffer
	go func() {
		for c := range sub.Ch {
			got.Write(c.Data)
			c.Release()
		}
		close(done)
	}()

	if err := b.Pump(src); err != nil {
		t.Fatalf("Pump: %v", err)
	}
	<-done

	if !bytes.Equal(got.Bytes(), payload) {
		t.Fatalf("subscriber missed bytes: got %d, want %d", got.Len(), len(payload))
	}
	if d := sub.Dropped.Load(); d != 0 {
		t.Fatalf("expected 0 drops, got %d", d)
	}
}

func TestBroadcaster_MultipleSubscribersAllReceiveSameStream(t *testing.T) {
	b := New()
	const nSubs = 4
	subs := make([]*Sub, nSubs)
	buffers := make([]*bytes.Buffer, nSubs)
	for i := range subs {
		subs[i] = b.Subscribe(32)
		buffers[i] = &bytes.Buffer{}
	}

	var wg sync.WaitGroup
	for i, s := range subs {
		wg.Add(1)
		go func(i int, s *Sub) {
			defer wg.Done()
			for c := range s.Ch {
				buffers[i].Write(c.Data)
				c.Release()
			}
		}(i, s)
	}

	payload := randBytes(ChunkSize * 8)
	if err := b.Pump(bytes.NewReader(payload)); err != nil {
		t.Fatalf("Pump: %v", err)
	}
	wg.Wait()

	for i, buf := range buffers {
		if !bytes.Equal(buf.Bytes(), payload) {
			t.Fatalf("sub %d: got %d bytes, want %d", i, buf.Len(), len(payload))
		}
	}
}

func TestBroadcaster_SlowSubscriberDropsButDoesNotBlockFastOne(t *testing.T) {
	b := New()
	fast := b.Subscribe(64)
	slow := b.Subscribe(2) // tiny buffer; will drop

	var fastReceived int
	fastDone := make(chan struct{})
	go func() {
		for c := range fast.Ch {
			fastReceived += len(c.Data)
			c.Release()
		}
		close(fastDone)
	}()

	// Slow consumer: never reads. Its channel will fill and chunks
	// destined for it will be dropped.

	src := bytes.NewReader(randBytes(ChunkSize * 100))
	if err := b.Pump(src); err != nil {
		t.Fatalf("Pump: %v", err)
	}
	<-fastDone

	if fastReceived == 0 {
		t.Fatal("fast subscriber received nothing")
	}
	// Slow sub should have dropped a lot. It buffered 2 chunks.
	dropped := slow.Dropped.Load()
	if dropped < 90 {
		t.Fatalf("slow sub dropped only %d; expected most of 100", dropped)
	}

	// Drain whatever made it into slow's buffer so refcounts settle.
	go func() {
		for c := range slow.Ch {
			c.Release()
		}
	}()
}

func TestBroadcaster_UnsubscribeClosesChannel(t *testing.T) {
	b := New()
	sub := b.Subscribe(4)
	b.Unsubscribe(sub)

	// Reading a closed channel returns immediately.
	select {
	case _, ok := <-sub.Ch:
		if ok {
			t.Fatal("channel should be closed")
		}
	case <-time.After(time.Second):
		t.Fatal("Unsubscribe did not close channel")
	}

	// Idempotent.
	b.Unsubscribe(sub)
}

func TestBroadcaster_SubscribeAfterCloseDeliversClosedChannel(t *testing.T) {
	b := New()
	if err := b.Pump(bytes.NewReader(nil)); err != nil {
		t.Fatalf("Pump: %v", err)
	}

	sub := b.Subscribe(4)
	select {
	case _, ok := <-sub.Ch:
		if ok {
			t.Fatal("expected closed channel")
		}
	case <-time.After(time.Second):
		t.Fatal("late Subscribe did not get closed channel")
	}
}

func TestBroadcaster_PumpPropagatesReadError(t *testing.T) {
	b := New()
	sub := b.Subscribe(4)
	go func() {
		for c := range sub.Ch {
			c.Release()
		}
	}()

	err := b.Pump(errReader{io.ErrClosedPipe})
	if err != io.ErrClosedPipe {
		t.Fatalf("got %v, want ErrClosedPipe", err)
	}
}

type errReader struct{ err error }

func (e errReader) Read(p []byte) (int, error) { return 0, e.err }

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// Sanity check: confirm pool reuse is happening. After heavy
// streaming, the broadcaster's seq counter should be high but
// allocation pressure (judged indirectly by completion under a
// tight memory budget) should stay flat. We don't assert on
// allocations directly; this test mainly guards against deadlocks
// at scale.
func TestBroadcaster_HighThroughputDoesNotDeadlock(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	b := New()
	const nSubs = 3
	subs := make([]*Sub, nSubs)
	for i := range subs {
		subs[i] = b.Subscribe(8)
	}
	var wg sync.WaitGroup
	for _, s := range subs {
		wg.Add(1)
		go func(s *Sub) {
			defer wg.Done()
			for c := range s.Ch {
				c.Release()
			}
		}(s)
	}

	src := io.LimitReader(zeroes{}, int64(ChunkSize)*5000)
	if err := b.Pump(src); err != nil {
		t.Fatalf("Pump: %v", err)
	}
	wg.Wait()
}

type zeroes struct{}

func (zeroes) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
