package otlpsink

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

func callEnvelopes(session string, id int, started time.Time) (proxy.Envelope, proxy.Envelope) {
	request := proxy.Envelope{
		SessionID:   session,
		ServerLabel: "inventory",
		Seq:         uint64(id*2 - 1),
		TS:          started,
		Direction:   proxy.ClientToServer,
		Raw:         json.RawMessage(`{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"tools/call","params":{"name":"lookup"}}`),
	}
	response := proxy.Envelope{
		SessionID:   session,
		ServerLabel: "inventory",
		Seq:         uint64(id * 2),
		TS:          started.Add(25 * time.Millisecond),
		Direction:   proxy.ServerToClient,
		Raw:         json.RawMessage(`{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"result":{"content":[]}}`),
	}
	return request, response
}

func TestSinkPostsCompletedCallAsOTLP(t *testing.T) {
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode OTLP payload: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		received <- payload
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sink := New(Config{
		Endpoint: server.URL,
		Headers:  http.Header{"Authorization": {"Bearer test-token"}},
	})
	defer sink.Close()

	request, response := callEnvelopes("session-1", 1, time.Unix(1_700_000_000, 0))
	sink.Emit(request)
	sink.Emit(response)

	select {
	case payload := <-received:
		resourceSpans := payload["resourceSpans"].([]any)
		scopeSpans := resourceSpans[0].(map[string]any)["scopeSpans"].([]any)
		spans := scopeSpans[0].(map[string]any)["spans"].([]any)
		if len(spans) != 1 {
			t.Fatalf("posted %d spans, want 1", len(spans))
		}
		span := spans[0].(map[string]any)
		if span["name"] != "tools/call" {
			t.Fatalf("span name = %v, want tools/call", span["name"])
		}
		if span["endTimeUnixNano"] == span["startTimeUnixNano"] {
			t.Fatal("completed span has zero duration")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for OTLP payload")
	}
}

func TestSinkPostsDuplicateResponseOnlyOnce(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sink := New(Config{Endpoint: server.URL})
	request, response := callEnvelopes("session-1", 1, time.Now())
	sink.Emit(request)
	sink.Emit(response)
	sink.Emit(response)
	deadline := time.Now().Add(time.Second)
	for posts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("posts = %d, want one span for one completed call", got)
	}
}

func TestSinkCloseDrainsQueuedCompletedCall(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sink := New(Config{Endpoint: server.URL})
	request, response := callEnvelopes("session-1", 1, time.Now())
	sink.Emit(request)
	sink.Emit(response)
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("posts after Close = %d, want queued completed call drained", got)
	}
}

func TestSinkCorrelatesServerInitiatedCall(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sink := New(Config{Endpoint: server.URL})
	request, response := callEnvelopes("session-1", 1, time.Now())
	request.Direction = proxy.ServerToClient
	response.Direction = proxy.ClientToServer
	sink.Emit(request)
	sink.Emit(response)
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("posts = %d, want one server-initiated span", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSinkRecoversAfterTransportFailureAndBoundsQueue(t *testing.T) {
	received := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	started := make(chan struct{})
	release := make(chan struct{})
	var attempts atomic.Int32
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if attempts.Add(1) == 1 {
			close(started)
			select {
			case <-release:
				return nil, io.ErrUnexpectedEOF
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	sink := New(Config{
		Endpoint:   server.URL,
		Buffer:     2,
		Client:     &http.Client{Transport: transport},
		MinBackoff: time.Millisecond,
		MaxBackoff: 5 * time.Millisecond,
	})
	defer sink.Close()

	request, response := callEnvelopes("session-1", 1, time.Now())
	sink.Emit(request)
	sink.Emit(response)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first delivery attempt")
	}

	for i := 2; i < 20; i++ {
		request, response := callEnvelopes("session-1", i%9+1, time.Now())
		sink.Emit(request)
		sink.Emit(response)
	}
	if sink.Dropped() == 0 {
		t.Fatal("queue never dropped while delivery was blocked")
	}
	close(release)

	select {
	case <-received:
		if attempts.Load() < 2 {
			t.Fatalf("delivery attempts = %d, want retry", attempts.Load())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delivery after transport recovery")
	}
}

func TestSinkCloseCancelsBlockedDelivery(t *testing.T) {
	started := make(chan struct{})
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	sink := New(Config{
		Endpoint: "http://collector.invalid/v1/traces",
		Client:   &http.Client{Transport: transport},
	})
	request, response := callEnvelopes("session-1", 1, time.Now())
	sink.Emit(request)
	sink.Emit(response)

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for blocked delivery")
	}

	closed := make(chan error, 1)
	go func() { closed <- sink.Close() }()
	select {
	case err := <-closed:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Close() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close blocked on unavailable collector")
	}
}
