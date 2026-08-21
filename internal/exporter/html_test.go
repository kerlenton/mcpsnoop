package exporter

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// TestHTMLFilterFindsTruncatedUnderWarn checks the HTML status filter agrees with
// the TUI: a truncated frame matches status:warn there too. The filter runs in the
// browser, so assert the data carries the flag and matchStatus keys on it.
func TestHTMLFilterFindsTruncatedUnderWarn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trunc.jsonl")
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 1, TS: time.Now(),
		Direction: proxy.ServerToClient, Truncated: true,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","result":{}}`),
	})
	st, id, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Build(st, id)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, out, Options{Format: FormatHTML}); err != nil {
		t.Fatal(err)
	}
	html := buf.String()

	if !strings.Contains(html, `"truncated":true`) {
		t.Fatal("embedded data is missing the truncated flag")
	}
	if !strings.Contains(html, `return !!ev.warning || !!ev.truncated`) {
		t.Fatal("the HTML status:warn filter does not match truncated frames like the TUI")
	}
}

// TestHTMLMarksTruncatedEvent checks the export marks a capped observation rather
// than rendering it as an ordinary event, the same way the TUI now does.
func TestHTMLMarksTruncatedEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trunc.jsonl")
	t0 := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 1, TS: t0,
		Direction: proxy.ServerToClient, Truncated: true,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","result":{}}`),
	})

	st, id, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Build(st, id)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, out, Options{Format: FormatHTML}); err != nil {
		t.Fatal(err)
	}
	html := buf.String()

	if !strings.Contains(html, `"truncated":true`) {
		t.Fatal("embedded data is missing the truncated flag")
	}
	// statusOf and toneOf mark it warn from the flag rather than passing it through.
	if !strings.Contains(html, `if (ev.truncated) return "warn"`) {
		t.Fatal("the renderer does not mark a truncated event")
	}
}

// TestHTMLSurfacesSupersededStatus checks that a request whose id was reused
// carries the superseded status in the exported HTML (data, renderer, and CSS),
// while a normal answered request keeps an empty status cell.
func TestHTMLSurfacesSupersededStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reuse.jsonl")
	t0 := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	// id 1 is reused while in flight, so its first request is superseded.
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 1, TS: t0,
		Direction: proxy.ClientToServer, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`),
	})
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 2, TS: t0.Add(5 * time.Millisecond),
		Direction: proxy.ClientToServer, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`),
	})
	// id 2 is a normal answered request.
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 3, TS: t0.Add(10 * time.Millisecond),
		Direction: proxy.ClientToServer, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo"}}`),
	})
	writeEnv(t, path, proxy.Envelope{
		SessionID: "s1", ServerLabel: "demo", Seq: 4, TS: t0.Add(15 * time.Millisecond),
		Direction: proxy.ServerToClient, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":2,"result":{"content":[]}}`),
	})

	st, id, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Build(st, id)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, out, Options{Format: FormatHTML}); err != nil {
		t.Fatal(err)
	}
	html := buf.String()

	// The reused request is exported as a superseded call, and the answered one as
	// ok, so the browser has the data it needs to render each.
	if !strings.Contains(html, `"status":"superseded"`) {
		t.Fatal("embedded data is missing the superseded call status")
	}
	if !strings.Contains(html, `"status":"ok"`) {
		t.Fatal("embedded data should also carry the answered request as ok")
	}
	// statusOf surfaces superseded on a request row while ok still yields an empty
	// cell (the ternary returns "" for anything else).
	if !strings.Contains(html, `["pending", "superseded", "call_cancelled", "late_result"].includes(call.status) ? call.status : ""`) {
		t.Fatal("statusOf does not surface the superseded status on a request row")
	}
	// The CSS rule that colors it (as warn) must exist.
	if !strings.Contains(html, ".status.superseded { color:var(--warn); }") {
		t.Fatal("HTML is missing the .status.superseded CSS rule")
	}
}

// TestHTMLSurfacesCallCancellationStatuses. A cancelled call and a late result
// each need their own status token and their own CSS rule, or both render as an
// ordinary row in the one export people forward to somebody else.
func TestHTMLSurfacesCallCancellationStatuses(t *testing.T) {
	cancelledIndex, lateIndex := 0, 1
	data := SessionExport{
		Session: SessionSummary{ID: "s1", LateResults: 1},
		Calls: []CallExport{
			{Index: cancelledIndex, ID: "1", Method: "tools/call", Status: "call_cancelled"},
			{Index: lateIndex, ID: "2", Method: "tools/call", Status: "late_result", LateResult: true},
		},
		Events: []EventExport{
			{Seq: 1, Kind: "request", CallIndex: &cancelledIndex},
			{Seq: 2, Kind: "response", CallIndex: &lateIndex, Observation: "result arrived after cancellation"},
		},
	}
	var buf bytes.Buffer
	if err := Write(&buf, data, Options{Format: FormatHTML}); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	for _, want := range []string{
		`"status":"call_cancelled"`,
		`"status":"late_result"`,
		`"observation":"result arrived after cancellation"`,
		`["Late results", data.session.late_results]`,
		`.status.call_cancelled, .status.late_result`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("HTML missing %q", want)
		}
	}
}

// TestWriteHTMLStillEscapesMarkup. The HTML export is the one writer that must
// keep escaping. Its payload lands in template.JS inside a script block, where
// template.JS disables the contextual escaping html/template would apply, and a
// tool result containing </script> would otherwise close the element and run as
// markup. Wire fidelity loses to stored XSS in a file opened in a browser.
func TestWriteHTMLStillEscapesMarkup(t *testing.T) {
	data := SessionExport{
		Session: SessionSummary{ID: "s1"},
		Events: []EventExport{{
			Seq: 1, Kind: "response",
			Raw: json.RawMessage(`{"result":{"text":"</script><img src=x onerror=alert(1)>"}}`),
		}},
	}
	var buf bytes.Buffer
	if err := writeHTML(&buf, data); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "</script><img") {
		t.Fatalf("a payload must not be able to close the script element:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), `\u003c/script\u003e`) {
		t.Fatal("expected the markup to stay escaped in the HTML export")
	}
}

// TestHTMLNamesTheEndpoint covers the human-facing half of a self-describing
// capture. The heading falls back to the label, which on HTTP defaults to the
// target host alone, so the endpoint is what tells two paths of one host apart.
func TestHTMLNamesTheEndpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "http.jsonl")
	t0 := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	meta, err := json.Marshal(proxy.SessionMeta{Target: "https://api.example.com/tenant-a/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	writeEnv(t, path, proxy.Envelope{SessionID: "s1", ServerLabel: "api.example.com", Seq: 1, TS: t0, Direction: proxy.DirectionMeta, Transport: proxy.TransportHTTP, Raw: meta})
	writeEnv(t, path, proxy.Envelope{SessionID: "s1", ServerLabel: "api.example.com", Seq: 2, TS: t0.Add(time.Millisecond), Direction: proxy.ClientToServer, Transport: proxy.TransportHTTP, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)})
	st, id, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Build(st, id)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, out, Options{Format: FormatHTML}); err != nil {
		t.Fatal(err)
	}
	html := buf.String()

	if !strings.Contains(html, `"endpoint":"https://api.example.com/tenant-a/mcp"`) {
		t.Fatal("the embedded data does not carry the endpoint")
	}
	if !strings.Contains(html, "data.session.endpoint") {
		t.Fatal("the page never renders the endpoint it was given")
	}
}
