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
	"unicode/utf8"

	"github.com/kerlenton/mcpsnoop/internal/hub"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

const (
	toolCallsMetric    = "mcpsnoop_tool_calls_total"
	toolErrorsMetric   = "mcpsnoop_tool_errors_total"
	toolDurationMetric = "mcpsnoop_tool_call_duration_seconds"
	// transportErrorsMetric counts failures that never became a JSON-RPC message,
	// so they can never be attributed to a tool.
	//
	// A gateway answering 502, or a 401 challenge, arrives as a status and a body
	// that is not JSON-RPC. The store counts it against the session and attaches
	// no call, deliberately, because nothing in the response says which request
	// it answered: the connection id is the client's address, and a client may
	// have several requests in flight on it. Without this series a gateway outage
	// showed as a fall in mcpsnoop_tool_calls_total and a flat error line, which
	// reads like traffic stopping rather than traffic failing, while a default
	// `mcpsnoop check` run over the same capture failed.
	transportErrorsMetric = "mcpsnoop_transport_errors_total"
)

// Buckets are the conventional Prometheus defaults, expressed in seconds.
var buckets = [...]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

const (
	// maxLabelBytes bounds a label value taken off the wire.
	//
	// A tool label is whatever a peer put in params.name, and Write repeats it on
	// seventeen lines per series. A frame may carry 16 MiB of it, so one request
	// measured out at a 71 MB scrape body from a 4 MiB name, which Prometheus
	// drops whole against body_size_limit, taking every real series for that
	// target with it. Truncating happens before the cardinality cap below, so a
	// thousand long names that share a prefix fold into one series rather than
	// each minting its own.
	maxLabelBytes = 128
	// labelElision marks a value this cut. It is longer than it needs to be so
	// that a reader who meets it in a dashboard is not left guessing.
	labelElision = "...(truncated by mcpsnoop)"
	// maxSeries bounds distinct series, which is the other half of the same
	// problem. Nothing on the wire constrains how many tool names a peer uses,
	// and neither peer has to cooperate: a server writing unsolicited tools/call
	// lines on its own stdout mints one series each. Past the cap, calls are
	// still counted, under overflowTool, so the totals stay right and the flood
	// is visible rather than fatal.
	//
	// Folding rather than evicting is deliberate. Evicting a series resets a
	// counter, and Prometheus reads a counter that went backwards as a target
	// restart, which fabricates traffic that never happened.
	maxSeries = 2000
	// overflowTool is where everything past the cap is counted. A real tool of
	// this name would land in the same bucket, which is a collision folding
	// makes possible and cannot avoid, since folding is lossy by construction.
	overflowTool = "(over-series-cap)"
)

// clampLabel bounds one wire-supplied label value.
func clampLabel(value string) string {
	if len(value) <= maxLabelBytes {
		return value
	}
	// Cut on a rune boundary so the result is still valid UTF-8, which the
	// exposition format requires of a label value.
	cut := maxLabelBytes - len(labelElision)
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + labelElision
}

// Collector is the hub-side live metrics aggregate. Its small bounded store is
// only a correlator: counters and histogram observations are retained here,
// while frame bodies and the timeline are not.
type Collector struct {
	mu         sync.RWMutex
	store      *store.Store
	series     map[seriesKey]*toolSeries
	transport  map[transportKey]uint64
	operations map[operationKey]*operation
	inflight   map[requestKey]operationKey
	cancelled  []operationKey
	// identities memoises the labels for a session. Deriving them walks every
	// session the store has ever seen, allocating a full header for each, and it
	// ran once per observed envelope while the write lock was held. A session's
	// identity is fixed once its first frames land, so the walk only has to
	// happen the first time one is seen.
	identities map[string]serverIdentityLabels
}

type seriesKey struct {
	server   string
	serverID string
	tool     string
}

// transportKey is a failure with no tool to hang it on. status is bounded by
// what an HTTP status line can hold, so unlike a tool name it needs no cap.
type transportKey struct {
	server   string
	serverID string
	status   int
}

type operationKey struct {
	session string
	seq     uint64
}

type requestKey struct {
	session string
	// dir, for the reason store.callKey carries it. JSON-RPC scopes id
	// uniqueness to the sender, so a server-initiated request may legally reuse
	// the id of a tool call that is still in flight. Without it, the supersede
	// branch in Observe treats that as a retry and finishes the live call, and
	// its error and its latency are never recorded.
	dir  proxy.Direction
	id   string
	conn string
}

type operation struct {
	series    seriesKey
	dir       proxy.Direction
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
		transport:  make(map[transportKey]uint64),
		operations: make(map[operationKey]*operation),
		inflight:   make(map[requestKey]operationKey),
		identities: make(map[string]serverIdentityLabels),
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
	server, known := c.identities[env.SessionID]
	if !known {
		server = labelsForSession(c.store, env.SessionID, header)
		c.identities[env.SessionID] = server
	}

	if event.Kind == store.EventTransport && event.Errored {
		c.transport[transportKey{server: server.label, serverID: server.id, status: event.HTTPStatus}]++
	}

	if event.Kind == store.EventRequest && event.Call != nil && event.MRTRRoot == "" {
		key := operationKey{session: env.SessionID, seq: event.Call.RequestSeq}
		request := requestKey{session: env.SessionID, dir: env.Direction, id: event.Call.ID, conn: env.ConnID}
		if previous, exists := c.inflight[request]; exists && previous != key {
			if op := c.operations[previous]; op != nil {
				c.finish(previous, op)
			} else {
				delete(c.inflight, request)
			}
		}
		if event.Call.IsTool && event.Call.ToolName != "" {
			if _, exists := c.operations[key]; !exists {
				series := seriesKey{server: server.label, serverID: server.id, tool: clampLabel(event.Call.ToolName)}
				c.operations[key] = &operation{series: series, dir: env.Direction, id: event.Call.ID, conn: env.ConnID}
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
	request := requestKey{session: key.session, dir: op.dir, id: op.id, conn: op.conn}
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

// seriesFor returns the series for key, creating it while there is room. Past
// the cap the call is counted against overflowTool for the same server, so a
// flood of names costs one series rather than one each and the totals still add
// up. Caller holds c.mu for writing.
func (c *Collector) seriesFor(key seriesKey) *toolSeries {
	if series := c.series[key]; series != nil {
		return series
	}
	if len(c.series) >= maxSeries {
		overflow := seriesKey{server: key.server, serverID: key.serverID, tool: overflowTool}
		if series := c.series[overflow]; series != nil {
			return series
		}
		// The overflow series itself must always fit, or the cap would silently
		// discard the calls it is meant to keep counting.
		series := &toolSeries{}
		c.series[overflow] = series
		return series
	}
	series := &toolSeries{}
	c.series[key] = series
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
	transportByKey := make(map[transportKey]uint64, len(c.transport))
	transportKeys := make([]transportKey, 0, len(c.transport))
	for key, count := range c.transport {
		transportByKey[key] = count
		transportKeys = append(transportKeys, key)
	}
	c.mu.RUnlock()
	slices.SortFunc(transportKeys, func(a, b transportKey) int {
		if c := strings.Compare(a.server, b.server); c != 0 {
			return c
		}
		if c := strings.Compare(a.serverID, b.serverID); c != 0 {
			return c
		}
		return a.status - b.status
	})
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

	// Emitted whether or not anything failed, so a dashboard that graphs it does
	// not go blank on a healthy hub and leave the reader wondering which of the
	// two it is looking at.
	if _, err := fmt.Fprintln(w, "# HELP "+transportErrorsMetric+" Total number of observed transport failures that carried no JSON-RPC message."); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "# TYPE "+transportErrorsMetric+" counter"); err != nil {
		return err
	}
	for _, key := range transportKeys {
		if _, err := fmt.Fprintf(w, "%s{server=\"%s\",server_id=\"%s\",status=\"%d\"} %d\n",
			transportErrorsMetric, escapeLabel(key.server), escapeLabel(key.serverID), key.status,
			transportByKey[key]); err != nil {
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
		// The hub first, and its error before the listener's. A scrape in flight
		// keeps Shutdown polling to its deadline, so on an ordinary SIGTERM
		// during a scrape the listener reports a timeout it already recovered
		// from by closing the connections, and reporting that would exit 1 on a
		// clean stop and hide whatever the hub had to say.
		hubErr := <-hubErr
		closeErr := server.Close()
		metricsErr := <-serveErr
		if hubErr != nil {
			return hubErr
		}
		if metricsErr != nil {
			return metricsErr
		}
		if closeErr != nil && !errors.Is(closeErr, context.DeadlineExceeded) {
			return fmt.Errorf("metrics listener shutdown: %w", closeErr)
		}
		return nil
	}
}
