// Package fanout turns one io.Reader of TS bytes into N subscribers.
//
// Design constraint: a slow subscriber must never block the source or
// the other subscribers. Policy: drop chunks for any subscriber whose
// buffer is full. Recording must not starve live HLS, and vice versa.
//
// Chunks are sized to match dvbr's stdout block (188 * 512 = 96 KiB)
// to keep per-chunk overhead amortized. Buffers are pooled to keep
// GC pressure flat under sustained streaming.
package fanout

import (
	"io"
	"sync"
	"sync/atomic"
)

// Chunk is a refcounted slice borrowed from the broadcaster's pool.
// Subscribers MUST call Release when they're done with it.
type Chunk struct {
	Data []byte
	Seq  uint64
	refs atomic.Int32
	pool *sync.Pool
}

func (c *Chunk) Release() {
	if c.refs.Add(-1) == 0 && c.pool != nil {
		c.pool.Put(c.Data[:cap(c.Data)])
	}
}

// Sub is a single subscription to a Broadcaster.
type Sub struct {
	Ch      <-chan *Chunk
	Dropped *atomic.Uint64
}

// Broadcaster fans out chunks from a single io.Reader to N Subs.
type Broadcaster struct {
	mu   sync.RWMutex
	subs map[*Sub]chan *Chunk
	pool sync.Pool
	seq  atomic.Uint64
}

// New returns a ready broadcaster. Call Pump to feed it.
func New() *Broadcaster {
	panic("not implemented")
}

// Subscribe registers a new subscriber. bufChunks bounds the per-sub
// queue; a full queue causes the broadcaster to drop the next chunk
// for that sub and increment Sub.Dropped.
func (b *Broadcaster) Subscribe(bufChunks int) *Sub {
	panic("not implemented")
}

// Unsubscribe removes the sub and closes its channel.
func (b *Broadcaster) Unsubscribe(s *Sub) {
	panic("not implemented")
}

// Pump reads from r and broadcasts until EOF or error. Returns when
// the source closes; subscribers see their channels closed.
func (b *Broadcaster) Pump(r io.Reader) error {
	panic("not implemented")
}
