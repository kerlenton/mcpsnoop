// Package metrics aggregates live tool calls and exposes Prometheus text
// exposition without adding a runtime telemetry dependency.
package metrics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/hub"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

const (
	toolCallsMetric    = "mcpsnoop_tool_calls_total"
	toolErrorsMetric   = "mcpsnoop_tool_errors_total"
	toolDurationMetric = "mcpsnoop_tool_call_duration_seconds"
)

// Buckets are the conventional Prometheus defaults, expressed in seconds.
var buckets = [...]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Collector is the hub-side live metrics aggregate. Its small bounded store is
// only a correlator: counters and histogram observations are retained here,
// while frame bodies and the timeline are not.
type Collector struct {
	mu         sync.RWMutex
	store      *store.Store
	series     map[seriesKey]*toolSeries
	operations map[operationKey]*operation
	inflight   map[requestKey]operationKey
	cancelled  []operationKey
}

type seriesKey struct {
	server   string
	serverID string
	tool     string
}

type operationKey struct {
	session string
	seq     uint64
}

type requestKey struct {
	session string
	id      string
	conn    string
}

type operation struct {
	series    seriesKey
	id        string
	conn      string
	cancelled bool
}

type toolSeries struct {
	calls  uint64
	errors [2]uint64
	bucket [len(buckets) + 1]uint64
	sum    float64
	count  uint64
}

// New creates an empty live collector. It deliberately uses a one-frame
// bounded Store so Store remains the sole owner of JSON-RPC and MRTR
// correlation without retaining a second copy of the live capture.
func New() *Collector {
	return &Collector{
		store:      store.NewBounded(1, 1),
		series:     make(map[seriesKey]*toolSeries),
		operations: make(map[operationKey]*operation),
		inflight:   make(map[requestKey]operationKey),
	}
}

// Prime adds historical state without producing live metrics. It is used during
// hub startup so a reconnect can retain the session identity and correlation
// context learned from the log.
func (c *Collector) Prime(env proxy.Envelope) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store.Ingest(env)
}

// Observe adds one deduplicated live envelope. It does no network or disk work.
func (c *Collector) Observe(env proxy.Envelope) {
	c.mu.Lock()
	defer c.mu.Unlock()

	event := c.store.Ingest(env)
	header, ok := sessionHeader(c.store, env.SessionID)
	if !ok {
		return
	}
	server := labelsForSession(c.store, env.SessionID, header)

	if event.Kind == store.EventRequest && event.Call != nil && event.MRTRRoot == "" {
		key := operationKey{session: env.SessionID, seq: event.Call.RequestSeq}
		request := requestKey{session: env.SessionID, id: event.Call.ID, conn: env.ConnID}
		if previous, exists := c.inflight[request]; exists && previous != key {
			if op := c.operations[previous]; op != nil {
				c.finish(previous, op)
			} else {
				delete(c.inflight, request)
			}
		}
		if event.Call.IsTool && event.Call.ToolName != "" {
			if _, exists := c.operations[key]; !exists {
				series := seriesKey{server: server.label, serverID: server.id, tool: event.Call.ToolName}
				c.operations[key] = &operation{series: series, id: event.Call.ID, conn: env.ConnID}
				c.inflight[request] = key
				c.seriesFor(series).calls++
			}
		}
	}

	call := event.Call
	if event.TaskCall != nil && event.TaskCall.IsTool {
		call = event.TaskCall
	}
	if call == nil || !call.IsTool || call.ToolName == "" || !call.Done() {
		return
	}
	key := operationKey{session: env.SessionID, seq: call.RequestSeq}
	op := c.operations[key]
	if op == nil {
		return
	}
	if call.State == store.Cancelled && !call.LateResult {
		if !op.cancelled {
			op.cancelled = true
			c.cancelled = append(c.cancelled, key)
			c.trimCancelled()
		}
		return
	}

	series := c.seriesFor(op.series)
	if call.Errored {
		if call.ToolErr {
			series.errors[0]++
		} else {
			series.errors[1]++
		}
	}
	if call.State != store.Superseded && !(call.State == store.Cancelled && !call.LateResult) &&
		!(call.TaskStatus == "cancelled" && !call.Errored) {
		duration := call.Duration()
		if duration >= 0 {
			seconds := duration.Seconds()
			series.sum += seconds
			series.count++
			for i, bound := range buckets {
				if seconds <= bound {
					series.bucket[i]++
				}
			}
			series.bucket[len(buckets)]++
		}
	}
	c.finish(key, op)
}

func (c *Collector) finish(key operationKey, op *operation) {
	delete(c.operations, key)
	request := requestKey{session: key.session, id: op.id, conn: op.conn}
	if current, ok := c.inflight[request]; ok && current == key {
		delete(c.inflight, request)
	}
}

func (c *Collector) trimCancelled() {
	const maxCancelled = 1024
	if len(c.cancelled) <= maxCancelled {
		return
	}
	key := c.cancelled[0]
	if op := c.operations[key]; op != nil {
		c.finish(key, op)
	}
	c.cancelled = c.cancelled[1:]
}

func (c *Collector) seriesFor(key seriesKey) *toolSeries {
	series := c.series[key]
	if series == nil {
		series = &toolSeries{}
		c.series[key] = series
	}
	return series
}

type serverIdentityLabels struct {
	label string
	id    string
}

func sessionHeader(st *store.Store, sessionID string) (store.SessionHeader, bool) {
	for _, header := range st.Sessions() {
		if header.ID == sessionID {
			return header, true
		}
	}
	return store.SessionHeader{}, false
}

// labelsForSession follows the identity split used by inventory and stats. The
// friendly label is paired with a stable fingerprint of the recorded command
// and cwd (stdio), endpoint (HTTP), or label/transport fallback. Raw commands,
// paths, endpoints, and session ids never become Prometheus label values.
func labelsForSession(st *store.Store, sessionID string, header store.SessionHeader) serverIdentityLabels {
	command, cwd, _ := st.Command(sessionID)
	const sep = "\x00"
	var identity string
	switch {
	case len(command) > 0:
		identity = "stdio" + sep + cwd + sep + strings.Join(command, sep)
	case header.Endpoint != "":
		identity = "http" + sep + header.Endpoint
	default:
		identity = "label" + sep + header.Transport + sep + header.Label
	}
	identity += sep + header.Label
	digest := sha256.Sum256([]byte(identity))
	label := header.Label
	if label == "" {
		label = "(unlabelled)"
	}
	return serverIdentityLabels{label: label, id: hex.EncodeToString(digest[:8])}
}

// Write renders the current aggregate in deterministic Prometheus text format.
func (c *Collector) Write(w io.Writer) error {
	c.mu.RLock()
	seriesByKey := make(map[seriesKey]toolSeries, len(c.series))
	keys := make([]seriesKey, 0, len(c.series))
	for key, series := range c.series {
		seriesByKey[key] = *series
		keys = append(keys, key)
	}
	c.mu.RUnlock()
	slices.SortFunc(keys, func(a, b seriesKey) int {
		if c := strings.Compare(a.server, b.server); c != 0 {
			return c
		}
		if c := strings.Compare(a.serverID, b.serverID); c != 0 {
			return c
		}
		return strings.Compare(a.tool, b.tool)
	})

	if _, err := fmt.Fprintln(w, "# HELP "+toolCallsMetric+" Total number of observed MCP tool calls."); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "# TYPE "+toolCallsMetric+" counter"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "# HELP "+toolErrorsMetric+" Total number of observed MCP tool call errors by error type."); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "# TYPE "+toolErrorsMetric+" counter"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "# HELP "+toolDurationMetric+" Request-to-response duration of observed MCP tool calls in seconds."); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "# TYPE "+toolDurationMetric+" histogram"); err != nil {
		return err
	}
	for _, key := range keys {
		series := seriesByKey[key]
		labels := baseLabels(key)
		if _, err := fmt.Fprintf(w, "%s{%s} %d\n", toolCallsMetric, labels, series.calls); err != nil {
			return err
		}
		for i, kind := range []string{"tool", "protocol"} {
			if _, err := fmt.Fprintf(w, "%s{%s,error_type=\"%s\"} %d\n", toolErrorsMetric, labels, kind, series.errors[i]); err != nil {
				return err
			}
		}
		for i, bound := range buckets {
			if _, err := fmt.Fprintf(w, "%s_bucket{%s,le=\"%s\"} %d\n", toolDurationMetric, labels, formatFloat(bound), series.bucket[i]); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "%s_bucket{%s,le=\"+Inf\"} %d\n", toolDurationMetric, labels, series.bucket[len(buckets)]); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "%s_sum{%s} %s\n", toolDurationMetric, labels, formatFloat(series.sum)); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "%s_count{%s} %d\n", toolDurationMetric, labels, series.count); err != nil {
			return err
		}
	}
	return nil
}

func baseLabels(key seriesKey) string {
	return `server="` + escapeLabel(key.server) + `",server_id="` + escapeLabel(key.serverID) + `",tool="` + escapeLabel(key.tool) + `"`
}

func escapeLabel(value string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(value)
}

func formatFloat(value float64) string {
	return fmt.Sprintf("%g", value)
}

// ServeHTTP serves only /metrics. A separate server/listener is used by the
// CLI, so this handler can never shadow an MCP proxy path.
func (c *Collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/metrics" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if r.Method == http.MethodHead {
		return
	}
	if err := c.Write(w); err != nil {
		return
	}
}

// Server is the opt-in Prometheus HTTP listener.
type Server struct {
	listener net.Listener
	server   *http.Server
}

// NewServer binds an independent TCP listener for metrics.
func NewServer(addr string, collector *Collector) (*Server, error) {
	if collector == nil {
		return nil, errors.New("metrics: nil collector")
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &Server{
		listener: listener,
		server: &http.Server{
			Handler:           collector,
			ReadHeaderTimeout: 5 * time.Second,
		},
	}, nil
}

// Addr returns the bound address, useful when the listener requested port 0.
func (s *Server) Addr() net.Addr { return s.listener.Addr() }

// Serve runs the metrics listener until it is closed.
func (s *Server) Serve() error {
	err := s.server.Serve(s.listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Close stops the metrics listener and waits for in-flight requests to finish.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := s.server.Shutdown(ctx); err != nil {
		// Shutdown leaves active handlers alone after the deadline. Close the
		// connections so a blocked scrape cannot hold up hub shutdown forever.
		_ = s.server.Close()
		return err
	}
	return nil
}

// RunHeadless runs the hub and metrics listener without starting a TUI. History
// primes the collector's correlation store, but only envelopes received after
// backfill are observed as live metrics.
func RunHeadless(ctx context.Context, socketPath, sessionsDir string, historyLimit int, listen string) error {
	if listen == "" {
		return errors.New("metrics: empty listen address")
	}
	collector := New()
	server, err := NewServer(listen, collector)
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve() }()

	h := hub.NewWithOptions(socketPath, sessionsDir, func(proxy.Envelope) {}, hub.Options{
		BackfillLimit:      historyLimit,
		OnBackfillEnvelope: collector.Prime,
		OnLive:             collector.Observe,
	})
	hubErr := make(chan error, 1)
	go func() { hubErr <- h.Run(runCtx) }()

	select {
	case err := <-hubErr:
		closeErr := server.Close()
		metricsErr := <-serveErr
		if err != nil {
			return err
		}
		if closeErr != nil {
			return fmt.Errorf("metrics listener shutdown: %w", closeErr)
		}
		return metricsErr
	case err := <-serveErr:
		cancel()
		<-hubErr
		if err != nil {
			return fmt.Errorf("metrics listener stopped: %w", err)
		}
		return nil
	case <-ctx.Done():
		cancel()
		closeErr := server.Close()
		hubErr := <-hubErr
		metricsErr := <-serveErr
		if closeErr != nil {
			return fmt.Errorf("metrics listener shutdown: %w", closeErr)
		}
		if hubErr != nil {
			return hubErr
		}
		return metricsErr
	}
}
