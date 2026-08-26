package metrics

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

func envelope(session, label string, seq uint64, at time.Time, direction proxy.Direction, raw string) proxy.Envelope {
	return proxy.Envelope{
		SessionID: session, ServerLabel: label, Seq: seq, TS: at,
		Direction: direction, Transport: proxy.TransportStdio, Raw: json.RawMessage(raw),
	}
}

func toolRequest(session, label string, seq uint64, at time.Time, id, tool string) proxy.Envelope {
	return envelope(session, label, seq, at, proxy.ClientToServer,
		`{"jsonrpc":"2.0","id":`+strconv.Quote(id)+`,"method":"tools/call","params":{"name":`+strconv.Quote(tool)+`}}`)
}

func toolResponse(session, label string, seq uint64, at time.Time, id, answer string) proxy.Envelope {
	return envelope(session, label, seq, at, proxy.ServerToClient,
		`{"jsonrpc":"2.0","id":`+strconv.Quote(id)+`,`+answer+`}`)
}

func toolRequestOnConn(session, label string, seq uint64, at time.Time, conn, id, tool string) proxy.Envelope {
	e := toolRequest(session, label, seq, at, id, tool)
	e.Transport = proxy.TransportHTTP
	e.ConnID = conn
	return e
}

func toolResponseOnConn(session, label string, seq uint64, at time.Time, conn, id, answer string) proxy.Envelope {
	e := toolResponse(session, label, seq, at, id, answer)
	e.Transport = proxy.TransportHTTP
	e.ConnID = conn
	return e
}

func TestCollectorWritesCountersHistogramAndEscapedLabels(t *testing.T) {
	c := New()
	start := time.Unix(1_700_000_000, 0)
	label := "srv\"\\\n"
	tool := "echo\"\\\n"
	c.Observe(toolRequest("s1", label, 1, start, "1", tool))
	c.Observe(toolResponse("s1", label, 2, start.Add(20*time.Millisecond), "1", `"result":{"content":[]}`))
	c.Observe(toolRequest("s1", label, 3, start.Add(time.Second), "2", tool))
	c.Observe(toolResponse("s1", label, 4, start.Add(2*time.Second), "2", `"result":{"content":[],"isError":true}`))
	c.Observe(toolRequest("s1", label, 5, start.Add(3*time.Second), "3", tool))
	c.Observe(toolResponse("s1", label, 6, start.Add(4*time.Second), "3", `"error":{"code":-32603,"message":"boom"}`))

	var out strings.Builder
	if err := c.Write(&out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, `# TYPE mcpsnoop_tool_calls_total counter`) {
		t.Fatal("missing calls counter type")
	}
	if !strings.Contains(got, `mcpsnoop_tool_calls_total{server="srv\"\\\n",server_id="`) || !strings.Contains(got, `",tool="echo\"\\\n"} 3`) {
		t.Fatalf("escaped counter labels or value missing:\n%s", got)
	}
	if !strings.Contains(got, `mcpsnoop_tool_errors_total{server="srv\"\\\n",server_id="`) || !strings.Contains(got, `",tool="echo\"\\\n",error_type="tool"} 1`) {
		t.Fatalf("tool error counter missing:\n%s", got)
	}
	if !strings.Contains(got, `mcpsnoop_tool_errors_total{server="srv\"\\\n",server_id="`) || !strings.Contains(got, `",tool="echo\"\\\n",error_type="protocol"} 1`) {
		t.Fatalf("protocol error counter missing:\n%s", got)
	}
	if !strings.Contains(got, `mcpsnoop_tool_call_duration_seconds_bucket{server="srv\"\\\n",server_id="`) || !strings.Contains(got, `",tool="echo\"\\\n",le="0.025"} 1`) {
		t.Fatalf("cumulative 25ms bucket missing:\n%s", got)
	}
	if !strings.Contains(got, `",tool="echo\"\\\n",le="+Inf"} 3`) || !strings.Contains(got, `mcpsnoop_tool_call_duration_seconds_count{server="srv\"\\\n",server_id="`) {
		t.Fatalf("histogram count missing:\n%s", got)
	}
}

func TestCollectorCountsMRTRAsOneCallAndOneObservation(t *testing.T) {
	c := New()
	start := time.Unix(1_700_000_000, 0)
	c.Observe(toolRequest("s1", "server", 1, start, "1", "confirm"))
	c.Observe(envelope("s1", "server", 2, start.Add(time.Second), proxy.ServerToClient,
		`{"jsonrpc":"2.0","id":"1","result":{"resultType":"input_required","requestState":"state","inputRequests":{"answer":{"method":"elicitation/create"}}}}`))
	c.Observe(envelope("s1", "server", 3, start.Add(5*time.Second), proxy.ClientToServer,
		`{"jsonrpc":"2.0","id":"2","method":"tools/call","params":{"name":"confirm","requestState":"state","inputResponses":{"answer":{"action":"accept"}}}}`))
	c.Observe(toolResponse("s1", "server", 4, start.Add(6*time.Second), "2", `"result":{"content":[]}`))

	var out strings.Builder
	if err := c.Write(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `mcpsnoop_tool_calls_total{server="server",server_id="`) || !strings.Contains(out.String(), `",tool="confirm"} 1`) {
		t.Fatalf("MRTR operation counted more than once:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `mcpsnoop_tool_call_duration_seconds_count{server="server",server_id="`) || !strings.Contains(out.String(), `",tool="confirm"} 1`) {
		t.Fatalf("MRTR duration observed more than once:\n%s", out.String())
	}
}

func TestCollectorSkipsCancelledTaskLatency(t *testing.T) {
	c := New()
	start := time.Unix(1_700_000_000, 0)
	c.Observe(toolRequest("s1", "server", 1, start, "1", "slow"))
	c.Observe(toolResponse("s1", "server", 2, start.Add(time.Millisecond), "1", `"result":{"resultType":"task","taskId":"task-1","status":"working"}`))
	c.Observe(envelope("s1", "server", 3, start.Add(time.Second), proxy.ClientToServer,
		`{"jsonrpc":"2.0","id":"2","method":"tasks/get","params":{"taskId":"task-1"}}`))
	c.Observe(envelope("s1", "server", 4, start.Add(5*time.Second), proxy.ServerToClient,
		`{"jsonrpc":"2.0","method":"notifications/tasks","params":{"taskId":"task-1","status":"cancelled"}}`))

	var out strings.Builder
	if err := c.Write(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `",tool="slow"} 1`) {
		t.Fatalf("cancelled task call missing:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `mcpsnoop_tool_call_duration_seconds_count{server="server",server_id="`) || !strings.Contains(out.String(), `",tool="slow"} 0`) {
		t.Fatalf("cancelled task must not create a latency observation:\n%s", out.String())
	}
}

func TestCollectorDropsSupersededOperationState(t *testing.T) {
	c := New()
	start := time.Unix(1_700_000_000, 0)
	c.Observe(toolRequest("s1", "server", 1, start, "same", "echo"))
	c.Observe(toolRequest("s1", "server", 2, start.Add(time.Second), "same", "echo"))
	if len(c.operations) != 1 {
		t.Fatalf("operations = %d, want only the newest request", len(c.operations))
	}
	c.Observe(toolResponse("s1", "server", 3, start.Add(2*time.Second), "same", `"result":{"content":[]}`))

	var out strings.Builder
	if err := c.Write(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `",tool="echo"} 2`) || !strings.Contains(out.String(), `mcpsnoop_tool_call_duration_seconds_count{server="server",server_id="`) || !strings.Contains(out.String(), `",tool="echo"} 1`) {
		t.Fatalf("superseded call accounting is wrong:\n%s", out.String())
	}
}

func TestCollectorDropsToolWhenSameRequestIdentityIsReusedByNonTool(t *testing.T) {
	c := New()
	start := time.Unix(1_700_000_000, 0)
	c.Observe(toolRequest("s1", "server", 1, start, "same", "echo"))
	c.Observe(envelope("s1", "server", 2, start.Add(time.Second), proxy.ClientToServer,
		`{"jsonrpc":"2.0","id":"same","method":"ping"}`))
	c.Observe(toolResponse("s1", "server", 3, start.Add(2*time.Second), "same", `"result":{"content":[]}`))

	var out strings.Builder
	if err := c.Write(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `",tool="echo"} 1`) || !strings.Contains(out.String(), `mcpsnoop_tool_call_duration_seconds_count{server="server",server_id="`) || !strings.Contains(out.String(), `",tool="echo"} 0`) {
		t.Fatalf("superseded tool operation remained active:\n%s", out.String())
	}
}

func TestCollectorCorrelatesSameHTTPIDByConnID(t *testing.T) {
	c := New()
	start := time.Unix(1_700_000_000, 0)
	c.Observe(toolRequestOnConn("s1", "server", 1, start, "client-a", "1", "echo"))
	c.Observe(toolRequestOnConn("s1", "server", 2, start.Add(time.Second), "client-b", "1", "echo"))
	c.Observe(toolResponseOnConn("s1", "server", 3, start.Add(1100*time.Millisecond), "client-a", "1", `"result":{"content":[]}`))
	c.Observe(toolResponseOnConn("s1", "server", 4, start.Add(3*time.Second), "client-b", "1", `"error":{"code":-32603,"message":"boom"}`))

	var out strings.Builder
	if err := c.Write(&out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, `",tool="echo"} 2`) {
		t.Fatalf("same-id HTTP calls were not both counted:\n%s", got)
	}
	if !strings.Contains(got, `",tool="echo",error_type="protocol"} 1`) {
		t.Fatalf("same-id HTTP protocol error was not retained:\n%s", got)
	}
	if !strings.Contains(got, `mcpsnoop_tool_call_duration_seconds_sum{server="server",server_id="`) || !strings.Contains(got, `",tool="echo"} 3.1`) {
		t.Fatalf("same-id HTTP durations were not both observed:\n%s", got)
	}
}

func TestCollectorPrimeExcludesBackfillMetrics(t *testing.T) {
	c := New()
	start := time.Unix(1_700_000_000, 0)
	c.Prime(toolRequest("s1", "server", 1, start, "1", "echo"))
	c.Prime(toolResponse("s1", "server", 2, start.Add(time.Second), "1", `"result":{"content":[]}`))

	var out strings.Builder
	if err := c.Write(&out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), `",tool="echo"} 1`) {
		t.Fatalf("backfill produced live metrics:\n%s", out.String())
	}
}

func TestRunHeadlessMetricsDoesNotStartTUI(t *testing.T) {
	if _, err := net.Listen("unix", filepath.Join(t.TempDir(), "probe.sock")); err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	dir := t.TempDir()
	socket := filepath.Join(dir, "hub.sock")
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- RunHeadless(ctx, socket, dir, 0, addr) }()

	var resp *http.Response
	for range 50 {
		resp, err = http.Get("http://" + addr + "/metrics")
		if err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		cancel()
		t.Fatalf("headless metrics listener did not start: %v", err)
	}
	cancel()
	if err := <-runErr; err != nil {
		t.Fatalf("headless metrics run = %v", err)
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("headless hub socket still exists, stat error = %v", err)
	}
}

type blockingWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(p), nil
}

func TestCollectorScrapeDoesNotBlockLiveAggregation(t *testing.T) {
	c := New()
	start := time.Unix(1_700_000_000, 0)
	c.Observe(toolRequest("s1", "server", 1, start, "1", "echo"))
	c.Observe(toolResponse("s1", "server", 2, start.Add(time.Millisecond), "1", `"result":{"content":[]}`))

	writer := &blockingWriter{started: make(chan struct{}), release: make(chan struct{})}
	written := make(chan error, 1)
	go func() { written <- c.Write(writer) }()
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("scrape did not start")
	}

	aggregated := make(chan struct{})
	go func() {
		c.Observe(toolRequest("s1", "server", 3, start.Add(time.Second), "2", "echo"))
		close(aggregated)
	}()
	select {
	case <-aggregated:
	case <-time.After(time.Second):
		close(writer.release)
		t.Fatal("live aggregation was blocked by a scrape writer")
	}
	close(writer.release)
	if err := <-written; err != nil {
		t.Fatal(err)
	}
}

func TestCollectorAggregatesConcurrentSessions(t *testing.T) {
	c := New()
	start := time.Unix(1_700_000_000, 0)
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			session := "s" + strconv.Itoa(i)
			at := start.Add(time.Duration(i) * time.Millisecond)
			c.Observe(toolRequest(session, "server", 1, at, "1", "echo"))
			c.Observe(toolResponse(session, "server", 2, at.Add(10*time.Millisecond), "1", `"result":{"content":[]}`))
		}(i)
	}
	wg.Wait()

	var out strings.Builder
	if err := c.Write(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `mcpsnoop_tool_calls_total{server="server",server_id="`) || !strings.Contains(out.String(), `",tool="echo"} 40`) {
		t.Fatalf("concurrent calls were lost or duplicated:\n%s", out.String())
	}
}

func TestCollectorKeepsDistinctServerIdentitiesApart(t *testing.T) {
	c := New()
	start := time.Unix(1_700_000_000, 0)
	for i, command := range [][]string{{"node", "alpha.js"}, {"node", "beta.js"}} {
		session := "s" + strconv.Itoa(i)
		meta, _ := json.Marshal(proxy.SessionMeta{Command: command, CWD: "/srv"})
		c.Observe(proxy.Envelope{SessionID: session, ServerLabel: "same", Seq: 1, TS: start, Direction: proxy.DirectionMeta, Raw: meta})
		c.Observe(toolRequest(session, "same", 2, start, "1", "search"))
		c.Observe(toolResponse(session, "same", 3, start.Add(time.Millisecond), "1", `"result":{"content":[]}`))
	}

	var out strings.Builder
	if err := c.Write(&out); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(out.String(), `mcpsnoop_tool_calls_total{server="same"`); got != 2 {
		t.Fatalf("same-label servers were pooled, got %d series:\n%s", got, out.String())
	}
}

func TestMetricsServerOnlyServesMetricsOnItsOwnListener(t *testing.T) {
	c := New()
	server, err := NewServer("127.0.0.1:0", c)
	if err != nil {
		t.Fatal(err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve() }()
	defer func() {
		if err := server.Close(); err != nil {
			t.Fatal(err)
		}
		if err := <-serveErr; err != nil {
			t.Fatal(err)
		}
	}()

	resp, err := http.Get("http://" + server.Addr().String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("metrics response = %d/%q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(string(body), "# TYPE mcpsnoop_tool_calls_total counter") {
		t.Fatalf("metrics response missing exposition:\n%s", body)
	}

	resp, err = http.Get("http://" + server.Addr().String() + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("non-metrics path status = %d, want 404", resp.StatusCode)
	}
}
