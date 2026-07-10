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
	"strings"
	"time"

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
)

type Options struct {
	Format Format
}

type SessionExport struct {
	GeneratedAt  time.Time           `json:"generated_at"`
	Session      SessionSummary      `json:"session"`
	Capabilities *CapabilitiesExport `json:"capabilities,omitempty"`
	Calls        []CallExport        `json:"calls"`
	Events       []EventExport       `json:"events"`
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
}

type CapabilitiesExport struct {
	ProtocolVersion string          `json:"protocol_version,omitempty"`
	ClientInfo      json.RawMessage `json:"client_info,omitempty"`
	ServerInfo      json.RawMessage `json:"server_info,omitempty"`
	Client          json.RawMessage `json:"client,omitempty"`
	Server          json.RawMessage `json:"server,omitempty"`
}

type CallExport struct {
	Index      int             `json:"index"`
	ID         string          `json:"id"`
	Method     string          `json:"method"`
	Direction  proxy.Direction `json:"direction"`
	State      string          `json:"state"`
	Status     string          `json:"status"`
	IsTool     bool            `json:"is_tool"`
	ToolName   string          `json:"tool_name,omitempty"`
	IsError    bool            `json:"is_error"`
	ToolError  bool            `json:"tool_error"`
	StartedAt  time.Time       `json:"started_at"`
	EndedAt    *time.Time      `json:"ended_at,omitempty"`
	DurationMS *float64        `json:"duration_ms,omitempty"`
	Params     json.RawMessage `json:"params,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      *proxy.RPCError `json:"error,omitempty"`
}

type EventExport struct {
	Seq       uint64          `json:"seq"`
	Timestamp time.Time       `json:"timestamp"`
	Direction proxy.Direction `json:"direction"`
	Kind      string          `json:"kind"`
	Method    string          `json:"method,omitempty"`
	ID        string          `json:"id,omitempty"`
	Warning   string          `json:"warning,omitempty"`
	CallIndex *int            `json:"call_index,omitempty"`
	Raw       json.RawMessage `json:"raw,omitempty"`
	Text      string          `json:"text,omitempty"`
}

func ParseFormat(s string) (Format, error) {
	switch Format(strings.ToLower(strings.TrimSpace(s))) {
	case FormatJSON:
		return FormatJSON, nil
	case FormatHTML:
		return FormatHTML, nil
	case FormatText:
		return FormatText, nil
	case FormatOTLP:
		return FormatOTLP, nil
	default:
		return "", fmt.Errorf("unknown export format %q (want json, html, text, or otlp)", s)
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
	if format == FormatText {
		ext = "txt"
	}
	return filepath.Join(paths.ExportsDir(), safeFileName(sessionID)+"."+ext)
}

func LoadFile(path string) (*store.Store, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()

	st := store.New(0)
	var firstSession string
	dec := json.NewDecoder(f)
	for {
		var env proxy.Envelope
		if err := dec.Decode(&env); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, "", fmt.Errorf("%s: invalid JSONL envelope: %w", path, err)
		}
		if firstSession == "" {
			firstSession = env.SessionID
		}
		st.Ingest(env)
	}
	if firstSession == "" {
		return nil, "", fmt.Errorf("%s: no envelopes found", path)
	}
	return st, firstSession, nil
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
		},
		Calls:  outCalls,
		Events: outEvents,
	}
	if caps, ok := st.Capabilities(sessionID); ok {
		out.Capabilities = &CapabilitiesExport{
			ProtocolVersion: caps.ProtocolVersion,
			ClientInfo:      caps.ClientInfo,
			ServerInfo:      caps.ServerInfo,
			Client:          caps.Client,
			Server:          caps.Server,
		}
	}
	return out, nil
}

func Write(w io.Writer, data SessionExport, opts Options) error {
	format := opts.Format
	if format == "" {
		format = FormatJSON
	}
	switch format {
	case FormatJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(data)
	case FormatHTML:
		return writeHTML(w, data)
	case FormatText:
		return writeText(w, data)
	case FormatOTLP:
		return writeOTLP(w, data)
	default:
		return fmt.Errorf("unknown export format %q", format)
	}
}

// otlpExport follows the OTLP JSON encoding. Each correlated MCP call becomes
// a span in one trace, making a session importable into tracing backends.
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
}

func writeOTLP(w io.Writer, data SessionExport) error {
	traceID := otlpID(16, "trace", data.Session.ID)
	spans := make([]otlpSpan, 0, len(data.Calls))
	for _, call := range data.Calls {
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
		status := "STATUS_CODE_OK"
		if call.State == "pending" {
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
		}},
		ScopeSpans: []otlpScopeSpans{{Scope: otlpScope{Name: "mcpsnoop"}, Spans: spans}},
	}}}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
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

func ExportFile(inputPath string, w io.Writer, opts Options) error {
	st, sessionID, err := LoadFile(inputPath)
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
	if c.State == store.Pending {
		status = "pending"
	} else if c.Failed() {
		status = "error"
	}
	out := CallExport{
		Index:     index,
		ID:        c.ID,
		Method:    c.Method,
		Direction: c.ReqDir,
		State:     c.State.String(),
		Status:    status,
		IsTool:    c.IsTool,
		ToolName:  c.ToolName,
		IsError:   c.Failed(),
		ToolError: c.ToolErr,
		StartedAt: c.Start,
		Params:    c.Params,
		Result:    c.Result,
		Error:     c.Err,
	}
	if c.Done() {
		end := c.End
		dur := float64(c.End.Sub(c.Start)) / float64(time.Millisecond)
		out.EndedAt = &end
		out.DurationMS = &dur
	}
	return out
}

func exportEvent(ev store.EventView, callIndex map[string]int) EventExport {
	out := EventExport{
		Seq:       ev.Seq,
		Timestamp: ev.TS,
		Direction: ev.Dir,
		Kind:      eventKind(ev.Kind),
		Method:    ev.Method,
		ID:        ev.ID,
		Warning:   ev.Warning,
		Raw:       ev.Raw,
		Text:      ev.Text,
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
	_, err = fmt.Fprintf(w, "frames: %d  calls: %d  requests: %d  responses: %d  errors: %d  pending: %d\n\n",
		len(data.Events), len(data.Calls), data.Session.Requests, data.Session.Responses, data.Session.Errors, data.Session.Pending)
	if err != nil {
		return err
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
	default:
		return "other"
	}
}

func callKey(c store.CallView) string {
	return string(c.ReqDir) + "\x00" + c.ID
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
