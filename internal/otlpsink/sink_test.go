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

	"bytes"
	"github.com/kerlenton/mcpsnoop/internal/exporter"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/store"
	"strings"
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
		// The sink posts through the exporter's own span code, so it carries the
		// same semantic conventions as the file export, span name included. A live
		// push and an exported file describe one capture the same way, and a
		// consumer does not have to know which produced it.
		// TestSinkPostsAMultiRoundTripOperationWholeAndAsTheExportWould holds that
		// to the span for the case where it used to fail.
		if span["name"] != "tools/call lookup" {
			t.Fatalf("span name = %v, want the convention's \"{method} {target}\"", span["name"])
		}
		if attrs, ok := span["attributes"].([]any); ok {
			found := false
			for _, a := range attrs {
				if a.(map[string]any)["key"] == "mcp.method.name" {
					found = true
				}
			}
			if !found {
				t.Error("a posted span must carry the required mcp.method.name")
			}
		} else {
			t.Error("posted span has no attributes")
		}
		if span["endTimeUnixNano"] == span["startTimeUnixNano"] {
			t.Fatal("completed span has zero duration")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for OTLP payload")
	}
}

func TestSinkPostsRequestTraceContext(t *testing.T) {
	const (
		traceID      = "4bf92f3577b34da6a3ce929d0e0e4736"
		parentSpanID = "00f067aa0ba902b7"
		traceparent  = "00-" + traceID + "-" + parentSpanID + "-01"
	)
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	sink := New(Config{Endpoint: server.URL})
	defer sink.Close()
	request, response := callEnvelopes("session-1", 1, time.Unix(1_700_000_000, 0))
	request.Raw = json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup","_meta":{"traceparent":"` + traceparent + `"}}}`)
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
		if span["traceId"] != traceID || span["parentSpanId"] != parentSpanID {
			t.Fatalf("posted trace context = %v/%v, want %s/%s", span["traceId"], span["parentSpanId"], traceID, parentSpanID)
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

func TestSinkEmitAfterCloseDoesNotPanic(t *testing.T) {
	sink := New(Config{Endpoint: "http://collector.invalid/v1/traces", Buffer: 1})
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	// A proxy goroutine can still emit during shutdown; it must drop rather than
	// panic on a send to a closed channel.
	request, response := callEnvelopes("session-1", 1, time.Now())
	sink.Emit(request)
	sink.Emit(response)
	if sink.Dropped() == 0 {
		t.Fatal("post-close emits should be counted as dropped")
	}
}

func TestSinkDropsPayloadAfterMaxRetriesAndAdvances(t *testing.T) {
	received := make(chan struct{}, 1)
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) <= maxDeliveryAttempts {
			w.WriteHeader(http.StatusInternalServerError) // fail every attempt for the first call
			return
		}
		received <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sink := New(Config{
		Endpoint:   server.URL,
		Buffer:     8,
		MinBackoff: time.Millisecond,
		MaxBackoff: 2 * time.Millisecond,
	})
	defer sink.Close()

	// The first completed call fails every attempt, so it is dropped after the
	// retry budget instead of blocking newer spans forever.
	req1, resp1 := callEnvelopes("session-1", 1, time.Now())
	sink.Emit(req1)
	sink.Emit(resp1)
	// The second call is delivered once the sink advances past the dropped first.
	req2, resp2 := callEnvelopes("session-1", 2, time.Now())
	sink.Emit(req2)
	sink.Emit(resp2)

	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("sink did not advance to the second call after giving up on the first")
	}
	if sink.Dropped() == 0 {
		t.Fatal("the undeliverable first call should have been dropped")
	}
}

// mrtrEnvelopes is a booking that took two requests, with a person answering in
// between, and an ordinary call landing while it waited. Timestamps carry no
// monotonic reading, so the live path and the file export see identical times.
func mrtrEnvelopes() []proxy.Envelope {
	t0 := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	frames := []struct {
		dir proxy.Direction
		ms  int
		raw string
	}{
		{proxy.ClientToServer, 0, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"book_flight"}}`},
		{proxy.ServerToClient, 100, `{"jsonrpc":"2.0","id":1,"result":{"resultType":"input_required","requestState":"st-1","inputRequests":{"confirm":{"method":"elicitation/create"}}}}`},
		{proxy.ClientToServer, 200, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"lookup"}}`},
		{proxy.ServerToClient, 250, `{"jsonrpc":"2.0","id":9,"result":{"content":[]}}`},
		{proxy.ClientToServer, 4100, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"book_flight","requestState":"st-1","inputResponses":{"confirm":{"action":"accept"}}}}`},
		{proxy.ServerToClient, 4400, `{"jsonrpc":"2.0","id":2,"result":{"content":[]}}`},
	}
	out := make([]proxy.Envelope, 0, len(frames))
	for i, f := range frames {
		out = append(out, proxy.Envelope{
			SessionID: "session-mrtr", ServerLabel: "booking", Seq: uint64(i + 1),
			TS: t0.Add(time.Duration(f.ms) * time.Millisecond), Direction: f.dir,
			Transport: proxy.TransportStdio, Raw: json.RawMessage(f.raw),
		})
	}
	return out
}

// spansByID flattens OTLP payloads to span id and the span's JSON, attributes
// included, so two paths can be compared span for span.
func spansByID(t *testing.T, payloads ...[]byte) map[string]string {
	t.Helper()
	out := make(map[string]string)
	for _, p := range payloads {
		var doc struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []map[string]json.RawMessage `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		if err := json.Unmarshal(p, &doc); err != nil {
			t.Fatalf("invalid OTLP JSON: %v", err)
		}
		for _, rs := range doc.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, sp := range ss.Spans {
					var id string
					if err := json.Unmarshal(sp["spanId"], &id); err != nil {
						t.Fatal(err)
					}
					if _, dup := out[id]; dup {
						t.Errorf("span %s delivered twice", id)
					}
					canon, err := json.Marshal(sp)
					if err != nil {
						t.Fatal(err)
					}
					out[id] = string(canon)
				}
			}
		}
	}
	return out
}

// TestSinkPostsAMultiRoundTripOperationWholeAndAsTheExportWould is the live half
// of the multi round-trip fix. A fresh store per request and response pair could
// not link a retry to the operation it continues, so the live push never sent the
// request the server answered by asking a person, and sent the retry alone with
// its own short duration, which is the measurement this tool exists to get right.
// The sink now holds one store, sends nothing until the operation finishes, then
// sends it whole, and what it sends is exactly what an export of the same capture
// says, span for span and attribute for attribute.
func TestSinkPostsAMultiRoundTripOperationWholeAndAsTheExportWould(t *testing.T) {
	received := make(chan []byte, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		received <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sink := New(Config{Endpoint: server.URL})
	envelopes := mrtrEnvelopes()
	for _, env := range envelopes {
		sink.Emit(env)
	}

	var posts [][]byte
	for len(posts) < 2 {
		select {
		case p := <-received:
			posts = append(posts, p)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out after %d posts, want 2", len(posts))
		}
	}
	sink.Close()

	// The ordinary call landed while the booking waited for its person, so it is
	// the first thing out. A sink that posted the waiting request on its own would
	// have sent that first.
	first := spansByID(t, posts[0])
	if len(first) != 1 || !strings.Contains(firstSpanName(first), "tools/call lookup") {
		t.Fatalf("first post = %v, want the ordinary call alone", first)
	}
	second := spansByID(t, posts[1])
	if len(second) != 3 {
		t.Fatalf("the finished booking went out as %d spans, want execute_tool and both hops", len(second))
	}

	st := store.New()
	for _, env := range envelopes {
		st.Ingest(env)
	}
	data, err := exporter.Build(st, "session-mrtr")
	if err != nil {
		t.Fatal(err)
	}
	var file bytes.Buffer
	if err := exporter.WriteOTLP(&file, data); err != nil {
		t.Fatal(err)
	}
	live, exported := spansByID(t, posts...), spansByID(t, file.Bytes())
	if len(live) != len(exported) {
		t.Fatalf("live push sent %d spans, the export holds %d", len(live), len(exported))
	}
	for id, want := range exported {
		if got, ok := live[id]; !ok {
			t.Errorf("span %s is in the export and was never pushed: %s", id, want)
		} else if got != want {
			t.Errorf("span %s differs\n live   %s\n export %s", id, got, want)
		}
	}
}

func firstSpanName(spans map[string]string) string {
	for _, s := range spans {
		return s
	}
	return ""
}

// TestSinkWaitsForALateResultRatherThanPostingTheCancellation pins the race the
// cancellation section tells both sides to handle, a response already in flight
// when the cancellation went out. Posting the moment the cancellation arrives
// would send the call without that answer and then drop the answer as a
// duplicate, so a cancelled call waits a short grace window first.
func TestSinkWaitsForALateResultRatherThanPostingTheCancellation(t *testing.T) {
	received := make(chan []byte, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sink := New(Config{Endpoint: server.URL})
	t0 := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	frames := []struct {
		dir proxy.Direction
		ms  int
		raw string
	}{
		{proxy.ClientToServer, 0, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"slow"}}`},
		{proxy.ClientToServer, 1000, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":3,"reason":"user gave up"}}`},
		{proxy.ServerToClient, 4000, `{"jsonrpc":"2.0","id":3,"result":{"content":[]}}`},
	}
	for i, f := range frames {
		sink.Emit(proxy.Envelope{SessionID: "late", ServerLabel: "slow", Seq: uint64(i + 1),
			TS: t0.Add(time.Duration(f.ms) * time.Millisecond), Direction: f.dir, Raw: json.RawMessage(f.raw)})
	}

	var post []byte
	select {
	case post = <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("the late result was never posted")
	}
	sink.Close()
	select {
	case extra := <-received:
		t.Fatalf("the call went out twice, second post %s", extra)
	default:
	}
	spans := spansByID(t, post)
	if len(spans) != 1 {
		t.Fatalf("posted %d spans, want the one call", len(spans))
	}
	span := firstSpanName(spans)
	for _, want := range []string{`"mcpsnoop.call.late_result"`, `"mcpsnoop.call.cancelled_at"`, `"user gave up"`} {
		if !strings.Contains(span, want) {
			t.Errorf("the posted span should carry %s, since it went out with the late answer\n%s", want, span)
		}
	}
}

func cancelledCall(sink *Sink, session string, withReason bool) {
	t0 := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	params := `{"requestId":5}`
	if withReason {
		params = `{"requestId":5,"reason":"timed out"}`
	}
	sink.Emit(proxy.Envelope{SessionID: session, ServerLabel: "slow", Seq: 1, TS: t0,
		Direction: proxy.ClientToServer, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"hang"}}`)})
	sink.Emit(proxy.Envelope{SessionID: session, ServerLabel: "slow", Seq: 2, TS: t0.Add(30 * time.Second),
		Direction: proxy.ClientToServer, Raw: json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":` + params + `}`)})
}

// TestSinkPostsACancelledCallThatGetsNoAnswer covers the ordinary end of a
// cancellation. A server receiving one SHOULD not answer, and a client cancels
// exactly when a request has hung past its timeout, so a sink that waited for an
// answer kept every hung call out of the collector. It goes out once the grace
// window passes, saying when it was cancelled and why, and claiming no result.
func TestSinkPostsACancelledCallThatGetsNoAnswer(t *testing.T) {
	received := make(chan []byte, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sink := New(Config{Endpoint: server.URL, CancelGrace: 50 * time.Millisecond})
	defer sink.Close()
	cancelledCall(sink, "hung", true)

	var post []byte
	select {
	case post = <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled call that got no answer never reached the collector")
	}
	span := firstSpanName(spansByID(t, post))
	for _, want := range []string{`"mcpsnoop.call.cancelled_at"`, `"timed out"`, `"STATUS_CODE_UNSET"`} {
		if !strings.Contains(span, want) {
			t.Errorf("the cancelled call should carry %s\n%s", want, span)
		}
	}
	if strings.Contains(span, `"mcpsnoop.call.late_result"`) {
		t.Errorf("no answer came, so no late result may be claimed\n%s", span)
	}
}

// TestSinkCloseSendsACancelledCallStillWaiting covers shutdown inside the grace
// window. Nothing more will arrive once the sink is closing, so a call waiting
// for an answer goes out then rather than being lost with the process.
func TestSinkCloseSendsACancelledCallStillWaiting(t *testing.T) {
	received := make(chan []byte, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sink := New(Config{Endpoint: server.URL, CancelGrace: time.Hour})
	cancelledCall(sink, "closing", false)
	sink.Close()

	select {
	case post := <-received:
		if !strings.Contains(firstSpanName(spansByID(t, post)), `"mcpsnoop.call.cancelled_at"`) {
			t.Errorf("close sent something other than the cancelled call: %s", post)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("closing the sink dropped a cancelled call that was waiting for its answer")
	}
}
