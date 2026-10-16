package exporter

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

const (
	headerTraceID  = "4bf92f3577b34da6a3ce929d0e0e4736"
	headerParentID = "00f067aa0ba902b7"
	headerParent   = "00-" + headerTraceID + "-" + headerParentID + "-01"
	metaTraceID    = "0af7651916cd43dd8448eb211c80319c"
	metaParentID   = "b7ad6b7169203331"
	metaParent     = "00-" + metaTraceID + "-" + metaParentID + "-01"
)

// TestWriteOTLPJoinsATraceCarriedInHTTPHeaders. OpenTelemetry's HTTP
// instrumentation puts a trace in the traceparent header and an MCP SDK writes
// nothing to _meta on its own, so a call from an instrumented client used to
// start a trace of its own. _meta still wins, being the carrier the
// specification names today, and a header that does not parse is no carrier.
func TestWriteOTLPJoinsATraceCarriedInHTTPHeaders(t *testing.T) {
	started := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	call := func(id, params string, tc *proxy.TraceContext) CallExport {
		return CallExport{ID: id, Method: "tools/call", StartedAt: started, Params: json.RawMessage(params), TraceContext: tc}
	}
	data := SessionExport{
		Session: SessionSummary{ID: "s1"},
		Calls: []CallExport{
			call("header-only", `{"name":"x"}`, &proxy.TraceContext{Traceparent: headerParent, Tracestate: "vendor=a"}),
			call("both", `{"name":"x","_meta":{"traceparent":"`+metaParent+`"}}`, &proxy.TraceContext{Traceparent: headerParent}),
			call("broken-header", `{"name":"x"}`, &proxy.TraceContext{Traceparent: "not-a-traceparent", Tracestate: "vendor=a"}),
		},
	}
	spans := writeOTLPTraceFields(t, data)
	if len(spans) != 3 {
		t.Fatalf("spans = %d, want 3", len(spans))
	}
	if spans[0].TraceID != headerTraceID || spans[0].ParentSpanID != headerParentID || spans[0].TraceState != "vendor=a" {
		t.Fatalf("header-only = %s/%s, want the header's %s/%s", spans[0].TraceID, spans[0].ParentSpanID, headerTraceID, headerParentID)
	}
	if spans[1].TraceID != metaTraceID || spans[1].ParentSpanID != metaParentID {
		t.Fatalf("both = %s/%s, want _meta's %s/%s", spans[1].TraceID, spans[1].ParentSpanID, metaTraceID, metaParentID)
	}
	if spans[2].TraceID != otlpID(16, "trace", "s1") || spans[2].ParentSpanID != "" || spans[2].TraceState != "" {
		t.Fatalf("a header that does not parse joined a trace: %s/%s", spans[2].TraceID, spans[2].ParentSpanID)
	}
}

// TestExportCarriesTheHeadersFromCaptureToSpan follows the headers the whole way,
// from the envelope the proxy wrote, through the store, to the span.
func TestExportCarriesTheHeadersFromCaptureToSpan(t *testing.T) {
	st := store.New()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: now, Direction: proxy.ClientToServer,
		Transport: proxy.TransportHTTP, TraceContext: &proxy.TraceContext{Traceparent: headerParent},
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x"}}`)})
	st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: now.Add(time.Millisecond), Direction: proxy.ServerToClient,
		Transport: proxy.TransportHTTP, Status: 200, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`)})
	data, err := Build(st, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Calls) != 1 || data.Calls[0].TraceContext == nil || data.Calls[0].TraceContext.Traceparent != headerParent {
		t.Fatalf("the export lost the request's trace context: %+v", data.Calls)
	}
	spans := writeOTLPTraceFields(t, data)
	if len(spans) != 1 || spans[0].TraceID != headerTraceID || spans[0].ParentSpanID != headerParentID {
		t.Fatalf("spans = %+v, want one under the header's trace", spans)
	}
}
