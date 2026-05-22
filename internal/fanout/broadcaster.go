// Package fanout turns one io.Reader of TS bytes into N subscribers.
//
// Policy: a slow subscriber must never block the source or its
// siblings. When a subscriber's per-sub buffer is full, the next
// chunk is dropped for that subscriber (and Sub.Dropped increments).
// Recording must not starve live HLS, and vice versa.
//
// Chunks are sized to match dvbr's stdout block (188 * 512 = 96 KiB)
// and are returned to a sync.Pool when their refcount hits zero, so
// sustained streaming has near-zero allocation pressure.
package fanout

import (
	"io"
	"sync"
	"sync/atomic"
)

// ChunkSize is the broadcaster's preferred read size. Matches dvbr's
// BUF constant so a single Read pulls one whole dvbr emit.
const ChunkSize = 188 * 512

// Chunk is a refcounted slice borrowed from the broadcaster's pool.
// Subscribers MUST call Release exactly once when done with each
// chunk they receive. Failing to Release leaks the buffer from the
// pool (GC will still collect it eventually, but throughput suffers).
type Chunk struct {
	Data []byte
	Seq  uint64
	refs atomic.Int32
	pool *sync.Pool
}

// Release decrements the refcount and returns the underlying buffer
// to the pool when the count reaches zero. Safe to call from any
// goroutine.
func (c *Chunk) Release() {
	if c.refs.Add(-1) == 0 && c.pool != nil {
		// Restore to full capacity before pooling.
		buf := c.Data[:cap(c.Data)]
		c.Data = nil
		c.pool.Put(buf)
	}
}

// Sub is a single subscription to a Broadcaster.
type Sub struct {
	// Ch delivers chunks until the broadcaster closes it (either via
	// Unsubscribe or because Pump returned).
	Ch <-chan *Chunk

	// Dropped counts chunks the broadcaster discarded because Ch was
	// full when it tried to deliver. Useful for /api/status.
	Dropped *atomic.Uint64

	ch chan *Chunk // write side, owned by the broadcaster
}

// Broadcaster fans out chunks from a single io.Reader to N Subs.
//
// One Pump goroutine per broadcaster is supported. Calling Pump
// concurrently is undefined.
type Broadcaster struct {
	mu      sync.RWMutex
	subs    map[*Sub]struct{}
	pool    sync.Pool
	seq     atomic.Uint64
	closed  bool
}

// New returns a ready broadcaster.
func New() *Broadcaster {
	b := &Broadcaster{
		subs: make(map[*Sub]struct{}),
	}
	b.pool.New = func() any {
		return make([]byte, ChunkSize)
	}
	return b
}

// Subscribe registers a new subscriber. bufChunks bounds the per-sub
// channel; chunks dropped due to a full buffer increment Sub.Dropped.
//
// A bufChunks of 8 (≈ 768 KiB held per sub) is a sensible default
// for ISDB-T workloads.
func (b *Broadcaster) Subscribe(bufChunks int) *Sub {
	if bufChunks < 1 {
		bufChunks = 1
	}
	ch := make(chan *Chunk, bufChunks)
	s := &Sub{
		Ch:      ch,
		Dropped: new(atomic.Uint64),
		ch:      ch,
	}
	b.mu.Lock()
	if b.closed {
		// Broadcaster already finished pumping; deliver a closed
		// channel so the caller's range loop exits immediately.
		close(ch)
	} else {
		b.subs[s] = struct{}{}
	}
	b.mu.Unlock()
	return s
}

// Unsubscribe removes s and closes its channel. Safe to call multiple
// times.
func (b *Broadcaster) Unsubscribe(s *Sub) {
	b.mu.Lock()
	if _, ok := b.subs[s]; !ok {
		b.mu.Unlock()
		return
	}
	delete(b.subs, s)
	close(s.ch)
	b.mu.Unlock()
}

// Pump reads from r and broadcasts until EOF or read error. When it
// returns, every subscriber's channel is closed.
func (b *Broadcaster) Pump(r io.Reader) error {
	defer b.closeAll()

	for {
		buf := b.pool.Get().([]byte)
		buf = buf[:cap(buf)]
		n, err := r.Read(buf)
		if n > 0 {
			b.broadcast(buf[:n])
		} else {
			// Nothing to send; return the unused buffer.
			b.pool.Put(buf[:cap(buf)])
		}

		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (b *Broadcaster) broadcast(data []byte) {
	b.mu.RLock()
	n := len(b.subs)
	if n == 0 {
		b.mu.RUnlock()
		b.pool.Put(data[:cap(data)])
		return
	}

	seq := b.seq.Add(1)
	chunk := &Chunk{
		Data: data,
		Seq:  seq,
		pool: &b.pool,
	}
	chunk.refs.Store(int32(n))

	for s := range b.subs {
		select {
		case s.ch <- chunk:
		default:
			s.Dropped.Add(1)
			chunk.Release()
		}
	}
	b.mu.RUnlock()
}

func (b *Broadcaster) closeAll() {
	b.mu.Lock()
	b.closed = true
	for s := range b.subs {
		close(s.ch)
	}
	b.subs = nil
	b.mu.Unlock()
}
