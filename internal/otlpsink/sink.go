// Package otlpsink streams completed MCP calls to an OTLP/HTTP collector.
package otlpsink

import (
	"bytes"
	"container/list"
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/exporter"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

// Config controls live OTLP delivery.
type Config struct {
	Endpoint   string
	Headers    http.Header
	Buffer     int
	Client     *http.Client
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// CancelGrace is how long a cancelled call waits for an answer that was
	// already on its way before it goes out without one. Two seconds when unset.
	CancelGrace time.Duration
}

// Sink delivers each finished MCP operation to a collector the moment the
// response that finishes it is observed. An operation that took one request is
// one span. One that took several, which multi round-trip requests make
// ordinary, is a span per request under an execute_tool span, built by the same
// code as an export so the two can never describe it differently. Network work
// stays in a single background goroutine.
type Sink struct {
	endpoint string
	headers  http.Header
	client   *http.Client
	ch       chan proxy.Envelope
	cancel   context.CancelFunc
	done     chan struct{}
	mu       sync.RWMutex
	closed   bool
	dropped  atomic.Uint64
	minRetry time.Duration
	maxRetry time.Duration
	limit    int
	grace    time.Duration
}

// New starts a live OTLP sink. The queue is bounded and Emit drops on overflow.
func New(cfg Config) *Sink {
	if cfg.Buffer <= 0 {
		cfg.Buffer = 4096
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = 200 * time.Millisecond
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 2 * time.Second
	}
	if cfg.MaxBackoff < cfg.MinBackoff {
		cfg.MaxBackoff = cfg.MinBackoff
	}
	if cfg.CancelGrace <= 0 {
		cfg.CancelGrace = 2 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Sink{
		endpoint: cfg.Endpoint,
		headers:  cfg.Headers.Clone(),
		client:   cfg.Client,
		ch:       make(chan proxy.Envelope, cfg.Buffer),
		cancel:   cancel,
		done:     make(chan struct{}),
		minRetry: cfg.MinBackoff,
		maxRetry: cfg.MaxBackoff,
		limit:    cfg.Buffer,
		grace:    cfg.CancelGrace,
	}
	go s.run(ctx)
	return s
}

// The store the sink correlates with. It holds open operations however long a
// person takes to answer one, and releases settled frames past these bounds.
// The frame bound is far above what passes during an ordinary answer. Past it,
// an open operation can lose its first hop, and the operation then goes out as
// one span with an exact server and client split rather than as a partial
// breakdown passed off as the whole.
const (
	sinkBodyBytes = 8 << 20
	sinkFrames    = 8192
)

func (s *Sink) run(ctx context.Context) {
	defer close(s.done)
	// One store for the life of the sink, so the store links the requests of a
	// multi round-trip operation the way it does everywhere else. A fresh store
	// per request and response pair could not, which sent a retry out alone with
	// its own short duration and never sent the request the server answered by
	// asking a person something.
	st := store.NewBounded(sinkBodyBytes, sinkFrames)
	posted := newRecent(s.limit)
	awaiting := make(map[operationKey]awaitingCancel)
	tick := min(max(s.grace/4, 10*time.Millisecond), 500*time.Millisecond)
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			for key, a := range awaiting {
				if now.Before(a.due) {
					continue
				}
				delete(awaiting, key)
				if !s.send(ctx, st, posted, key, a.op) {
					return
				}
			}
		case env, ok := <-s.ch:
			if !ok {
				// Shutting down, so no answer is coming for anything still waiting.
				for key, a := range awaiting {
					delete(awaiting, key)
					if !s.send(ctx, st, posted, key, a.op) {
						return
					}
				}
				return
			}
			ev := st.Ingest(env)
			if ev.Call == nil || !ev.Call.Done() {
				continue
			}
			key := operationKey{session: env.SessionID, seq: ev.Call.RequestSeq}
			if ev.Kind == store.EventResponse {
				delete(awaiting, key)
				if !s.send(ctx, st, posted, key, *ev.Call) {
					return
				}
				continue
			}
			// Settled without an answer, which is how a cancelled call ends, since a
			// server receiving the cancellation SHOULD not send a response. The
			// specification also has both sides handle a response that was already in
			// flight when the cancellation went out, so the call waits a moment for
			// one before it goes without. Waiting for an answer indefinitely would
			// mean the most common cancellation, and the one a client sends when a
			// request times out, never reached the collector at all.
			if ev.Call.State != store.Cancelled {
				continue
			}
			if _, waiting := awaiting[key]; waiting || posted.has(key) {
				continue
			}
			if len(awaiting) >= s.limit {
				if !s.send(ctx, st, posted, key, *ev.Call) {
					return
				}
				continue
			}
			awaiting[key] = awaitingCancel{op: *ev.Call, due: time.Now().Add(s.grace)}
		}
	}
}

// awaitingCancel is a cancelled call held for an answer already on its way.
type awaitingCancel struct {
	op  store.CallView
	due time.Time
}

// send delivers one operation, once. It reports whether run should keep going.
func (s *Sink) send(ctx context.Context, st *store.Store, posted *recent, key operationKey, op store.CallView) bool {
	if !posted.add(key) {
		return true // a duplicate or late response for one already sent
	}
	var payload bytes.Buffer
	if err := exporter.OperationOTLP(&payload, st, key.session, op); err != nil {
		return true
	}
	return s.deliver(ctx, payload.Bytes())
}

// operationKey names one operation across every session a sink carries.
type operationKey struct {
	session string
	seq     uint64
}

// recent is a bounded set of the operations already delivered, oldest forgotten
// first, so a duplicate response cannot send one twice.
type recent struct {
	limit int
	seen  map[operationKey]*list.Element
	order *list.List
}

func newRecent(limit int) *recent {
	return &recent{limit: limit, seen: make(map[operationKey]*list.Element), order: list.New()}
}

func (r *recent) has(key operationKey) bool {
	_, ok := r.seen[key]
	return ok
}

// add reports whether key is new, and remembers it.
func (r *recent) add(key operationKey) bool {
	if _, ok := r.seen[key]; ok {
		return false
	}
	if len(r.seen) >= r.limit {
		oldest := r.order.Front()
		delete(r.seen, oldest.Value.(operationKey))
		r.order.Remove(oldest)
	}
	r.seen[key] = r.order.PushBack(key)
	return true
}

// maxDeliveryAttempts bounds the retries of a single payload, so a persistently
// failing collector drops the current span and advances to newer ones instead of
// blocking delivery forever.
const maxDeliveryAttempts = 5

// deliver POSTs payload, retrying with backoff up to maxDeliveryAttempts. It
// reports whether run should keep going: true after a success or after giving up
// on the payload, and false only when ctx is cancelled (shutdown).
func (s *Sink) deliver(ctx context.Context, payload []byte) bool {
	backoff := s.minRetry
	for attempt := range maxDeliveryAttempts {
		if s.post(ctx, payload) {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		if attempt == maxDeliveryAttempts-1 {
			break
		}
		if !sleep(ctx, backoff) {
			return false
		}
		backoff = min(backoff*2, s.maxRetry)
	}
	s.dropped.Add(1) // exhausted the retry budget, drop this span and move on
	return true
}

// post makes one delivery attempt and reports a 2xx success.
func (s *Sink) post(ctx context.Context, payload []byte) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(payload))
	if err != nil {
		return false
	}
	for name, values := range s.headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// Emit queues env without waiting for correlation or network delivery.
func (s *Sink) Emit(env proxy.Envelope) {
	// The RLock pairs with Close taking the write lock before it closes s.ch, so a
	// late emit during shutdown (e.g. an SSE tap draining after the HTTP server's
	// grace period) drops instead of panicking on a send to a closed channel.
	s.mu.RLock()
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

// Dropped reports how many envelopes were dropped because the queue was full.
func (s *Sink) Dropped() uint64 { return s.dropped.Load() }

// Close stops delivery promptly, including an in-flight HTTP request.
func (s *Sink) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.ch)
	s.mu.Unlock()

	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-s.done:
		s.cancel()
	case <-timer.C:
		s.cancel()
		<-s.done
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
