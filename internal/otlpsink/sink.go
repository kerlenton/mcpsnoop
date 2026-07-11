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
}

// Sink correlates observed envelopes and delivers each completed call as one
// OTLP span. Network work stays in a single background goroutine.
type Sink struct {
	endpoint string
	headers  http.Header
	client   *http.Client
	ch       chan proxy.Envelope
	cancel   context.CancelFunc
	done     chan struct{}
	once     sync.Once
	dropped  atomic.Uint64
	minRetry time.Duration
	maxRetry time.Duration
	limit    int
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
	}
	go s.run(ctx)
	return s
}

func (s *Sink) run(ctx context.Context) {
	defer close(s.done)
	pending := make(map[callKey]pendingCall)
	order := list.New()
	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-s.ch:
			if !ok {
				return
			}
			msg, ok := proxy.ParseRPC(env.Raw)
			if !ok {
				continue
			}
			if msg.IsRequest() && len(msg.ID) > 0 {
				s.remember(pending, order, requestKey(env, msg), env)
				continue
			}
			if !msg.IsResponse() {
				continue
			}
			key, ok := responseKey(env, msg)
			if !ok {
				continue
			}
			request, ok := pending[key]
			if !ok {
				continue
			}
			delete(pending, key)
			order.Remove(request.element)
			payload, ok := payloadFor(request.envelope, env)
			if !ok {
				continue
			}
			if !s.deliver(ctx, payload) {
				return
			}
		}
	}
}

type callKey struct {
	session   string
	direction proxy.Direction
	id        string
}

type pendingCall struct {
	envelope proxy.Envelope
	element  *list.Element
}

func requestKey(env proxy.Envelope, msg proxy.RPCMessage) callKey {
	return callKey{session: env.SessionID, direction: env.Direction, id: string(msg.ID)}
}

func responseKey(env proxy.Envelope, msg proxy.RPCMessage) (callKey, bool) {
	var requestDirection proxy.Direction
	switch env.Direction {
	case proxy.ClientToServer:
		requestDirection = proxy.ServerToClient
	case proxy.ServerToClient:
		requestDirection = proxy.ClientToServer
	default:
		return callKey{}, false
	}
	return callKey{session: env.SessionID, direction: requestDirection, id: string(msg.ID)}, true
}

func (s *Sink) remember(pending map[callKey]pendingCall, order *list.List, key callKey, env proxy.Envelope) {
	if previous, ok := pending[key]; ok {
		previous.envelope = env
		pending[key] = previous
		order.MoveToBack(previous.element)
		return
	}
	if len(pending) >= s.limit {
		oldest := order.Front()
		delete(pending, oldest.Value.(callKey))
		order.Remove(oldest)
		s.dropped.Add(1)
	}
	element := order.PushBack(key)
	pending[key] = pendingCall{envelope: env, element: element}
}

func payloadFor(request, response proxy.Envelope) ([]byte, bool) {
	st := store.New(0)
	st.Ingest(request)
	event := st.Ingest(response)
	if event.Call == nil || !event.Call.Done() {
		return nil, false
	}
	data, err := exporter.Build(st, request.SessionID)
	if err != nil {
		return nil, false
	}
	if len(data.Calls) != 1 {
		return nil, false
	}
	var payload bytes.Buffer
	if err := exporter.WriteOTLP(&payload, data); err != nil {
		return nil, false
	}
	return payload.Bytes(), true
}

func (s *Sink) deliver(ctx context.Context, payload []byte) bool {
	backoff := s.minRetry
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(payload))
		if err == nil {
			for name, values := range s.headers {
				for _, value := range values {
					req.Header.Add(name, value)
				}
			}
			req.Header.Set("Content-Type", "application/json")
			var resp *http.Response
			resp, err = s.client.Do(req)
			if err == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
				_ = resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					return true
				}
			}
		}
		if !sleep(ctx, backoff) {
			return false
		}
		backoff = min(backoff*2, s.maxRetry)
	}
}

// Emit queues env without waiting for correlation or network delivery.
func (s *Sink) Emit(env proxy.Envelope) {
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
	s.once.Do(func() { close(s.ch) })
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
