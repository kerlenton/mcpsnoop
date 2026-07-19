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

// TestHTMLSurfacesSupersededStatus checks that a request whose id was reused
// carries the superseded status in the exported HTML (data, renderer, and CSS),
// while a normal answered request keeps an empty status cell.
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
	if !strings.Contains(html, `call.status === "pending" || call.status === "superseded" ? call.status : ""`) {
		t.Fatal("statusOf does not surface the superseded status on a request row")
	}
	// The CSS rule that colors it (as warn) must exist.
	if !strings.Contains(html, ".status.superseded { color:var(--warn); }") {
		t.Fatal("HTML is missing the .status.superseded CSS rule")
	}
}
