package proxy

import (
	"io"
	"sync"
	"sync/atomic"

	"github.com/kerlenton/mcpsnoop/internal/jsonwire"
)

// Sink receives observed envelopes. Implementations MUST be non-blocking and
// MUST NOT propagate errors into the data path. Tracing is best-effort and must
// never slow down or break the real MCP traffic.
type Sink interface {
	Emit(Envelope)
	Close() error
}

// DropCounter is an optional interface a Sink may implement to report how many
// envelopes it took and did not record. Callers type-assert for it, so a sink
// that never drops need not implement it. MultiSink totals it across children.
//
// Why not "on a full buffer": that was already narrower than what the sinks did.
// SocketSink also drops when no hub is listening, and both it and AsyncSink drop
// what arrives after Close. A reader who takes the count for a queue-depth
// problem goes looking for load that is not there.
type DropCounter interface {
	Dropped() uint64
}

// nopSink discards everything. Used when tracing is disabled.
type nopSink struct{}

// NopSink returns a Sink that drops all envelopes.
func NopSink() Sink           { return nopSink{} }
func (nopSink) Emit(Envelope) {}
func (nopSink) Close() error  { return nil }

// AsyncSink writes envelopes as newline-delimited JSON to an io.Writer from a
// single background goroutine. The channel is buffered and drops on overflow, so
// a slow or blocked writer can never back-pressure the proxied stream.
type AsyncSink struct {
	w       io.Writer
	closer  io.Closer
	ch      chan Envelope
	quit    chan struct{}
	done    chan struct{}
	once    sync.Once
	mu      sync.RWMutex
	closed  bool
	dropped atomic.Uint64
}

// NewAsyncSink writes envelopes to w. If w also implements io.Closer it is
// closed on Close. buffer is the queue depth before envelopes start dropping.
func NewAsyncSink(w io.Writer, buffer int) *AsyncSink {
	if buffer <= 0 {
		buffer = 4096
	}
	s := &AsyncSink{
		w:    w,
		ch:   make(chan Envelope, buffer),
		quit: make(chan struct{}),
		done: make(chan struct{}),
	}
	if c, ok := w.(io.Closer); ok {
		s.closer = c
	}
	go s.loop()
	return s
}

func (s *AsyncSink) loop() {
	defer close(s.done)
	enc := jsonwire.NewEncoder(s.w)
	for {
		select {
		case env := <-s.ch:
			_ = enc.Encode(env) // best-effort, a write error must not crash the proxy
		case <-s.quit:
			// Drain what is already queued, then stop. s.ch is deliberately never
			// closed, so a proxy goroutine still emitting during shutdown (e.g. an SSE
			// tap draining after the HTTP grace period) drops into Emit's default case
			// instead of panicking on a send to a closed channel (mirrors SocketSink).
			for {
				select {
				case env := <-s.ch:
					_ = enc.Encode(env)
				default:
					return
				}
			}
		}
	}
}

// Emit queues env, dropping it if the sink is closed or the buffer is full.
//
// The lock pairs with Close, which takes it for writing before it sets closed.
// Without it the channel outlives the goroutine draining it, so a late emit
// queued an envelope nobody would ever read and the count said nothing was lost.
// Holding it across the send is what makes that exact rather than merely
// unlikely: Close cannot publish closed while a send is in flight, and no send
// starts once it has.
//
// TryRLock rather than RLock, which is what internal/otlpsink uses for the same
// shape. This sink sits on the proxied path, where the interface contract above
// forbids waiting, and RWMutex parks a reader as soon as a writer is pending. A
// failure here is only ever Close holding that write lock, so the envelope is
// counted for the same reason the closed branch counts one, just observed a few
// nanoseconds earlier.
func (s *AsyncSink) Emit(env Envelope) {
	if !s.mu.TryRLock() {
		s.dropped.Add(1)
		return
	}
	defer s.mu.RUnlock()
	if s.closed {
		s.dropped.Add(1)
		return
	}

	select {
	case s.ch <- env:
	default:
		s.dropped.Add(1)
	}
}

// Dropped reports how many envelopes were taken and not recorded, whether the
// buffer was full or the sink was already closed.
func (s *AsyncSink) Dropped() uint64 { return s.dropped.Load() }

// Close flushes the queue and releases the underlying writer. It signals the
// loop via quit rather than closing s.ch, so a late Emit after Close drops
// instead of panicking.
func (s *AsyncSink) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.quit)
		s.mu.Unlock()
	})
	<-s.done
	if s.closer != nil {
		return s.closer.Close()
	}
	return nil
}
