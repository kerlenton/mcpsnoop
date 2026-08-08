package exporter

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/jsonwire"
	"github.com/kerlenton/mcpsnoop/internal/paths"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

type Format string

const (
	FormatJSON Format = "json"
	FormatHTML Format = "html"
	FormatText Format = "text"
	FormatOTLP Format = "otlp"
	FormatHAR  Format = "har"
)

type Options struct {
	Format    Format
	Redaction proxy.RedactConfig
}

type SessionExport struct {
	GeneratedAt  time.Time           `json:"generated_at"`
	Session      SessionSummary      `json:"session"`
	Summary      ToolSummaryExport   `json:"summary"`
	Capabilities *CapabilitiesExport `json:"capabilities,omitempty"`
	Calls        []CallExport        `json:"calls"`
	Events       []EventExport       `json:"events"`
}

type ToolSummaryExport struct {
	Definitions  *ToolListCostExport `json:"definitions,omitempty"`
	Tools        []ToolStatsExport   `json:"tools"`
	SlowestCalls []SlowCallExport    `json:"slowest_calls"`
}

// ToolListCostExport is the fixed context cost of the advertised tool list. All
// figures are byte counts; mcpsnoop does not tokenise, so nothing here is or
// implies a token count.
type ToolListCostExport struct {
	Tools int `json:"tools"`
	Bytes int `json:"bytes"`
	// Complete is false when tools/list never finished paginating, which makes
	// Bytes a floor rather than the total.
	Complete bool             `json:"complete"`
	PerTool  []ToolCostExport `json:"per_tool"`
}

type ToolCostExport struct {
	Name             string   `json:"name"`
	Bytes            int      `json:"bytes"`
	DescriptionBytes int      `json:"description_bytes"`
	SchemaBytes      int      `json:"schema_bytes"`
	Findings         []string `json:"findings,omitempty"`
}

type ToolStatsExport struct {
	Name    string  `json:"name"`
	Calls   int     `json:"calls"`
	Errors  int     `json:"errors"`
	Pending int     `json:"pending"`
	P50MS   float64 `json:"p50_ms"`
	P95MS   float64 `json:"p95_ms"`
	P99MS   float64 `json:"p99_ms"`
	// ResultBytes and MaxResultBytes are the per-call half of the context cost.
	ResultBytes    int64 `json:"result_bytes"`
	MaxResultBytes int   `json:"max_result_bytes"`
}

type SlowCallExport struct {
	CallIndex  int     `json:"call_index"`
	ID         string  `json:"id"`
	ToolName   string  `json:"tool_name"`
	DurationMS float64 `json:"duration_ms"`
	IsError    bool    `json:"is_error"`
}

type SessionSummary struct {
	ID            string    `json:"id"`
	Label         string    `json:"label"`
	First         time.Time `json:"first"`
	Last          time.Time `json:"last"`
	Requests      int       `json:"requests"`
	Responses     int       `json:"responses"`
	Notifications int       `json:"notifications"`
	Errors        int       `json:"errors"`
	Pending       int       `json:"pending"`
	LateResults   int       `json:"late_results"`
	// MissingFrames counts envelopes dropped upstream, inferred from Seq gaps.
	// A non-zero value means the capture is incomplete.
	MissingFrames uint64 `json:"missing_frames"`
}

type CapabilitiesExport struct {
	ProtocolVersion string          `json:"protocol_version,omitempty"`
	ClientInfo      json.RawMessage `json:"client_info,omitempty"`
	ServerInfo      json.RawMessage `json:"server_info,omitempty"`
	Client          json.RawMessage `json:"client,omitempty"`
	Server          json.RawMessage `json:"server,omitempty"`
	Instructions    string          `json:"instructions,omitempty"`
}

type CallExport struct {
	Index        int             `json:"index"`
	ID           string          `json:"id"`
	Method       string          `json:"method"`
	Direction    proxy.Direction `json:"direction"`
	State        string          `json:"state"`
	Status       string          `json:"status"`
	IsTool       bool            `json:"is_tool"`
	ToolName     string          `json:"tool_name,omitempty"`
	IsError      bool            `json:"is_error"`
	ToolError    bool            `json:"tool_error"`
	TaskID       string          `json:"task_id,omitempty"`
	TaskStatus   string          `json:"task_status,omitempty"`
	StartedAt    time.Time       `json:"started_at"`
	EndedAt      *time.Time      `json:"ended_at,omitempty"`
	CancelledAt  *time.Time      `json:"cancelled_at,omitempty"`
	CancelReason string          `json:"cancel_reason,omitempty"`
	LateResult   bool            `json:"late_result,omitempty"`
	DurationMS   *float64        `json:"duration_ms,omitempty"`
	Params       json.RawMessage `json:"params,omitempty"`
	Result       json.RawMessage `json:"result,omitempty"`
	Error        *proxy.RPCError `json:"error,omitempty"`
	// ErrorName is the specification's name for Error.Code, absent when the code
	// is one the spec leaves to implementations. A sibling rather than a field
	// inside Error, so the error object stays the wire shape.
	ErrorName string `json:"error_name,omitempty"`
}

type EventExport struct {
	Seq         uint64          `json:"seq"`
	Timestamp   time.Time       `json:"timestamp"`
	Direction   proxy.Direction `json:"direction"`
	Kind        string          `json:"kind"`
	Method      string          `json:"method,omitempty"`
	ID          string          `json:"id,omitempty"`
	Warning     string          `json:"warning,omitempty"`
	Observation string          `json:"observation,omitempty"`
	Mismatch    bool            `json:"mismatch,omitempty"`
	// Status is the HTTP status the frame arrived on and AuthChallenge that
	// response's WWW-Authenticate header, both absent on stdio.
	Status        int    `json:"http_status,omitempty"`
	AuthChallenge string `json:"auth_challenge,omitempty"`
	Truncated     bool   `json:"truncated,omitempty"`
	Deprecated    string `json:"deprecated,omitempty"`
	// CacheTTLMs is a pointer so an explicit ttlMs of 0, which the spec defines as
	// immediately stale, stays distinguishable from a server that declared none.
	CacheTTLMs        *int            `json:"cache_ttl_ms,omitempty"`
	CacheScope        string          `json:"cache_scope,omitempty"`
	CacheStaleRefetch string          `json:"cache_stale_refetch,omitempty"`
	CallIndex         *int            `json:"call_index,omitempty"`
	Raw               json.RawMessage `json:"raw,omitempty"`
	Text              string          `json:"text,omitempty"`
}

// cacheTTLExport keeps a declared ttlMs of 0 in the export. omitempty on a bare
// int would drop it, which would render "immediately stale" as though the server
// had declared nothing, the exact state the check beside it reports as a
// violation.
func cacheTTLExport(h store.CacheHint) *int {
	if !h.TTLPresent {
		return nil
	}
	ttl := h.TTLMs
	return &ttl
}

func ParseFormat(s string) (Format, error) {
	switch Format(strings.ToLower(strings.TrimSpace(s))) {
	case FormatJSON:
		return FormatJSON, nil
	case FormatHTML:
		return FormatHTML, nil
	case FormatText:
		return FormatText, nil
	case FormatHAR:
		return FormatHAR, nil
	case FormatOTLP:
		return FormatOTLP, nil
	default:
		return "", fmt.Errorf("unknown export format %q (want json, html, text, har, or otlp)", s)
	}
}

func ResolveSessionPath(arg string) (string, error) {
	if arg != "" {
		if _, err := os.Stat(arg); err == nil {
			return arg, nil
		}
		if filepath.Ext(arg) == ".jsonl" || strings.ContainsRune(arg, filepath.Separator) {
			return "", errPathNotFound(arg)
		}
		path := paths.SessionLogPath(arg)
		if _, err := os.Stat(path); err != nil {
			return "", errPathNotFound(path)
		}
		return path, nil
	}

	files, err := filepath.Glob(filepath.Join(paths.SessionsDir(), "*.jsonl"))
	if err != nil || len(files) == 0 {
		return "", errors.New("no session logs found")
	}
	var latest string
	var latestMod time.Time
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			continue
		}
		// An empty log holds no session. The trace file is created before the
		// wrapped server is started, so a launch that fails leaves one behind, and
		// it is the newest thing in the directory. Resolving to it answered "no
		// envelopes found" for a bare `check` or `export` that meant the last real
		// capture, which in CI reads as a failure caused by an unrelated run.
		if info.Size() == 0 {
			continue
		}
		if latest == "" || info.ModTime().After(latestMod) {
			latest = f
			latestMod = info.ModTime()
		}
	}
	if latest == "" {
		return "", errors.New("no readable session logs found")
	}
	return latest, nil
}

func DefaultOutputPath(sessionID string, format Format) string {
	ext := string(format)
	switch format {
	case FormatText:
		ext = "txt"
	case FormatOTLP:
		ext = "otlp.json" // OTLP payload is JSON, keep it distinct from the json export
	}
	return filepath.Join(paths.ExportsDir(), safeFileName(sessionID)+"."+ext)
}

func LoadFile(path string) (*store.Store, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	return Load(f, path)
}

// LoadFileLines is LoadFile plus the log line every envelope was decoded from,
// for a caller that has to point a reader at one specific frame of the file.
func LoadFileLines(path string) (*store.Store, string, FrameLines, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", nil, err
	}
	defer f.Close()
	lines := make(FrameLines)
	st, sessionID, err := load(f, path, proxy.RedactConfig{}, lines)
	if err != nil {
		return nil, "", nil, err
	}
	return st, sessionID, lines, nil
}

// FrameRef identifies one captured envelope inside a log. Seq alone would not:
// it restarts per session, and a concatenated capture holds several.
type FrameRef struct {
	SessionID string
	Seq       uint64
}

// FrameLines maps a captured envelope to the 1-based line of the log it was
// decoded from. Seq cannot stand in for that line, because it is scoped per
// session and skips the frames dropped upstream, so the two diverge in exactly
// the captures worth pointing a reader at.
type FrameLines map[FrameRef]int

// Line returns the 1-based log line a frame was decoded from. ok is false for a
// frame this index never saw, and for every frame of a stream that was not
// loaded through LoadFileLines.
func (f FrameLines) Line(sessionID string, seq uint64) (int, bool) {
	line, ok := f[FrameRef{SessionID: sessionID, Seq: seq}]
	return line, ok
}

// Load reads a JSONL envelope stream into a store and returns its first session.
func Load(r io.Reader, source string) (*store.Store, string, error) {
	return load(r, source, proxy.RedactConfig{}, nil)
}

// load folds a JSONL envelope stream into a store. When lines is non-nil it is
// filled with the log line each envelope came from, which costs a wrapping
// reader, so the callers that do not need it pass nil.
func load(r io.Reader, source string, redaction proxy.RedactConfig, lines FrameLines) (*store.Store, string, error) {
	st := store.New()
	redactor := proxy.NewRedactor(redaction)
	var firstSession string
	var counter *newlineCounter
	if lines != nil {
		counter = &newlineCounter{r: r}
		r = counter
	}
	dec := json.NewDecoder(r)
	for {
		var env proxy.Envelope
		if err := dec.Decode(&env); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, "", fmt.Errorf("%s: invalid JSONL envelope: %w", source, err)
		}
		if firstSession == "" {
			firstSession = env.SessionID
		}
		if counter != nil {
			// InputOffset lands just past the envelope's closing brace, so it names
			// the line the value ended on. The proxy writes one compact envelope per
			// line, so that is also the line it started on.
			lines[FrameRef{SessionID: env.SessionID, Seq: env.Seq}] = counter.lineAt(dec.InputOffset())
		}
		st.Ingest(redactor.RedactEnvelope(env))
	}
	if firstSession == "" {
		return nil, "", fmt.Errorf("%s: no envelopes found", source)
	}
	return st, firstSession, nil
}

// newlineCounter records where every newline fell in the stream, so a decoded
// value's byte offset can be turned back into a line number. A running count
// kept alongside Decode would be wrong: json.Decoder buffers ahead of the value
// it hands back, so by the time Decode returns the reader has usually already
// consumed the lines that follow.
type newlineCounter struct {
	r    io.Reader
	read int64 // bytes pulled from r so far
	// offsets holds the newlines not yet walked past, next indexes the first of
	// them, and passed counts the ones already behind us.
	offsets []int64
	next    int
	passed  int
}

func (c *newlineCounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	for i, b := range p[:n] {
		if b == '\n' {
			c.offsets = append(c.offsets, c.read+int64(i))
		}
	}
	c.read += int64(n)
	return n, err
}

// lineAt returns the 1-based line containing the byte before offset, which is
// where a decoded value ends. Offsets must arrive in non-decreasing order, which
// a single forward pass over the stream guarantees.
func (c *newlineCounter) lineAt(offset int64) int {
	for c.next < len(c.offsets) && c.offsets[c.next] < offset {
		c.next++
		c.passed++
	}
	// The offsets already walked past are dropped on every call rather than only
	// when the buffer happens to drain, which a json.Decoder reading ahead of the
	// value it returns almost never leaves true: a long capture kept one entry per
	// line for the whole load. What remains is bounded by the decoder's read-ahead,
	// so the copy is over a handful of entries.
	if c.next > 0 {
		c.offsets = c.offsets[:copy(c.offsets, c.offsets[c.next:])]
		c.next = 0
	}
	return c.passed + 1
}

func Build(st *store.Store, sessionID string) (SessionExport, error) {
	var header store.SessionHeader
	found := false
	for _, h := range st.Sessions() {
		if h.ID == sessionID {
			header = h
			found = true
			break
		}
	}
	if !found {
		return SessionExport{}, fmt.Errorf("session %q not found", sessionID)
	}

	calls := st.Calls(sessionID)
	callIndex := make(map[string]int, len(calls))
	outCalls := make([]CallExport, 0, len(calls))
	for i, c := range calls {
		callIndex[callKey(c)] = i
		outCalls = append(outCalls, exportCall(i, c))
	}

	events := st.Timeline(sessionID)
	outEvents := make([]EventExport, 0, len(events))
	for _, ev := range events {
		outEvents = append(outEvents, exportEvent(ev, callIndex))
	}

	out := SessionExport{
		GeneratedAt: time.Now().UTC(),
		Session: SessionSummary{
			ID:            header.ID,
			Label:         header.Label,
			First:         header.First,
			Last:          header.Last,
			Requests:      header.Requests,
			Responses:     header.Responses,
			Notifications: header.Notifications,
			Errors:        header.Errors,
			Pending:       header.Pending,
			LateResults:   header.LateResults,
			MissingFrames: header.MissingFrames,
		},
		Calls:  outCalls,
		Events: outEvents,
	}
	if summary, ok := st.ToolSummary(sessionID); ok {
		out.Summary = exportToolSummary(summary)
	}
	// Definition cost is keyed to the advertised list rather than to the calls,
	// so it is fetched separately: a server whose expensive tools were never
	// called still charged for them, and that is exactly the case worth seeing.
	if costs, ok := st.ToolCosts(sessionID); ok {
		out.Summary.Definitions = exportToolListCost(costs)
	}
	if caps, ok := st.Capabilities(sessionID); ok {
		out.Capabilities = &CapabilitiesExport{
			ProtocolVersion: caps.ProtocolVersion,
			ClientInfo:      caps.ClientInfo,
			ServerInfo:      caps.ServerInfo,
			Client:          caps.Client,
			Server:          caps.Server,
			Instructions:    caps.Instructions,
		}
	}
	return out, nil
}

func exportToolSummary(summary store.SessionToolSummary) ToolSummaryExport {
	out := ToolSummaryExport{
		Tools:        make([]ToolStatsExport, 0, len(summary.Tools)),
		SlowestCalls: make([]SlowCallExport, 0, len(summary.Slowest)),
	}
	for _, tool := range summary.Tools {
		out.Tools = append(out.Tools, ToolStatsExport{
			Name: tool.Name, Calls: tool.Calls, Errors: tool.Errors, Pending: tool.Pending,
			P50MS: durationMS(tool.P50), P95MS: durationMS(tool.P95), P99MS: durationMS(tool.P99),
			ResultBytes: tool.ResultBytes, MaxResultBytes: tool.MaxResultBytes,
		})
	}
	for _, call := range summary.Slowest {
		out.SlowestCalls = append(out.SlowestCalls, SlowCallExport{
			CallIndex: call.CallIndex,
			ID:        call.ID, ToolName: call.ToolName, DurationMS: durationMS(call.Duration), IsError: call.Failed,
		})
	}
	return out
}

func exportToolListCost(cost store.ToolListCost) *ToolListCostExport {
	out := &ToolListCostExport{
		Tools:    cost.Tools,
		Bytes:    cost.Bytes,
		Complete: cost.Complete,
		PerTool:  make([]ToolCostExport, 0, len(cost.PerTool)),
	}
	for _, tool := range cost.PerTool {
		findings := make([]string, 0, len(tool.FindingKinds))
		for _, kind := range tool.FindingKinds {
			findings = append(findings, string(kind))
		}
		out.PerTool = append(out.PerTool, ToolCostExport{
			Name:             tool.Name,
			Bytes:            tool.Bytes,
			DescriptionBytes: tool.DescriptionBytes,
			SchemaBytes:      tool.SchemaBytes,
			Findings:         findings,
		})
	}
	return out
}

func durationMS(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}

func Write(w io.Writer, data SessionExport, opts Options) error {
	format := opts.Format
	if format == "" {
		format = FormatJSON
	}
	switch format {
	case FormatJSON:
		enc := jsonwire.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(data)
	case FormatHTML:
		return writeHTML(w, data)
	case FormatText:
		return writeText(w, data)
	case FormatHAR:
		return WriteHAR(w, data)
	case FormatOTLP:
		return WriteOTLP(w, data)
	default:
		return fmt.Errorf("unknown export format %q", format)
	}
}

// otlpExport follows the OTLP JSON encoding. Each correlated MCP call becomes a
// span, making a session importable into tracing backends.
//
// A call whose request carried a traceparent joins the caller's trace, so the
// spans of one session can legitimately span several traces. The rest share a
// trace derived from the session id, so a session with no propagation still
// reads as one unit. Either way mcpsnoop.session.id is a resource attribute
// rather than a span one, so the tie back to the capture survives the split.
// When a capture is incomplete the span count understates what happened on the
// wire, so the dropped-frame count rides the payload as the resource attribute
// mcpsnoop.session.missing_frames. It belongs there rather than in this comment:
// the person who needs it is importing the trace, not reading this file.
type otlpExport struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

type otlpResource struct {
	Attributes []otlpAttribute `json:"attributes"`
}

type otlpScopeSpans struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpScope struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type otlpSpan struct {
	TraceID           string          `json:"traceId"`
	SpanID            string          `json:"spanId"`
	ParentSpanID      string          `json:"parentSpanId,omitempty"`
	TraceState        string          `json:"traceState,omitempty"`
	Name              string          `json:"name"`
	Kind              string          `json:"kind"`
	StartTimeUnixNano string          `json:"startTimeUnixNano"`
	EndTimeUnixNano   string          `json:"endTimeUnixNano"`
	Attributes        []otlpAttribute `json:"attributes"`
	Status            otlpStatus      `json:"status"`
}

type otlpStatus struct {
	Code string `json:"code"`
}

type otlpAttribute struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

type otlpAnyValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
	// IntValue is a string because proto3 JSON encodes int64 that way, and OTLP
	// receivers parse it back. Writing a bare number here is the mistake that
	// makes a payload look right and get rejected at the collector.
	IntValue *string `json:"intValue,omitempty"`
}

// WriteOTLP writes data using the OTLP JSON encoding.
func WriteOTLP(w io.Writer, data SessionExport) error {
	sessionTraceID := otlpID(16, "trace", data.Session.ID)
	spans := make([]otlpSpan, 0, len(data.Calls))
	for _, call := range data.Calls {
		traceID, parentSpanID, traceState := sessionTraceID, "", ""
		if propagated, ok := traceContext(call.Params); ok {
			traceID = propagated.TraceID
			parentSpanID = propagated.ParentSpanID
			traceState = propagated.TraceState
		}
		end := call.StartedAt
		if call.EndedAt != nil {
			end = *call.EndedAt
		}
		attrs := []otlpAttribute{
			otlpString("rpc.system", "mcp"),
			otlpString("rpc.method", call.Method),
			otlpString("mcpsnoop.call.id", call.ID),
			otlpString("mcpsnoop.call.status", call.Status),
			otlpString("mcpsnoop.call.state", call.State),
			otlpBool("mcpsnoop.call.is_tool", call.IsTool),
			otlpBool("mcpsnoop.call.is_error", call.IsError),
			otlpBool("mcpsnoop.call.tool_error", call.ToolError),
		}
		if call.ToolName != "" {
			attrs = append(attrs, otlpString("mcpsnoop.call.tool_name", call.ToolName))
		}
		if call.DurationMS != nil {
			attrs = append(attrs, otlpDouble("mcpsnoop.call.duration_ms", *call.DurationMS))
		}
		if call.CancelledAt != nil {
			attrs = append(attrs, otlpString("mcpsnoop.call.cancelled_at", call.CancelledAt.Format(time.RFC3339Nano)))
		}
		if call.CancelReason != "" {
			attrs = append(attrs, otlpString("mcpsnoop.call.cancel_reason", call.CancelReason))
		}
		if call.LateResult {
			attrs = append(attrs, otlpBool("mcpsnoop.call.late_result", true))
		}
		status := "STATUS_CODE_OK"
		if call.State == "pending" || call.State == "superseded" || call.Status == "call_cancelled" || call.Status == "late_result" {
			status = "STATUS_CODE_UNSET"
		}
		if call.IsError {
			status = "STATUS_CODE_ERROR"
		}
		kind := "SPAN_KIND_CLIENT"
		if call.Direction == proxy.ServerToClient {
			kind = "SPAN_KIND_SERVER"
		}
		spans = append(spans, otlpSpan{
			TraceID:           traceID,
			SpanID:            otlpID(8, "span", data.Session.ID, call.ID, string(call.Direction)),
			ParentSpanID:      parentSpanID,
			TraceState:        traceState,
			Name:              call.Method,
			Kind:              kind,
			StartTimeUnixNano: fmt.Sprint(call.StartedAt.UnixNano()),
			EndTimeUnixNano:   fmt.Sprint(end.UnixNano()),
			Attributes:        attrs,
			Status:            otlpStatus{Code: status},
		})
	}
	payload := otlpExport{ResourceSpans: []otlpResourceSpans{{
		Resource: otlpResource{Attributes: []otlpAttribute{
			otlpString("service.name", "mcpsnoop"),
			otlpString("mcpsnoop.session.id", data.Session.ID),
			otlpString("mcpsnoop.session.label", data.Session.Label),
			otlpInt("mcpsnoop.session.late_results", int64(data.Session.LateResults)),
			// Emitted even when zero. Absence would be ambiguous between a capture
			// that dropped nothing and one exported before this attribute existed,
			// and the whole point of the count is that the span total can be
			// trusted, which only an explicit claim supports.
			otlpInt("mcpsnoop.session.missing_frames", int64(data.Session.MissingFrames)),
		}},
		ScopeSpans: []otlpScopeSpans{{Scope: otlpScope{Name: "mcpsnoop"}, Spans: spans}},
	}}}
	enc := jsonwire.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}

// propagatedContext is the W3C trace context a request carried. TraceState is
// empty when the caller sent none, or sent one this cannot make sense of.
type propagatedContext struct {
	TraceID      string
	ParentSpanID string
	TraceState   string
}

// traceContext reads the trace context from a request's params.
//
// The keys are deliberately unprefixed. Every other _meta key in MCP is
// reverse-DNS namespaced, but the specification carves out traceparent,
// tracestate and baggage by name so that trace context stays wire-compatible
// with OpenTelemetry; namespacing them is called out as the thing that would
// break traces. baggage is not read here: it is propagation state, not part of
// a span.
func traceContext(params json.RawMessage) (propagatedContext, bool) {
	// Maps keep the SEP carrier keys exact; struct decoding also matches keys
	// case-insensitively, which would accept a different carrier by accident.
	var request map[string]json.RawMessage
	if err := json.Unmarshal(params, &request); err != nil {
		return propagatedContext{}, false
	}
	rawMeta, ok := request["_meta"]
	if !ok {
		return propagatedContext{}, false
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(rawMeta, &meta); err != nil {
		return propagatedContext{}, false
	}
	traceID, parentSpanID, ok := parseTraceparent(metaString(meta, "traceparent"))
	if !ok {
		return propagatedContext{}, false
	}
	// Only once traceparent is valid: W3C requires tracestate to be discarded
	// when the traceparent it belongs to cannot be trusted, since the state
	// describes a trace we would otherwise be unable to name.
	return propagatedContext{
		TraceID:      traceID,
		ParentSpanID: parentSpanID,
		TraceState:   validTraceState(metaString(meta, "tracestate")),
	}, true
}

// metaString returns a string-valued _meta entry, or "" when it is absent or is
// some other JSON type. A carrier of the wrong type is treated as no carrier,
// because a number where a traceparent belongs says nothing about the trace.
func metaString(meta map[string]json.RawMessage, key string) string {
	raw, ok := meta[key]
	if !ok {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return value
}

// maxTraceStateMembers is the W3C limit. A list longer than this is to be
// discarded rather than truncated, since dropping members silently would change
// which vendor's state survives.
const maxTraceStateMembers = 32

// validTraceState returns a tracestate worth carrying, or "".
//
// The grammar is checked only as far as it has to be. mcpsnoop observes rather
// than participates: it adds no member of its own and mutates nothing, so the
// value it emits is the caller's, and a backend that rejects it would have
// rejected the caller's request identically. Parsing each member's key and value
// against the full grammar would mostly create ways to drop a valid header,
// which is the failure that would be silent. What is checked is what cannot be
// passed on meaningfully: an empty list, a member that is not a key=value pair,
// and a list past the limit at which the spec says to discard rather than trim.
func validTraceState(value string) string {
	if value == "" {
		return ""
	}
	members := strings.Split(value, ",")
	if len(members) > maxTraceStateMembers {
		return ""
	}
	for _, member := range members {
		member = strings.TrimSpace(member)
		if member == "" {
			continue // OWS between members is allowed, and so is a trailing comma
		}
		key, _, ok := strings.Cut(member, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return ""
		}
	}
	return value
}

func parseTraceparent(value string) (traceID, parentSpanID string, ok bool) {
	const fixedLength = 55
	if len(value) < fixedLength || value[2] != '-' || value[35] != '-' || value[52] != '-' {
		return "", "", false
	}

	version := value[:2]
	traceID = value[3:35]
	parentSpanID = value[36:52]
	flags := value[53:55]
	if !lowerHex(version) || version == "ff" ||
		!lowerHex(traceID) || allZero(traceID) ||
		!lowerHex(parentSpanID) || allZero(parentSpanID) ||
		!lowerHex(flags) {
		return "", "", false
	}
	if version == "00" && len(value) != fixedLength {
		return "", "", false
	}
	// Later versions may append fields. Their fixed prefix remains usable, but
	// the first unknown field still has to begin at a field boundary.
	if len(value) > fixedLength && value[fixedLength] != '-' {
		return "", "", false
	}
	return traceID, parentSpanID, true
}

func lowerHex(value string) bool {
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func allZero(value string) bool {
	for _, c := range value {
		if c != '0' {
			return false
		}
	}
	return true
}

func otlpID(length int, parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = fmt.Fprint(h, part, "\x00")
	}
	return fmt.Sprintf("%x", h.Sum(nil)[:length])
}

func otlpString(key, value string) otlpAttribute {
	return otlpAttribute{Key: key, Value: otlpAnyValue{StringValue: &value}}
}

func otlpBool(key string, value bool) otlpAttribute {
	return otlpAttribute{Key: key, Value: otlpAnyValue{BoolValue: &value}}
}

func otlpDouble(key string, value float64) otlpAttribute {
	return otlpAttribute{Key: key, Value: otlpAnyValue{DoubleValue: &value}}
}

func otlpInt(key string, value int64) otlpAttribute {
	encoded := strconv.FormatInt(value, 10)
	return otlpAttribute{Key: key, Value: otlpAnyValue{IntValue: &encoded}}
}

func ExportFile(inputPath string, w io.Writer, opts Options) error {
	f, err := os.Open(inputPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return Export(f, inputPath, w, opts)
}

// Export renders the session read from r, a JSONL envelope stream, to w. It is
// the reader form of ExportFile, so a piped log ("-") exports like a file.
func Export(r io.Reader, source string, w io.Writer, opts Options) error {
	st, sessionID, err := load(r, source, opts.Redaction, nil)
	if err != nil {
		return err
	}
	data, err := Build(st, sessionID)
	if err != nil {
		return err
	}
	return Write(w, data, opts)
}

func exportCall(index int, c store.CallView) CallExport {
	status := "ok"
	switch {
	case c.State == store.Pending:
		status = "pending"
	case c.State == store.Streaming:
		status = "streaming"
	case c.State == store.Superseded:
		status = "superseded"
	case c.State == store.Cancelled && c.LateResult:
		status = "late_result"
	case c.State == store.Cancelled:
		status = "call_cancelled"
	case c.TaskStatus == "cancelled":
		// Terminal and without a result, so not ok, but the user stopped the work on
		// purpose rather than hitting an error. Its own status ahead of Failed(),
		// following the superseded precedent, keeps it out of the error branch.
		status = "cancelled"
	case c.Failed():
		status = "error"
	}
	out := CallExport{
		Index:        index,
		ID:           c.ID,
		Method:       c.Method,
		Direction:    c.ReqDir,
		State:        c.State.String(),
		Status:       status,
		IsTool:       c.IsTool,
		ToolName:     c.ToolName,
		IsError:      c.Errored,
		ToolError:    c.ToolErr,
		TaskID:       c.TaskID,
		TaskStatus:   c.TaskStatus,
		StartedAt:    c.Start,
		CancelReason: c.CancelReason,
		LateResult:   c.LateResult,
		Params:       c.Params,
		Result:       c.Result,
		Error:        c.Err,
		ErrorName:    errorName(c.Err),
	}
	if !c.CancelledAt.IsZero() {
		cancelledAt := c.CancelledAt
		out.CancelledAt = &cancelledAt
	}
	if c.Done() && c.State != store.Superseded && (c.State != store.Cancelled || c.LateResult) {
		end := c.End
		dur := float64(c.End.Sub(c.Start)) / float64(time.Millisecond)
		out.EndedAt = &end
		out.DurationMS = &dur
	}
	return out
}

func exportEvent(ev store.EventView, callIndex map[string]int) EventExport {
	out := EventExport{
		Seq:               ev.Seq,
		Timestamp:         ev.TS,
		Direction:         ev.Dir,
		Kind:              eventKind(ev.Kind),
		Method:            ev.Method,
		ID:                ev.ID,
		Warning:           ev.Warning,
		Observation:       ev.Observation,
		Mismatch:          ev.RoutingMismatch,
		Status:            ev.HTTPStatus,
		AuthChallenge:     ev.AuthChallenge,
		Truncated:         ev.Truncated,
		Deprecated:        ev.Deprecated,
		CacheTTLMs:        cacheTTLExport(ev.CacheHint),
		CacheScope:        ev.CacheHint.Scope,
		CacheStaleRefetch: ev.CacheStaleRefetch,
		Raw:               ev.Raw,
		Text:              ev.Text,
	}
	if ev.Call != nil {
		if idx, ok := callIndex[callKey(*ev.Call)]; ok {
			out.CallIndex = &idx
		}
	}
	return out
}

func writeText(w io.Writer, data SessionExport) error {
	_, err := fmt.Fprintf(w, "mcpsnoop session %s (%s)\n", data.Session.ID, data.Session.Label)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "frames: %d  calls: %d  requests: %d  responses: %d  errors: %d  pending: %d  late results: %d\n\n",
		len(data.Events), len(data.Calls), data.Session.Requests, data.Session.Responses, data.Session.Errors, data.Session.Pending, data.Session.LateResults)
	if err != nil {
		return err
	}
	if data.Summary.Definitions != nil && len(data.Summary.Definitions.PerTool) > 0 {
		hasFindings := false
		for _, tool := range data.Summary.Definitions.PerTool {
			if len(tool.Findings) > 0 {
				hasFindings = true
				break
			}
		}
		if hasFindings {
			if _, err := fmt.Fprintln(w, "schema findings:"); err != nil {
				return err
			}
			for _, tool := range data.Summary.Definitions.PerTool {
				if len(tool.Findings) == 0 {
					continue
				}
				if _, err := fmt.Fprintf(w, "  %s: %s\n", tool.Name, strings.Join(tool.Findings, ", ")); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
	}
	for _, ev := range data.Events {
		title := fmt.Sprintf("#%d %s %s %s", ev.Seq, ev.Timestamp.Format(time.RFC3339Nano), ev.Direction, ev.Kind)
		if ev.Method != "" {
			title += " " + ev.Method
		}
		if ev.ID != "" {
			title += " id=" + ev.ID
		}
		if ev.Warning != "" {
			title += " warning=" + ev.Warning
		}
		if ev.Observation != "" {
			title += " observation=" + ev.Observation
		}
		if ev.Truncated {
			title += " truncated"
		}
		if ev.Deprecated != "" {
			title += " deprecated"
		}
		if ev.CacheStaleRefetch != "" {
			title += " cache_stale_refetch"
		}
		if ev.CacheTTLMs != nil || ev.CacheScope != "" {
			title += " cache"
		}
		if ev.CallIndex != nil {
			c := data.Calls[*ev.CallIndex]
			title += fmt.Sprintf(" status=%s duration_ms=%s", c.Status, formatDuration(c.DurationMS))
			if c.ToolName != "" {
				title += " tool=" + c.ToolName
			}
		}
		if _, err := fmt.Fprintln(w, title); err != nil {
			return err
		}
		if ev.Text != "" {
			if _, err := fmt.Fprintln(w, ev.Text); err != nil {
				return err
			}
		} else if len(ev.Raw) > 0 {
			var pretty bytes.Buffer
			if json.Indent(&pretty, ev.Raw, "", "  ") == nil {
				if _, err := fmt.Fprintln(w, pretty.String()); err != nil {
					return err
				}
			} else if _, err := fmt.Fprintln(w, string(ev.Raw)); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	return nil
}

func writeHTML(w io.Writer, data SessionExport) error {
	// json.Marshal deliberately, not jsonwire.Marshal. This payload goes into
	// template.JS inside a script block, and template.JS switches off the
	// contextual escaping html/template would otherwise apply. Escaping < is then
	// the only thing keeping a tool result that contains </script> from ending the
	// script element and running as markup. Preserving wire bytes is not worth a
	// stored XSS in a file people open in a browser.
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return htmlTemplate.Execute(w, struct {
		Title string
		Data  template.JS
	}{
		Title: "mcpsnoop " + data.Session.Label,
		Data:  template.JS(payload),
	})
}

// errorName is ErrorCodeName over a possibly absent error, kept next to the
// call export so the nil case is answered once rather than at each caller.
func errorName(err *proxy.RPCError) string {
	if err == nil {
		return ""
	}
	return store.ErrorCodeName(err.Code)
}

func eventKind(k store.EventKind) string {
	switch k {
	case store.EventRequest:
		return "request"
	case store.EventResponse:
		return "response"
	case store.EventNotification:
		return "notification"
	case store.EventStderr:
		return "stderr"
	case store.EventInvalid:
		return "invalid"
	case store.EventTransport:
		return "transport"
	default:
		return "other"
	}
}

func callKey(c store.CallView) string {
	return strconv.FormatUint(c.RequestSeq, 10)
}

func formatDuration(ms *float64) string {
	if ms == nil {
		return "pending"
	}
	return fmt.Sprintf("%.3f", *ms)
}

func safeFileName(s string) string {
	if s == "" {
		return "session"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "._-")
	if out == "" {
		return "session"
	}
	return out
}

func errPathNotFound(path string) error {
	return fmt.Errorf("session log %q not found", path)
}
