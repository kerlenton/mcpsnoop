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

// TestCollectorBoundsALabelTakenOffTheWire. A tool label is whatever a peer put
// in params.name, a frame may carry 16 MiB of it, and Write repeats it on
// seventeen lines per series. One request measured out at a 71 MB scrape body,
// which Prometheus drops whole against body_size_limit, taking every real series
// for that target with it.
func TestCollectorBoundsALabelTakenOffTheWire(t *testing.T) {
	c := New()
	at := time.Unix(1_700_000_000, 0)
	huge := strings.Repeat("A", 4<<20)
	c.Observe(toolRequest("s1", "srv", 1, at, "1", huge))
	c.Observe(toolResponse("s1", "srv", 2, at.Add(time.Millisecond), "1", `"result":{"content":[]}`))

	var out strings.Builder
	if err := c.Write(&out); err != nil {
		t.Fatal(err)
	}
	if n := out.Len(); n > 64<<10 {
		t.Errorf("one 4 MiB tool name rendered %d bytes of exposition, want a bounded body", n)
	}
	if strings.Contains(out.String(), strings.Repeat("A", maxLabelBytes+1)) {
		t.Error("the label reached the exposition at more than its cap")
	}
	if !strings.Contains(out.String(), labelElision) {
		t.Error("the label was cut without saying so, so a reader cannot tell truncation from a real name")
	}
}

// TestCollectorFoldsTooManyToolNamesRatherThanGrowingForever. Nothing on the
// wire constrains how many tool names a peer uses, and neither peer has to
// cooperate: the store recognises a tools/call by its method, so a server
// writing them on its own stdout mints one series each. Past the cap the calls
// still have to be counted, or the cap would discard the traffic it exists to
// keep measuring.
func TestCollectorFoldsTooManyToolNamesRatherThanGrowingForever(t *testing.T) {
	c := New()
	at := time.Unix(1_700_000_000, 0)
	const flood = maxSeries * 3
	for i := range flood {
		id := strconv.Itoa(i)
		c.Observe(toolRequest("s1", "srv", uint64(2*i+1), at, id, "tool-"+id))
		c.Observe(toolResponse("s1", "srv", uint64(2*i+2), at.Add(time.Millisecond), id, `"result":{"content":[]}`))
	}
	c.mu.RLock()
	held := len(c.series)
	c.mu.RUnlock()
	if held > maxSeries+1 {
		t.Errorf("held %d series for %d tool names, want no more than the cap plus the overflow bucket", held, flood)
	}

	var out strings.Builder
	if err := c.Write(&out); err != nil {
		t.Fatal(err)
	}
	// Every call is still counted somewhere, so a dashboard's total stays true.
	var total float64
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.HasPrefix(line, toolCallsMetric+"{") {
			continue
		}
		// The value is after the last space, since a label may hold one.
		cut := strings.LastIndex(line, " ")
		if cut < 0 {
			t.Fatalf("no value in %q", line)
		}
		value := line[cut+1:]
		n, err := strconv.ParseFloat(value, 64)
		if err != nil {
			t.Fatalf("unparseable count in %q", line)
		}
		total += n
	}
	if int(total) != flood {
		t.Errorf("counted %d calls across every series, want %d: folding must not lose the totals", int(total), flood)
	}
	if !strings.Contains(out.String(), overflowTool) {
		t.Error("nothing in the exposition says the series list was capped")
	}
}

// TestCollectorKeepsAToolCallASeverInitiatedRequestReuses. JSON-RPC scopes id
// uniqueness to the sender, so a server may legally issue a request carrying the
// id of a tool call that is still in flight. Keying the in-flight map without
// the direction made that look like a retry, and the live call was finished
// before its answer arrived, losing its error and its latency.
func TestCollectorKeepsAToolCallASeverInitiatedRequestReuses(t *testing.T) {
	c := New()
	at := time.Unix(1_700_000_000, 0)
	c.Observe(toolRequest("s1", "srv", 1, at, "7", "search"))
	// The server asks the client something of its own, reusing id 7.
	c.Observe(envelope("s1", "srv", 2, at.Add(5*time.Millisecond), proxy.ServerToClient,
		`{"jsonrpc":"2.0","id":"7","method":"roots/list"}`))
	c.Observe(toolResponse("s1", "srv", 3, at.Add(20*time.Millisecond), "7",
		`"result":{"content":[],"isError":true}`))

	var out strings.Builder
	if err := c.Write(&out); err != nil {
		t.Fatal(err)
	}
	body := out.String()
	// The values, not the substrings. Every metric name appears in a HELP and a
	// TYPE line whether or not anything was counted, and the tool keeps its
	// calls_total series either way, so presence proves nothing here.
	if got := seriesValue(t, body, toolErrorsMetric, `tool="search"`); got != 1 {
		t.Errorf("errors for the tool = %v, want 1: a server request reusing its id must not finish it\n%s", got, body)
	}
	if got := seriesValue(t, body, toolDurationMetric+"_count", `tool="search"`); got != 1 {
		t.Errorf("latency observations for the tool = %v, want 1\n%s", got, body)
	}
}

// seriesValue totals every sample of metric whose line contains match.
//
// Every one, not the first. A tool carries two error series, one per
// error_type, so reading the first would let a count land in the other slot and
// go unnoticed. The value is after the last space, since a label may hold one.
func seriesValue(t *testing.T, body, metric, match string) float64 {
	t.Helper()
	var total float64
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, metric+"{") || !strings.Contains(line, match) {
			continue
		}
		cut := strings.LastIndex(line, " ")
		if cut < 0 {
			t.Fatalf("no value in %q", line)
		}
		v, err := strconv.ParseFloat(line[cut+1:], 64)
		if err != nil {
			t.Fatalf("unparseable value in %q", line)
		}
		total += v
	}
	return total
}

// TestCollectorCountsATransportFailureThatHasNoTool. A gateway answering 502, or
// a 401 challenge, arrives as a status and a body that is not JSON-RPC. Nothing
// in it says which request it answered, so it can never be attributed to a tool.
// Left uncounted, a gateway outage read as traffic stopping rather than traffic
// failing, while `mcpsnoop check` over the same capture failed on it.
func TestCollectorCountsATransportFailureThatHasNoTool(t *testing.T) {
	c := New()
	at := time.Unix(1_700_000_000, 0)
	c.Observe(toolRequestOnConn("s1", "srv", 1, at, "c1", "1", "echo"))
	c.Observe(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: at.Add(30 * time.Millisecond),
		Direction: proxy.ServerToClient, Transport: proxy.TransportHTTP, ConnID: "c1", Status: 502,
	})

	var out strings.Builder
	if err := c.Write(&out); err != nil {
		t.Fatal(err)
	}
	body := out.String()
	if got := seriesValue(t, body, transportErrorsMetric, `status="502"`); got != 1 {
		t.Errorf("transport errors = %v, want 1\n%s", got, body)
	}
	// And it is not invented onto the tool, since nothing said it was that call's.
	if got := seriesValue(t, body, toolErrorsMetric, `tool="echo"`); got != 0 {
		t.Errorf("the 502 was attributed to a tool it cannot be known to belong to, got %v", got)
	}
	if !strings.Contains(body, "# TYPE "+transportErrorsMetric+" counter") {
		t.Error("the family is missing its TYPE line")
	}
}

// TestCollectorAlwaysDeclaresTheTransportFamily. A dashboard that graphs a
// metric which only appears once something has failed cannot tell a healthy hub
// from a broken query.
func TestCollectorAlwaysDeclaresTheTransportFamily(t *testing.T) {
	var out strings.Builder
	if err := New().Write(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "# TYPE "+transportErrorsMetric+" counter") {
		t.Errorf("an idle collector does not declare %s:\n%s", transportErrorsMetric, out.String())
	}
}
