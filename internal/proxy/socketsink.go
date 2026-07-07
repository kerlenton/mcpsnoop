package proxy

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// MultiSink fans an envelope out to several sinks. The shim uses it to write the
// durable JSONL file AND stream to the hub at the same time.
type MultiSink struct {
	sinks []Sink
}

// NewMultiSink returns a Sink that forwards to all of sinks.
func NewMultiSink(sinks ...Sink) *MultiSink { return &MultiSink{sinks: sinks} }

func (m *MultiSink) Emit(e Envelope) {
	for _, s := range m.sinks {
		s.Emit(e)
	}
}

func (m *MultiSink) Close() error {
	var first error
	for _, s := range m.sinks {
		if err := s.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// SocketSink streams envelopes to the hub over a unix socket. It is best-effort
// by design: if the hub isn't running yet it keeps retrying in the background
// (the client spawns the shim whenever it likes; the user opens the TUI whenever
// they like — neither must start first), and it drops frames rather than ever
// blocking the proxied stream. Durability is the file sink's job; this one only
// powers the live view.
type SocketSink struct {
	addr    string
	ch      chan Envelope
	cancel  context.CancelFunc
	done    chan struct{}
	once    sync.Once
	dropped atomic.Uint64
	dial    func(context.Context, string) (net.Conn, error)
}

// NewSocketSink dials addr (a unix socket path) lazily and keeps it connected.
func NewSocketSink(addr string, buffer int) *SocketSink {
	return newSocketSink(addr, buffer, func(ctx context.Context, addr string) (net.Conn, error) {
		return defaultDial(ctx, "unix", addr)
	})
}

func (s *SocketSink) run(ctx context.Context) {
	defer close(s.done)
	const minBackoff, maxBackoff = 200 * time.Millisecond, 2 * time.Second
	backoff := minBackoff
	for {
		if ctx.Err() != nil {
			return
		}
		conn, err := s.dial(ctx, s.addr)
		if err != nil {
			if !sleep(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		backoff = minBackoff
		s.pump(ctx, conn)
	}
}

// pump encodes envelopes to conn until a write fails or ctx is cancelled.
func (s *SocketSink) pump(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	enc := json.NewEncoder(conn)
	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-s.ch:
			if !ok {
				return
			}
			if err := enc.Encode(env); err != nil {
				return // hub went away; outer loop will redial
			}
		}
	}
}

func (s *SocketSink) Emit(env Envelope) {
	select {
	case s.ch <- env:
	default:
		s.dropped.Add(1)
	}
}

// Dropped reports how many envelopes were dropped (buffer full or hub absent).
func (s *SocketSink) Dropped() uint64 { return s.dropped.Load() }

func (s *SocketSink) Close() error {
	s.once.Do(func() {
		s.cancel()
		close(s.ch)
	})
	<-s.done
	return nil
}
