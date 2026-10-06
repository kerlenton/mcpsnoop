package exporter

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/jsonwire"
	"github.com/kerlenton/mcpsnoop/internal/paths"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/store"
	"github.com/kerlenton/mcpsnoop/internal/wiretext"
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
	// Elicitations is the ledger of what servers asked the user for and what they
	// answered. Absent from a session that saw none, so an ordinary export is
	// unchanged.
	Elicitations []ElicitationExport `json:"elicitations,omitempty"`
	// Interactions is one entry per logical operation, with the per-hop breakdown
	// of a multi round-trip chain. A call with no retry is a one hop interaction,
	// so this describes every operation uniformly.
	Interactions []InteractionExport `json:"interactions,omitempty"`
}

// InteractionExport is one logical operation, however many requests it took.
type InteractionExport struct {
	CallIndex *int   `json:"call_index,omitempty"`
	CallID    string `json:"call_id"`
	Method    string `json:"method"`
	ToolName  string `json:"tool_name,omitempty"`
	State     string `json:"state"`
	// RoundTrips is how many requests the operation took. ServerTimeMS is how long
	// the server held it across all of them and ClientTurnaroundMS is the rest,
	// mostly a person deciding. The two sum to DurationMS by construction.
	RoundTrips         int       `json:"round_trips"`
	ServerTimeMS       float64   `json:"server_time_ms"`
	ClientTurnaroundMS float64   `json:"client_turnaround_ms"`
	DurationMS         float64   `json:"duration_ms"`
	StartedAt          time.Time `json:"started_at"`
	// Hops is the per-hop breakdown. HopsComplete is false when the store no
	// longer holds every frame, which a live one does to stay inside its budget,
	// so a partial breakdown is never read as the whole operation.
	Hops         []HopExport `json:"hops,omitempty"`
	HopsComplete bool        `json:"hops_complete"`
}

// HopExport is one request and its answer inside an interaction.
type HopExport struct {
	RequestID  string     `json:"request_id"`
	RequestAt  time.Time  `json:"request_at"`
	ResponseAt *time.Time `json:"response_at,omitempty"`
	// ServerTimeMS is this hop alone and ClientTurnaroundMS the wait before it,
	// zero on the first since nothing preceded it.
	ServerTimeMS       float64 `json:"server_time_ms"`
	ClientTurnaroundMS float64 `json:"client_turnaround_ms"`
	// Asked names what this hop's answer asked the client for, sorted, and is
	// absent on a hop that finished the operation. AskedUnknown separates that from
	// a hop whose answer is no longer readable, which a live store produces by
	// releasing frame bodies.
	Asked        []string `json:"asked,omitempty"`
	AskedUnknown bool     `json:"asked_unknown,omitempty"`
	Pending      bool     `json:"pending,omitempty"`
}

// ElicitationExport is one question a server put to the user through the client.
//
// It carries the shape of the question and never its content. The submitted
// values stay in the log, so this document is safe to pass around whatever the
// capture was redacted with, which matters most in url mode, where the
// specification puts credentials on purpose.
type ElicitationExport struct {
	// CallIndex points into Calls, and is absent when the call it names is not in
	// this document, which a bounded live store can produce by dropping the frame
	// that opened it. A pointer rather than a sentinel: -1 is a number, and jq and
	// Python both resolve calls[-1] to the last call rather than to nothing.
	// CallID, Method and ToolName name the operation regardless.
	CallIndex *int   `json:"call_index,omitempty"`
	CallID    string `json:"call_id"`
	Method    string `json:"method"`
	ToolName  string `json:"tool_name,omitempty"`
	// Key is the inputRequests entry the question was filed under, and what paired
	// it with its answer.
	Key string `json:"key"`
	// Mode is form or url. A request naming neither is form, which is what the
	// specification tells a client to assume.
	Mode    string `json:"mode"`
	Message string `json:"message,omitempty"`
	// Fields are the form mode requestedSchema properties. A type is absent when
	// the schema declared none, which includes a subschema redaction replaced with
	// its placeholder.
	Fields []ElicitFieldExport `json:"fields,omitempty"`
	// URL is the url mode target in full, since the specification makes a client
	// show it whole, and Host is the part it is told to highlight.
	URL  string `json:"url,omitempty"`
	Host string `json:"host,omitempty"`
	// Action is what the user did. Absent means no retry ever answered, which
	// Pending states outright so a consumer does not have to infer it.
	Action     string     `json:"action,omitempty"`
	Pending    bool       `json:"pending,omitempty"`
	AskedAt    time.Time  `json:"asked_at"`
	AnsweredAt *time.Time `json:"answered_at,omitempty"`
	ElapsedMS  *float64   `json:"elapsed_ms,omitempty"`
}

type ElicitFieldExport struct {
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
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
	// AnnotationFindings names how the tool declares its behaviour, by the kinds
	// mcpsnoop check reports, malformedAnnotations included.
	AnnotationFindings []string `json:"annotation_findings,omitempty"`
}

type ToolStatsExport struct {
	Name  string `json:"name"`
	Calls int    `json:"calls"`
	// Errors is the total, and the two below split it. A tool answering isError
	// is reporting a domain outcome, a server returning a JSON-RPC error is
	// broken, and a consumer that gates on one of those should not have to guess
	// which it is looking at. They always sum to Errors.
	Errors         int     `json:"errors"`
	ProtocolErrors int     `json:"protocol_errors"`
	ToolErrors     int     `json:"tool_errors"`
	Pending        int     `json:"pending"`
	P50MS          float64 `json:"p50_ms"`
	P95MS          float64 `json:"p95_ms"`
	P99MS          float64 `json:"p99_ms"`
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
	ID    string `json:"id"`
	Label string `json:"label"`
	// Transport and Endpoint say what was captured. The label alone cannot: on
	// HTTP it defaults to the target host, so two proxies pointed at two paths of
	// one host export under the same name. Endpoint is the MCP endpoint with
	// userinfo, query values and the fragment already removed, empty on stdio and
	// on an HTTP log captured before mcpsnoop recorded it.
	Transport     string    `json:"transport,omitempty"`
	Endpoint      string    `json:"endpoint,omitempty"`
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
	// RetiredExchanges counts multi round-trip operations dropped at the parking
	// cap while still open. A non-zero value means a retry for one of them could
	// no longer be linked, so an operation may be reported here as two calls.
	RetiredExchanges int `json:"retired_exchanges,omitempty"`
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
	// RoundTrips is how many requests this operation took, and ServerTimeMS the
	// share of DurationMS the server held it for. Under multi round-trip requests
	// one call is several requests and the time a person spent answering sits
	// inside the duration, so a record carrying only the total implies the server
	// was busy for all of it. Absent on an operation with no measured hops.
	RoundTrips   int             `json:"round_trips,omitempty"`
	ServerTimeMS *float64        `json:"server_time_ms,omitempty"`
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

// LoadFileTolerant is LoadFile for a caller reading many logs at once, where one
// of them being live must not fail the run.
//
// The two differ on exactly one thing, a truncated final envelope. LoadFile
// treats it as corruption and returns an error, which is right for check and
// export: they were pointed at one capture, and a capture that ends mid-frame is
// itself the finding. A shim appending to its log right now leaves that same
// torn tail every time, so a command walking the sessions directory would fail
// whenever anything was running, which is the moment its answer matters most.
// This one stops at the tear and reports what came before it, the way
// proxy.Decode does for the live path.
//
// Reach for LoadFile whenever the log is the subject. Reach for this one only
// when it is one of many and the others still have to be read.
func LoadFileTolerant(path string) (*store.Store, string, error) {
	return loadFileTolerant(path, store.New())
}

// LoadFileTolerantBounded is LoadFileTolerant into a store that releases frame
// bodies and old frames as it reads, so peak memory is the bound rather than the
// size of the log.
//
// What survives eviction is session state, which the store folds in at ingest:
// the negotiated capabilities, the tool inventory and whether its listing
// completed, the counters, the drift verdict. What does not is the timeline and
// the call list, since those are the frames themselves. Reach for this only when
// the answer is one of the former. A caller that will read Timeline or Calls
// wants LoadFileTolerant, or it will silently read a window instead of a log.
func LoadFileTolerantBounded(path string, bodyBytes, frames int) (*store.Store, string, error) {
	return loadFileTolerant(path, store.NewBounded(bodyBytes, frames))
}

func loadFileTolerant(path string, st *store.Store) (*store.Store, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()

	var firstSession string
	if err := proxy.Decode(f, func(env proxy.Envelope) {
		if firstSession == "" {
			firstSession = env.SessionID
		}
		st.Ingest(env)
	}); err != nil {
		return nil, "", fmt.Errorf("%s: invalid JSONL envelope: %w", path, err)
	}
	if firstSession == "" {
		return nil, "", fmt.Errorf("%s: no envelopes found", path)
	}
	return st, firstSession, nil
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
	header, found := sessionHeaderOf(st, sessionID)
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

	interactions := st.Interactions(sessionID)
	// Indexed by the call each one belongs to, so a per-call record can say how
	// many requests it took and how much of its duration was the server.
	byCall := make(map[uint64]store.InteractionView, len(interactions))
	outInteractions := make([]InteractionExport, 0, len(interactions))
	for _, in := range interactions {
		byCall[in.CallSeq] = in
		row := interactionExportOf(in)
		if idx, ok := callIndex[strconv.FormatUint(in.CallSeq, 10)]; ok {
			row.CallIndex = &idx
		}
		outInteractions = append(outInteractions, row)
	}
	for i := range outCalls {
		if in, ok := byCall[calls[i].RequestSeq]; ok {
			applyInteraction(&outCalls[i], in)
		}
	}

	elicitations := st.Elicitations(sessionID)
	outElicitations := make([]ElicitationExport, 0, len(elicitations))
	for _, e := range elicitations {
		row := ElicitationExport{
			CallID: e.CallID, Method: e.Method, ToolName: e.ToolName,
			Key: e.Key, Mode: e.Mode, Message: e.Message,
			URL: e.URL, Host: e.Host,
			Action: e.Action, Pending: e.Pending(), AskedAt: e.Asked,
		}
		if idx, ok := callIndex[strconv.FormatUint(e.CallSeq, 10)]; ok {
			row.CallIndex = &idx
		}
		for _, f := range e.Fields {
			row.Fields = append(row.Fields, ElicitFieldExport{Name: f.Name, Type: f.Type})
		}
		if !e.Answered.IsZero() {
			answered := e.Answered
			elapsed := durationMS(e.Elapsed)
			row.AnsweredAt, row.ElapsedMS = &answered, &elapsed
		}
		outElicitations = append(outElicitations, row)
	}

	out := SessionExport{
		GeneratedAt:  time.Now().UTC(),
		Session:      sessionSummaryOf(header),
		Calls:        outCalls,
		Events:       outEvents,
		Elicitations: outElicitations,
		Interactions: outInteractions,
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
	out.Capabilities = capabilitiesOf(st, sessionID)
	return out, nil
}

// OperationOTLP writes one operation as OTLP JSON, built by the same code as
// WriteOTLP. The live sink calls it when a response finishes an operation, so a
// push to a collector and an exported file describe that operation as the same
// spans with the same ids. It reads one operation from the store rather than
// building the whole session, because the sink asks once per finished
// operation and a whole-session build would make every answer cost all of them.
func OperationOTLP(w io.Writer, st *store.Store, sessionID string, op store.CallView) error {
	header, found := sessionHeaderOf(st, sessionID)
	if !found {
		return fmt.Errorf("session %q not found", sessionID)
	}
	data := SessionExport{
		GeneratedAt:  time.Now().UTC(),
		Session:      sessionSummaryOf(header),
		Calls:        []CallExport{exportCall(0, op)},
		Capabilities: capabilitiesOf(st, sessionID),
	}
	if in, ok := st.Interaction(sessionID, op.RequestSeq); ok {
		zero := 0
		row := interactionExportOf(in)
		row.CallIndex = &zero
		data.Interactions = []InteractionExport{row}
		applyInteraction(&data.Calls[0], in)
	}
	return WriteOTLP(w, data)
}

func sessionHeaderOf(st *store.Store, sessionID string) (store.SessionHeader, bool) {
	for _, h := range st.Sessions() {
		if h.ID == sessionID {
			return h, true
		}
	}
	return store.SessionHeader{}, false
}

func sessionSummaryOf(header store.SessionHeader) SessionSummary {
	return SessionSummary{
		ID:               header.ID,
		Label:            header.Label,
		Transport:        header.Transport,
		Endpoint:         header.Endpoint,
		First:            header.First,
		Last:             header.Last,
		Requests:         header.Requests,
		Responses:        header.Responses,
		Notifications:    header.Notifications,
		Errors:           header.Errors,
		Pending:          header.Pending,
		LateResults:      header.LateResults,
		MissingFrames:    header.MissingFrames,
		RetiredExchanges: header.RetiredExchanges,
	}
}

func capabilitiesOf(st *store.Store, sessionID string) *CapabilitiesExport {
	caps, ok := st.Capabilities(sessionID)
	if !ok {
		return nil
	}
	return &CapabilitiesExport{
		ProtocolVersion: caps.ProtocolVersion,
		ClientInfo:      caps.ClientInfo,
		ServerInfo:      caps.ServerInfo,
		Client:          caps.Client,
		Server:          caps.Server,
		Instructions:    caps.Instructions,
	}
}

func interactionExportOf(in store.InteractionView) InteractionExport {
	row := InteractionExport{
		CallID: in.CallID, Method: in.Method, ToolName: in.ToolName,
		State:        in.State.String(),
		RoundTrips:   in.RoundTrips,
		ServerTimeMS: durationMS(in.ServerTime), ClientTurnaroundMS: durationMS(in.ClientTurnaround),
		DurationMS: durationMS(in.Duration), StartedAt: in.Start,
		HopsComplete: in.HopsComplete,
	}
	for _, h := range in.Hops {
		hop := HopExport{
			RequestID: h.RequestID, RequestAt: h.RequestAt,
			ServerTimeMS: durationMS(h.ServerTime), ClientTurnaroundMS: durationMS(h.ClientTurnaround),
			Asked: h.Asked, AskedUnknown: h.AskedUnknown, Pending: h.Pending,
		}
		if !h.ResponseAt.IsZero() {
			at := h.ResponseAt
			hop.ResponseAt = &at
		}
		row.Hops = append(row.Hops, hop)
	}
	return row
}

// applyInteraction gives a call the counts that only its interaction knows.
func applyInteraction(c *CallExport, in store.InteractionView) {
	c.RoundTrips = in.RoundTrips
	if in.ServerTime > 0 || in.RoundTrips > 1 {
		ms := durationMS(in.ServerTime)
		c.ServerTimeMS = &ms
	}
}

func exportToolSummary(summary store.SessionToolSummary) ToolSummaryExport {
	out := ToolSummaryExport{
		Tools:        make([]ToolStatsExport, 0, len(summary.Tools)),
		SlowestCalls: make([]SlowCallExport, 0, len(summary.Slowest)),
	}
	for _, tool := range summary.Tools {
		out.Tools = append(out.Tools, ToolStatsExport{
			Name: tool.Name, Calls: tool.Calls, Errors: tool.Errors,
			ProtocolErrors: tool.ProtocolErrors, ToolErrors: tool.ToolErrors, Pending: tool.Pending,
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
		var annotations []string
		for _, kind := range tool.AnnotationKinds {
			annotations = append(annotations, string(kind))
		}
		out.PerTool = append(out.PerTool, ToolCostExport{
			Name:               tool.Name,
			Bytes:              tool.Bytes,
			DescriptionBytes:   tool.DescriptionBytes,
			SchemaBytes:        tool.SchemaBytes,
			Findings:           findings,
			AnnotationFindings: annotations,
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
	// Message is the status description. The MCP span convention says it SHOULD
	// match JSONRPCError.message when the status is ERROR, which is the one place
	// a reader of the trace learns what the server actually said.
	Message string `json:"message,omitempty"`
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
	// ArrayValue carries a list, which is what lets a backend filter on one member
	// of it rather than on a joined string it would have to split.
	ArrayValue *otlpArrayValue `json:"arrayValue,omitempty"`
}

type otlpArrayValue struct {
	Values []otlpAnyValue `json:"values"`
}

// WriteOTLP writes data using the OTLP JSON encoding.
func WriteOTLP(w io.Writer, data SessionExport) error {
	sessionTraceID := otlpID(16, "trace", data.Session.ID)
	// An interaction names its call by index, and only one that took more than a
	// single request has hops to render.
	byCall := make(map[int]*InteractionExport, len(data.Interactions))
	for i := range data.Interactions {
		if idx := data.Interactions[i].CallIndex; idx != nil {
			byCall[*idx] = &data.Interactions[i]
		}
	}
	spans := make([]otlpSpan, 0, len(data.Calls))
	for i, call := range data.Calls {
		spans = append(spans, callSpans(call, byCall[i], data, sessionTraceID)...)
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

// mcpSemconvAttrs is the OpenTelemetry semantic-convention surface for one MCP
// call. These names, rather than mcpsnoop's own, are what make a capture land in
// an off-the-shelf MCP dashboard instead of needing one built for this tool.
//
// Verified against the model in open-telemetry/semantic-conventions-genai,
// model/mcp/{registry,common,spans}.yaml, at Development stability. mcp.method.name
// is the only required attribute; the rest are conditional on data the capture
// may not hold, and each condition below is the one the model states.
//
// mcp.session.id is deliberately absent. It identifies a connection-scoped MCP
// session, and revision 2026-07-28 removed that concept, so a current capture has
// nothing to put there. A legacy capture had the header but mcpsnoop never
// recorded it. Writing mcpsnoop's own capture id there would be a different
// identifier wearing the convention's name.
//
// gen_ai.tool.call.arguments, gen_ai.tool.call.result and gen_ai.prompt.variable.*
// are opt-in in the model and carry payloads the model itself flags as possibly
// sensitive. They stay out until a flag asks for them, since an export is a file
// people hand around.
func mcpSemconvAttrs(call CallExport, data SessionExport) []otlpAttribute {
	return mcpAttrs(call, data, mcpAttrOptions{requestID: call.ID, operation: true, outcome: true})
}

// mcpAttrOptions says which request a span covers and what it stands for. A
// call that took one request is all three at once. A hop of a multi round-trip
// operation is one request among several and stands for neither the tool
// execution nor, unless it is the last, the outcome.
type mcpAttrOptions struct {
	requestID string
	operation bool
	outcome   bool
}

func mcpAttrs(call CallExport, data SessionExport, o mcpAttrOptions) []otlpAttribute {
	attrs := []otlpAttribute{otlpString("mcp.method.name", call.Method)}
	if call.ToolName != "" {
		attrs = append(attrs, otlpString("gen_ai.tool.name", call.ToolName))
	}
	// Set for a tool call and for nothing else, which is what lets a consumer
	// treat an MCP tool call like any other tool call without the attribute
	// claiming an operation kind for a handshake or a listing.
	if call.IsTool && o.operation {
		attrs = append(attrs, otlpString("gen_ai.operation.name", "execute_tool"))
	}
	if name := mcpPromptName(call); name != "" {
		attrs = append(attrs, otlpString("gen_ai.prompt.name", name))
	}
	if uri := mcpResourceURI(call); uri != "" {
		attrs = append(attrs, otlpString("mcp.resource.uri", uri))
	}
	// A string, and left out for a null or absent id, both as the registry says.
	if o.requestID != "" && o.requestID != "null" {
		attrs = append(attrs, otlpString("jsonrpc.request.id", o.requestID))
	}
	if o.outcome {
		if call.Error != nil {
			// rpc.response.status_code is a string across every RPC system, so the
			// JSON-RPC code goes in as decimal text. A bare number here is the
			// mistake that makes a payload look right and get rejected at the
			// collector.
			attrs = append(attrs, otlpString("rpc.response.status_code", strconv.Itoa(call.Error.Code)))
		}
		if t := mcpErrorType(call); t != "" {
			attrs = append(attrs, otlpString("error.type", t))
		}
	}
	if data.Capabilities != nil && data.Capabilities.ProtocolVersion != "" {
		attrs = append(attrs, otlpString("mcp.protocol.version", data.Capabilities.ProtocolVersion))
	}
	if t := mcpNetworkTransport(data.Session.Transport); t != "" {
		attrs = append(attrs, otlpString("network.transport", t))
	}
	if host, port := mcpServerAddress(data.Session.Endpoint); host != "" {
		attrs = append(attrs, otlpString("server.address", host))
		if port > 0 {
			attrs = append(attrs, otlpInt("server.port", int64(port)))
		}
	}
	return attrs
}

// mcpSpanName follows the convention, "{mcp.method.name} {target}", where target
// is the tool or prompt name when the call names one. The resource URI is left
// out on purpose: the model says instrumentation SHOULD NOT use it as the target
// by default, because a URI per span makes span names high cardinality and a
// tracing backend groups by name.
func mcpSpanName(call CallExport) string {
	if call.ToolName != "" {
		return call.Method + " " + call.ToolName
	}
	if name := mcpPromptName(call); name != "" {
		return call.Method + " " + name
	}
	return call.Method
}

// mcpErrorType is error.type, which the model requires if and only if the
// operation failed, so this returns empty for everything the span does not mark
// ERROR. A tool error is a JSON-RPC call that succeeded while the result said it
// did not, and the model names that exact case "tool_error".
func mcpErrorType(call CallExport) string {
	switch {
	case call.ToolError:
		return "tool_error"
	case call.Error != nil:
		return strconv.Itoa(call.Error.Code)
	case call.IsError:
		// The store settled it as failed without a JSON-RPC error object, which a
		// terminal task failure can do. _OTHER is the registry's fallback for an
		// error with no lower-cardinality name.
		return "_OTHER"
	}
	return ""
}

// statusMessage is the span status description, which the model says SHOULD match
// JSONRPCError.message when the status is ERROR. A tool error carries its text
// inside the result rather than in an error object, so there is nothing to quote.
func statusMessage(status string, call CallExport) string {
	if status != "STATUS_CODE_ERROR" || call.Error == nil {
		return ""
	}
	return call.Error.Message
}

// mcpNetworkTransport maps the captured transport onto network.transport, which
// the model pins to "pipe" for stdio and to the HTTP carrier otherwise. Empty for
// a legacy log whose frames named no transport, since guessing would assert
// something the capture does not say.
func mcpNetworkTransport(transport string) string {
	switch transport {
	case string(proxy.TransportStdio):
		return "pipe"
	case string(proxy.TransportHTTP):
		return "tcp"
	}
	return ""
}

// mcpResourceURI is the resource URI for the request types the model lists as
// carrying one. Any other method is left alone, so a tools/call argument named
// uri never reads as a resource.
func mcpResourceURI(call CallExport) string {
	switch call.Method {
	case "resources/read", "resources/subscribe", "resources/unsubscribe", "notifications/resources/updated":
		return paramString(call.Params, "uri")
	}
	return ""
}

// mcpPromptName is the prompt a call names, read only for prompts/get. tools/call
// also puts a name in params, and reading it generically would file a tool under
// gen_ai.prompt.name.
func mcpPromptName(call CallExport) string {
	if call.Method != "prompts/get" {
		return ""
	}
	return paramString(call.Params, "name")
}

// paramString reads one top-level string parameter, and returns empty for
// anything that is not a string, since the value goes out as a string attribute.
func paramString(params json.RawMessage, key string) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(params, &fields) != nil {
		return ""
	}
	var value string
	if raw, ok := fields[key]; !ok || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

// mcpServerAddress splits the captured endpoint into server.address and
// server.port. The port is the one the request actually went to, so a URL that
// left it implicit resolves to its scheme's default rather than reporting none.
// Empty host for a stdio capture, which has no endpoint.
func mcpServerAddress(endpoint string) (string, int) {
	if endpoint == "" {
		return "", 0
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return "", 0
	}
	if p := u.Port(); p != "" {
		port, err := strconv.Atoi(p)
		if err != nil {
			return u.Hostname(), 0
		}
		return u.Hostname(), port
	}
	switch u.Scheme {
	case "https":
		return u.Hostname(), 443
	case "http":
		return u.Hostname(), 80
	}
	return u.Hostname(), 0
}

// spanContext is where a span hangs in its trace.
type spanContext struct {
	traceID, parent, state string
}

// callSpans renders one operation. One that took a single request is one
// mcp.client span, as it always was.
//
// One that took several, which multi round-trip requests make ordinary, is a
// span per request. The convention defines mcp.client as a request and the wait
// for its answer, and the specification says a retry is an independent request
// with its own id, so one span stretched over all of them would carry only the
// first id and fold the time a person spent answering into what reads as the
// server's latency. A tool call among those also gets the GenAI execute_tool span
// above its hops, covering the whole operation, so a backend sees the server's
// time on the hops and the end-to-end time on the parent, and neither pollutes
// the other.
func callSpans(call CallExport, in *InteractionExport, data SessionExport, sessionTraceID string) []otlpSpan {
	ctx := spanContext{traceID: sessionTraceID}
	if propagated, ok := traceContext(call.Params); ok {
		ctx = spanContext{traceID: propagated.TraceID, parent: propagated.ParentSpanID, state: propagated.TraceState}
	}
	if in == nil || in.RoundTrips < 2 || !in.HopsComplete {
		return []otlpSpan{singleCallSpan(call, in, data, ctx)}
	}
	spans := make([]otlpSpan, 0, len(in.Hops)+1)
	hopCtx := ctx
	// execute_tool requires the tool's name, so a tools/call that never named one
	// keeps its hops and goes without a parent rather than inventing one.
	underTool := call.IsTool && call.ToolName != ""
	if underTool {
		parent := executeToolSpan(call, in, data, ctx)
		spans = append(spans, parent)
		hopCtx = spanContext{traceID: ctx.traceID, parent: parent.SpanID, state: ctx.state}
	}
	for i, h := range in.Hops {
		spans = append(spans, hopSpan(call, in, h, i, data, hopCtx, underTool))
	}
	return spans
}

func singleCallSpan(call CallExport, in *InteractionExport, data SessionExport, ctx spanContext) otlpSpan {
	end := callEnd(call)
	attrs := mcpSemconvAttrs(call, data)
	attrs = append(attrs, rpcCompatAttrs(call)...)
	attrs = append(attrs, logicalCallAttrs(call)...)
	// Several requests whose breakdown the store could no longer give in full,
	// which a live store that released old frames produces. One span is the honest
	// shape when the hops are unknown, and the split still comes along because
	// the store accumulates it as frames arrive, so it is exact either way.
	if in != nil && in.RoundTrips > 1 {
		attrs = append(attrs, roundTripAttrs(in)...)
		attrs = append(attrs, otlpBool("mcpsnoop.call.hops_complete", in.HopsComplete))
	}
	status := callStatus(call)
	// Every span here is the initiator-side one the convention calls mcp.client,
	// which it fixes at kind client whichever peer initiated, because what is
	// measured is the wait for the peer's reply. mcpsnoop is on the wire rather
	// than inside either peer, so it has no receiver-side timing to report and
	// emits no mcp.server span. Under 2026-07-28 a server cannot initiate a
	// request at all, so the server-initiated case is legacy captures only.
	return otlpSpan{
		TraceID:           ctx.traceID,
		SpanID:            otlpID(8, "span", data.Session.ID, call.ID, string(call.Direction)),
		ParentSpanID:      ctx.parent,
		TraceState:        ctx.state,
		Name:              mcpSpanName(call),
		Kind:              "SPAN_KIND_CLIENT",
		StartTimeUnixNano: fmt.Sprint(call.StartedAt.UnixNano()),
		EndTimeUnixNano:   fmt.Sprint(end.UnixNano()),
		Attributes:        attrs,
		Status:            otlpStatus{Code: status, Message: statusMessage(status, call)},
	}
}

// executeToolSpan is the GenAI span for one tool execution, which under multi
// round-trip requests spans several requests and the time a person spent
// answering between them. It follows gen_ai.execute_tool.internal in the
// semantic-conventions-genai model: named execute_tool {gen_ai.tool.name}, kind
// internal, gen_ai.operation.name required.
//
// It carries no mcp.method.name on purpose. Its hops do, and a backend counting
// tools/call by that attribute would otherwise count every operation once more.
func executeToolSpan(call CallExport, in *InteractionExport, data SessionExport, ctx spanContext) otlpSpan {
	end := callEnd(call)
	attrs := []otlpAttribute{
		otlpString("gen_ai.operation.name", "execute_tool"),
		otlpString("gen_ai.tool.name", call.ToolName),
	}
	if t := mcpErrorType(call); t != "" {
		attrs = append(attrs, otlpString("error.type", t))
	}
	attrs = append(attrs, logicalCallAttrs(call)...)
	attrs = append(attrs, roundTripAttrs(in)...)
	status := callStatus(call)
	return otlpSpan{
		TraceID:           ctx.traceID,
		SpanID:            otlpID(8, "span", data.Session.ID, call.ID, string(call.Direction), "execute_tool"),
		ParentSpanID:      ctx.parent,
		TraceState:        ctx.state,
		Name:              "execute_tool " + call.ToolName,
		Kind:              "SPAN_KIND_INTERNAL",
		StartTimeUnixNano: fmt.Sprint(call.StartedAt.UnixNano()),
		EndTimeUnixNano:   fmt.Sprint(end.UnixNano()),
		Attributes:        attrs,
		Status:            otlpStatus{Code: status, Message: statusMessage(status, call)},
	}
}

// hopSpan is one request of a multi round-trip operation and its answer, which
// is exactly what the convention means by an mcp.client span. Each carries its
// own JSON-RPC id and its own duration, the server's time for that request
// alone. Only the last carries the operation's outcome, since the earlier ones
// were answered with input_required, which is a request that succeeded.
//
// None carries gen_ai.operation.name. The convention asks for it on a span that
// describes a tool call, and the MCP span model asks that one tool execution not
// become two spans. Under an execute_tool parent the parent is that execution,
// and repeating the name on every hop would count one execution as many.
func hopSpan(call CallExport, in *InteractionExport, h HopExport, i int, data SessionExport, ctx spanContext, underTool bool) otlpSpan {
	last := i == len(in.Hops)-1
	end := h.RequestAt
	switch {
	case h.ResponseAt != nil:
		end = *h.ResponseAt
	case last && call.CancelledAt != nil:
		// The client stopped waiting here, which is where this request ended.
		end = *call.CancelledAt
	}
	attrs := mcpAttrs(call, data, mcpAttrOptions{requestID: h.RequestID, outcome: last})
	attrs = append(attrs, rpcCompatAttrs(call)...)
	attrs = append(attrs, otlpInt("mcpsnoop.hop.index", int64(i)))
	if len(h.Asked) > 0 {
		attrs = append(attrs, otlpStrings("mcpsnoop.hop.asked", h.Asked))
	}
	if h.AskedUnknown {
		attrs = append(attrs, otlpBool("mcpsnoop.hop.asked_unknown", true))
	}
	// The wait before this request, which is mostly a person deciding, and the
	// gap a trace view draws between two hops without saying what it was.
	if h.ClientTurnaroundMS > 0 {
		attrs = append(attrs, otlpDouble("mcpsnoop.hop.client_turnaround_ms", h.ClientTurnaroundMS))
	}
	// With no execute_tool span above them, the last hop is the one place the
	// operation as a whole can be described, and that description already names
	// the call. Every other hop names it alone, which is what groups them, and an
	// attribute key may appear once on a span.
	if last && !underTool {
		attrs = append(attrs, logicalCallAttrs(call)...)
		attrs = append(attrs, roundTripAttrs(in)...)
	} else {
		attrs = append(attrs, otlpString("mcpsnoop.call.id", call.ID))
	}
	status, message := "STATUS_CODE_OK", ""
	switch {
	case h.Pending:
		status = "STATUS_CODE_UNSET"
	case last:
		status = callStatus(call)
		message = statusMessage(status, call)
	}
	return otlpSpan{
		TraceID:           ctx.traceID,
		SpanID:            otlpID(8, "span", data.Session.ID, call.ID, string(call.Direction), "hop", strconv.Itoa(i), h.RequestID),
		ParentSpanID:      ctx.parent,
		TraceState:        ctx.state,
		Name:              mcpSpanName(call),
		Kind:              "SPAN_KIND_CLIENT",
		StartTimeUnixNano: fmt.Sprint(h.RequestAt.UnixNano()),
		EndTimeUnixNano:   fmt.Sprint(end.UnixNano()),
		Attributes:        attrs,
		Status:            otlpStatus{Code: status, Message: message},
	}
}

// rpcCompatAttrs are the generic RPC conventions, kept beside the MCP ones. They
// are not part of the MCP span definition and rpc.method restates
// mcp.method.name, but an existing dashboard may be keyed on them, so they go
// when a release can say they went.
func rpcCompatAttrs(call CallExport) []otlpAttribute {
	return []otlpAttribute{
		otlpString("rpc.system", "mcp"),
		otlpString("rpc.method", call.Method),
	}
}

// logicalCallAttrs describe the operation rather than any one request in it.
func logicalCallAttrs(call CallExport) []otlpAttribute {
	attrs := []otlpAttribute{
		otlpString("mcpsnoop.call.id", call.ID),
		otlpString("mcpsnoop.call.status", call.Status),
		otlpString("mcpsnoop.call.state", call.State),
		otlpBool("mcpsnoop.call.is_tool", call.IsTool),
		otlpBool("mcpsnoop.call.is_error", call.IsError),
		otlpBool("mcpsnoop.call.tool_error", call.ToolError),
		// Which peer opened the exchange. The span kind does not carry it, because
		// the convention ties kind to the span type rather than to the direction of
		// travel, and mcpsnoop always reports the initiator side.
		otlpString("mcpsnoop.call.direction", string(call.Direction)),
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
	return attrs
}

// roundTripAttrs split the operation's duration into the server's share and the
// client's, which sum to the whole by construction in the store.
func roundTripAttrs(in *InteractionExport) []otlpAttribute {
	return []otlpAttribute{
		otlpInt("mcpsnoop.call.round_trips", int64(in.RoundTrips)),
		otlpDouble("mcpsnoop.call.server_time_ms", in.ServerTimeMS),
		otlpDouble("mcpsnoop.call.client_turnaround_ms", in.ClientTurnaroundMS),
	}
}

// callEnd is when the operation ended for the client. A cancelled call that got
// no answer ended when the client gave up, and that interval is the hang a
// cancellation usually reports, so ending its span where it began would show a
// request that cost nothing.
func callEnd(call CallExport) time.Time {
	switch {
	case call.EndedAt != nil:
		return *call.EndedAt
	case call.CancelledAt != nil && call.CancelledAt.After(call.StartedAt):
		return *call.CancelledAt
	}
	return call.StartedAt
}

func callStatus(call CallExport) string {
	status := "STATUS_CODE_OK"
	if call.State == "pending" || call.State == "superseded" || call.Status == "call_cancelled" || call.Status == "late_result" {
		status = "STATUS_CODE_UNSET"
	}
	if call.IsError {
		status = "STATUS_CODE_ERROR"
	}
	return status
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

func otlpStrings(key string, values []string) otlpAttribute {
	out := make([]otlpAnyValue, 0, len(values))
	for _, v := range values {
		out = append(out, otlpAnyValue{StringValue: &v})
	}
	return otlpAttribute{Key: key, Value: otlpAnyValue{ArrayValue: &otlpArrayValue{Values: out}}}
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
	if len(data.Interactions) > 0 {
		chained := 0
		for _, in := range data.Interactions {
			if in.RoundTrips > 1 {
				chained++
			}
		}
		if chained > 0 {
			if _, err := fmt.Fprintln(w, "interactions:"); err != nil {
				return err
			}
			for _, in := range data.Interactions {
				if in.RoundTrips <= 1 {
					continue // an ordinary call is already a line above
				}
				who := in.Method
				if in.ToolName != "" {
					who = in.Method + " " + in.ToolName
				}
				if _, err := fmt.Fprintf(w, "  %s: %d round trips, %s total, %s server, %s client\n",
					wiretext.OneLine(who), in.RoundTrips,
					msDuration(in.DurationMS), msDuration(in.ServerTimeMS), msDuration(in.ClientTurnaroundMS)); err != nil {
					return err
				}
				for i, hop := range in.Hops {
					asked := ""
					if len(hop.Asked) > 0 {
						asked = "  asked " + wiretext.OneLine(strings.Join(hop.Asked, ", "))
					}
					state := ""
					if hop.Pending {
						state = "  (no answer)"
					}
					if _, err := fmt.Fprintf(w, "    hop %d id=%s  %s server", i+1, wiretext.OneLine(hop.RequestID), msDuration(hop.ServerTimeMS)); err != nil {
						return err
					}
					if hop.ClientTurnaroundMS > 0 {
						if _, err := fmt.Fprintf(w, ", %s waiting on the client", msDuration(hop.ClientTurnaroundMS)); err != nil {
							return err
						}
					}
					if _, err := fmt.Fprintf(w, "%s%s\n", asked, state); err != nil {
						return err
					}
				}
				if !in.HopsComplete {
					if _, err := fmt.Fprintf(w, "    (%d of %d hops still held, the rest were released to stay inside the memory budget)\n",
						len(in.Hops), in.RoundTrips); err != nil {
						return err
					}
				}
			}
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
	}
	if len(data.Elicitations) > 0 {
		if _, err := fmt.Fprintln(w, "elicitations:"); err != nil {
			return err
		}
		for _, e := range data.Elicitations {
			if _, err := fmt.Fprintf(w, "  %s\n", elicitationLine(e)); err != nil {
				return err
			}
			// The message is what the server wrote for a human to read, so it is on
			// every row rather than standing in for the fields when there are none.
			if e.Message != "" {
				if _, err := fmt.Fprintf(w, "    %s\n", wiretext.OneLine(e.Message)); err != nil {
					return err
				}
			}
			if detail := elicitationDetail(e); detail != "" {
				if _, err := fmt.Fprintf(w, "    %s\n", detail); err != nil {
					return err
				}
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	if data.Summary.Definitions != nil {
		for _, section := range []struct {
			title string
			kinds func(ToolCostExport) []string
		}{
			{"schema findings:", func(tool ToolCostExport) []string { return tool.Findings }},
			{"annotation findings:", func(tool ToolCostExport) []string { return tool.AnnotationFindings }},
		} {
			if err := writeToolFindings(w, data.Summary.Definitions.PerTool, section.title, section.kinds); err != nil {
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

// elicitationLine is the headline of one ledger row: who asked, in which mode,
// what the user did and how long they took.
func elicitationLine(e ElicitationExport) string {
	who := e.Method
	if e.ToolName != "" {
		who = e.Method + " " + e.ToolName
	}
	answer := "pending, no retry ever answered"
	if !e.Pending {
		answer = e.Action
		if e.ElapsedMS != nil {
			answer += fmt.Sprintf(" after %s", time.Duration(*e.ElapsedMS*float64(time.Millisecond)).Round(time.Millisecond))
		}
	}
	return fmt.Sprintf("%s [%s] %s: %s", wiretext.OneLine(who), wiretext.OneLine(e.Mode), wiretext.OneLine(e.Key), answer)
}

// elicitationDetail is what was asked for. A form names its fields and their
// declared types, a url names the address whole and the host to look at, since
// the specification tells a client to show one and highlight the other.
func elicitationDetail(e ElicitationExport) string {
	switch {
	case e.URL != "":
		out := wiretext.OneLine(e.URL)
		if e.Host != "" {
			out += " (host " + wiretext.OneLine(e.Host) + ")"
		}
		return out
	case len(e.Fields) > 0:
		parts := make([]string, 0, len(e.Fields))
		for _, f := range e.Fields {
			typ := f.Type
			if typ == "" {
				typ = "unknown"
			}
			parts = append(parts, wiretext.OneLine(f.Name)+" "+wiretext.OneLine(typ))
		}
		return strings.Join(parts, ", ")
	default:
		return ""
	}
}

// writeToolFindings writes one per-tool findings section of the text export, or
// nothing when no tool has any. A tool name came from the server, so it goes
// through wiretext.OneLine like every other wire value printed here.
func writeToolFindings(w io.Writer, tools []ToolCostExport, title string, kinds func(ToolCostExport) []string) error {
	if !slices.ContainsFunc(tools, func(tool ToolCostExport) bool { return len(kinds(tool)) > 0 }) {
		return nil
	}
	if _, err := fmt.Fprintln(w, title); err != nil {
		return err
	}
	for _, tool := range tools {
		if found := kinds(tool); len(found) > 0 {
			if _, err := fmt.Fprintf(w, "  %s: %s\n", wiretext.OneLine(tool.Name), strings.Join(found, ", ")); err != nil {
				return err
			}
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

// msDuration renders a millisecond figure the way the rest of the text export
// renders a duration, so a reader is not comparing two spellings.
func msDuration(ms float64) string {
	return time.Duration(ms * float64(time.Millisecond)).Round(time.Millisecond).String()
}
