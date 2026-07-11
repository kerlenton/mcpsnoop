package proxy

import (
	"encoding/json"
	"io"
	"sync"
	"sync/atomic"
)

// Sink receives observed envelopes. Implementations MUST be non-blocking and
// MUST NOT propagate errors into the data path. Tracing is best-effort and must
// never slow down or break the real MCP traffic.
type Sink interface {
	Emit(Envelope)
	Close() error
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
	done    chan struct{}
	once    sync.Once
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
	enc := json.NewEncoder(s.w)
	for env := range s.ch {
		_ = enc.Encode(env) // best-effort, a write error must not crash the proxy
	}
}

// Emit queues env, dropping it if the buffer is full.
func (s *AsyncSink) Emit(env Envelope) {
	select {
	case s.ch <- env:
	default:
		s.dropped.Add(1)
	}
}

// Dropped reports how many envelopes were dropped due to a full buffer.
func (s *AsyncSink) Dropped() uint64 { return s.dropped.Load() }

// Close flushes the queue and releases the underlying writer.
func (s *AsyncSink) Close() error {
	s.once.Do(func() { close(s.ch) })
	<-s.done
	if s.closer != nil {
		return s.closer.Close()
	}
	return nil
}
