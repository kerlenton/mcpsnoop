package store

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

const paramHeaderTool = `{"name":"route","inputSchema":{"type":"object","properties":{` +
	`"region":{"type":"string","x-mcp-header":"Region"},` +
	`"greeting":{"type":"string","x-mcp-header":"Greeting"},` +
	`"options":{"type":"object","properties":{` +
	`"verbose":{"type":"boolean","x-mcp-header":"Verbose"},` +
	`"count":{"type":"integer","x-mcp-header":"Count"}}}}}}`

func seedParamHeaderTool(s *Store) {
	listExchange(s, 1, "", toolsListResult("", paramHeaderTool))
}

func paramCall(seq uint64, transport string, batch bool, args string, headers ...proxy.MCPParamHeader) proxy.Envelope {
	return proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: seq, TS: time.Now(),
		Direction: proxy.ClientToServer, Transport: transport, Batch: batch,
		MCPMethod: "tools/call", MCPName: "route", MCPProtocolVersion: "2026-07-28",
		MCPParamHeaders: headers,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{` +
			`"name":"route","arguments":` + args + `}}`),
	}
}

func TestMCPParamHeadersMatchNestedPrimitiveArguments(t *testing.T) {
	s := New()
	seedParamHeaderTool(s)
	greeting := "Hello, 世界"
	encoded := "=?base64?" + base64.StdEncoding.EncodeToString([]byte(greeting)) + "?="

	ev := s.Ingest(paramCall(3, proxy.TransportHTTP, false,
		`{"region":"us-west1","greeting":"Hello, 世界","options":{"verbose":false,"count":42}}`,
		proxy.MCPParamHeader{Name: "mcp-param-Verbose", Value: "false"},
		proxy.MCPParamHeader{Name: "Mcp-Param-Region", Value: "us-west1"},
		proxy.MCPParamHeader{Name: "Mcp-Param-Greeting", Value: encoded},
		proxy.MCPParamHeader{Name: "Mcp-Param-Count", Value: "42.0"},
	))

	if ev.RoutingMismatch || strings.Contains(ev.Warning, "Mcp-Param") {
		t.Fatalf("matching parameter headers warned: mismatch=%v warning=%q", ev.RoutingMismatch, ev.Warning)
	}
	wantNames := []string{"Mcp-Param-Count", "Mcp-Param-Greeting", "Mcp-Param-Region", "mcp-param-Verbose"}
	if len(ev.MCPParamHeaders) != len(wantNames) {
		t.Fatalf("captured parameter headers = %+v", ev.MCPParamHeaders)
	}
	for i, want := range wantNames {
		if ev.MCPParamHeaders[i].Name != want {
			t.Fatalf("header order = %+v, want names %v", ev.MCPParamHeaders, wantNames)
		}
	}
}

func TestMCPParamHeaderMismatchSetsRoutingSignal(t *testing.T) {
	s := New()
	seedParamHeaderTool(s)
	ev := s.Ingest(paramCall(3, proxy.TransportHTTP, false, `{"region":"us-west1"}`,
		proxy.MCPParamHeader{Name: "Mcp-Param-Region", Value: "eu-west1"},
	))
	if !ev.RoutingMismatch || !strings.Contains(ev.Warning, "Mcp-Param-Region") || !strings.Contains(ev.Warning, "region") {
		t.Fatalf("parameter mismatch not surfaced: mismatch=%v warning=%q", ev.RoutingMismatch, ev.Warning)
	}
}

func TestMCPParamHeaderRequiredForModernAnnotatedArgument(t *testing.T) {
	s := New()
	seedParamHeaderTool(s)
	ev := s.Ingest(paramCall(3, proxy.TransportHTTP, false, `{"region":"us-west1"}`))
	if !ev.RoutingMismatch || !strings.Contains(ev.Warning, "Mcp-Param-Region") || !strings.Contains(ev.Warning, "missing") {
		t.Fatalf("missing parameter header not surfaced: mismatch=%v warning=%q", ev.RoutingMismatch, ev.Warning)
	}
}

func TestMCPParamHeadersRespectVersionNullAndEncodingBoundaries(t *testing.T) {
	t.Run("old protocol does not require header", func(t *testing.T) {
		s := New()
		seedParamHeaderTool(s)
		e := paramCall(3, proxy.TransportHTTP, false, `{"region":"us-west1"}`,
			proxy.MCPParamHeader{Name: "Mcp-Param-Region", Value: "wrong"},
		)
		e.MCPProtocolVersion = "2025-11-25"
		ev := s.Ingest(e)
		if ev.RoutingMismatch || strings.Contains(ev.Warning, "Mcp-Param") {
			t.Fatalf("old protocol missing header warned: mismatch=%v warning=%q", ev.RoutingMismatch, ev.Warning)
		}
	})

	t.Run("notification does not require header", func(t *testing.T) {
		s := New()
		seedParamHeaderTool(s)
		e := paramCall(3, proxy.TransportHTTP, false, `{"region":"us-west1"}`,
			proxy.MCPParamHeader{Name: "Mcp-Param-Region", Value: "wrong"},
		)
		e.Raw = json.RawMessage(`{"jsonrpc":"2.0","method":"tools/call","params":{` +
			`"name":"route","arguments":{"region":"us-west1"},` +
			`"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`)
		ev := s.Ingest(e)
		if ev.RoutingMismatch || strings.Contains(ev.Warning, "Mcp-Param") {
			t.Fatalf("notification missing parameter header warned: mismatch=%v warning=%q", ev.RoutingMismatch, ev.Warning)
		}
	})

	t.Run("undated request is not judged by modern neighbour", func(t *testing.T) {
		s := New()
		seedParamHeaderTool(s)
		s.Ingest(paramCall(3, proxy.TransportHTTP, false, `{"region":"us-west1"}`,
			proxy.MCPParamHeader{Name: "Mcp-Param-Region", Value: "us-west1"},
		))
		undated := paramCall(4, proxy.TransportHTTP, false, `{"region":"us-west1"}`,
			proxy.MCPParamHeader{Name: "Mcp-Param-Region", Value: "wrong"},
		)
		undated.MCPProtocolVersion = ""
		ev := s.Ingest(undated)
		if ev.RoutingMismatch || strings.Contains(ev.Warning, "Mcp-Param") {
			t.Fatalf("undated request inherited parameter-header requirement: mismatch=%v warning=%q", ev.RoutingMismatch, ev.Warning)
		}
	})

	t.Run("null omits header", func(t *testing.T) {
		s := New()
		seedParamHeaderTool(s)
		ev := s.Ingest(paramCall(3, proxy.TransportHTTP, false, `{"region":null}`))
		if ev.RoutingMismatch || strings.Contains(ev.Warning, "Mcp-Param") {
			t.Fatalf("null parameter without header warned: mismatch=%v warning=%q", ev.RoutingMismatch, ev.Warning)
		}
	})

	t.Run("header for null mismatches", func(t *testing.T) {
		s := New()
		seedParamHeaderTool(s)
		ev := s.Ingest(paramCall(3, proxy.TransportHTTP, false, `{"region":null}`,
			proxy.MCPParamHeader{Name: "Mcp-Param-Region", Value: "us-west1"},
		))
		if !ev.RoutingMismatch || !strings.Contains(ev.Warning, "absent or null") {
			t.Fatalf("header for null not surfaced: mismatch=%v warning=%q", ev.RoutingMismatch, ev.Warning)
		}
	})

	t.Run("malformed base64 mismatches", func(t *testing.T) {
		s := New()
		seedParamHeaderTool(s)
		ev := s.Ingest(paramCall(3, proxy.TransportHTTP, false, `{"region":"us-west1"}`,
			proxy.MCPParamHeader{Name: "Mcp-Param-Region", Value: "=?base64?%%%?="},
		))
		if !ev.RoutingMismatch || !strings.Contains(ev.Warning, "invalid Base64") {
			t.Fatalf("invalid Base64 not surfaced: mismatch=%v warning=%q", ev.RoutingMismatch, ev.Warning)
		}
	})

	t.Run("fraction near safe integer limit stays fractional", func(t *testing.T) {
		schema := `{"name":"route","inputSchema":{"type":"object","properties":` +
			`{"count":{"type":"integer","x-mcp-header":"Count"}}}}`
		s := New()
		listExchange(s, 1, "", toolsListResult("", schema))
		ev := s.Ingest(paramCall(3, proxy.TransportHTTP, false, `{"count":9007199254740991}`,
			proxy.MCPParamHeader{Name: "Mcp-Param-Count", Value: "9007199254740991.1"},
		))
		if !ev.RoutingMismatch {
			t.Fatal("a fractional header rounded by float64 must not equal the safe-integer body")
		}
	})
}

func TestMCPParamHeadersAvoidUnsupportedContextFalsePositives(t *testing.T) {
	t.Run("unknown header", func(t *testing.T) {
		s := New()
		seedParamHeaderTool(s)
		ev := s.Ingest(paramCall(3, proxy.TransportHTTP, false, `{}`,
			proxy.MCPParamHeader{Name: "Mcp-Param-Unknown", Value: "x"},
		))
		if ev.RoutingMismatch || strings.Contains(ev.Warning, "Mcp-Param") {
			t.Fatalf("unknown parameter header warned: mismatch=%v warning=%q", ev.RoutingMismatch, ev.Warning)
		}
	})

	t.Run("no tool definition", func(t *testing.T) {
		s := New()
		ev := s.Ingest(paramCall(1, proxy.TransportHTTP, false, `{"region":"us-west1"}`,
			proxy.MCPParamHeader{Name: "Mcp-Param-Region", Value: "wrong"},
		))
		if ev.RoutingMismatch || strings.Contains(ev.Warning, "Mcp-Param") {
			t.Fatalf("header without a definition warned: mismatch=%v warning=%q", ev.RoutingMismatch, ev.Warning)
		}
	})

	t.Run("stdio", func(t *testing.T) {
		s := New()
		seedParamHeaderTool(s)
		ev := s.Ingest(paramCall(3, proxy.TransportStdio, false, `{"region":"us-west1"}`,
			proxy.MCPParamHeader{Name: "Mcp-Param-Region", Value: "wrong"},
		))
		if ev.RoutingMismatch || strings.Contains(ev.Warning, "Mcp-Param") {
			t.Fatalf("stdio parameter metadata warned: mismatch=%v warning=%q", ev.RoutingMismatch, ev.Warning)
		}
	})

	t.Run("batch", func(t *testing.T) {
		s := New()
		seedParamHeaderTool(s)
		e := paramCall(3, proxy.TransportHTTP, true, `{"region":"us-west1"}`,
			proxy.MCPParamHeader{Name: "Mcp-Param-Region", Value: "wrong"},
		)
		e.MCPMethod, e.MCPName = "", ""
		ev := s.Ingest(e)
		if ev.RoutingMismatch || strings.Contains(ev.Warning, "Mcp-Param") {
			t.Fatalf("batch parameter metadata warned: mismatch=%v warning=%q", ev.RoutingMismatch, ev.Warning)
		}
	})
}

func TestMCPParamHeaderIgnoresNonStaticSchemaPath(t *testing.T) {
	tests := map[string]struct {
		schema string
		args   string
	}{
		"items": {
			schema: `{"type":"object","properties":{"items":{"type":"array","items":` +
				`{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"}}}}}}`,
			args: `{"items":[{"region":"us-west1"}]}`,
		},
		"oneOf": {
			schema: `{"oneOf":[{"type":"object","properties":` +
				`{"region":{"type":"string","x-mcp-header":"Region"}}}]}`,
			args: `{"region":"us-west1"}`,
		},
		"ref": {
			schema: `{"$ref":"#/$defs/Input","$defs":{"Input":{"type":"object","properties":` +
				`{"region":{"type":"string","x-mcp-header":"Region"}}}}}`,
			args: `{"region":"us-west1"}`,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			tool := `{"name":"route","inputSchema":` + test.schema + `}`
			s := New()
			listExchange(s, 1, "", toolsListResult("", tool))
			ev := s.Ingest(paramCall(3, proxy.TransportHTTP, false, test.args,
				proxy.MCPParamHeader{Name: "Mcp-Param-Region", Value: "wrong"},
			))
			if ev.RoutingMismatch || strings.Contains(ev.Warning, "Mcp-Param") {
				t.Fatalf("non-static schema path warned: mismatch=%v warning=%q", ev.RoutingMismatch, ev.Warning)
			}
		})
	}
}

func TestMCPParamHeaderSnapshotsAreIsolated(t *testing.T) {
	s := New()
	seedParamHeaderTool(s)
	headers := []proxy.MCPParamHeader{{Name: "Mcp-Param-Region", Value: "us-west1"}}
	ev := s.Ingest(paramCall(3, proxy.TransportHTTP, false, `{"region":"us-west1"}`, headers...))
	headers[0].Value = "changed input"
	ev.MCPParamHeaders[0].Value = "changed view"

	timeline := s.Timeline("s1")
	got := timeline[len(timeline)-1].MCPParamHeaders[0].Value
	if got != "us-west1" {
		t.Fatalf("stored parameter header changed through an external slice: %q", got)
	}
}

// paramSession is a session carrying one tool definition with the given schema,
// which is what mcpParamHeaderWarnings reads its bindings from.
func paramSession(t *testing.T, tool, schema string) *session {
	t.Helper()
	bindings, ok := mcpParamHeaderBindings(json.RawMessage(schema))
	if !ok {
		t.Fatalf("schema %s produced no valid bindings", schema)
	}
	return &session{toolDefinitions: map[string]ToolDefinition{
		tool: {Name: tool, paramHeaders: bindings},
	}}
}

func paramCallMsg(tool, args string) proxy.RPCMessage {
	return proxy.RPCMessage{
		Method: "tools/call",
		Params: json.RawMessage(`{"name":"` + tool + `","arguments":` + args + `}`),
	}
}

// TestParamHeaderRedactionMarkerMatchesTheProxy keeps the two literals in step.
// The store cannot import the proxy's redaction internals, so the marker is
// duplicated, and a duplicated literal that drifts silently turns the guard below
// back into the false positive it exists to prevent.
func TestParamHeaderRedactionMarkerMatchesTheProxy(t *testing.T) {
	// Asserted through a real redaction rather than against another constant, so
	// the test fails if the proxy ever changes what it writes.
	sink := &paramCaptureSink{}
	proxy.NewRedactingSink(sink, proxy.RedactConfig{Keys: []string{"secret"}}).Emit(proxy.Envelope{
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"params":{"secret":"hunter2"}}`),
	})
	if len(sink.envs) != 1 || !strings.Contains(string(sink.envs[0].Raw), redactedMarker) {
		t.Fatalf("the store's marker %q no longer matches what the proxy writes: %s", redactedMarker, sink.envs[0].Raw)
	}
}

type paramCaptureSink struct{ envs []proxy.Envelope }

func (s *paramCaptureSink) Emit(e proxy.Envelope) { s.envs = append(s.envs, e) }
func (s *paramCaptureSink) Close() error          { return nil }

// TestParamHeaderRedactionIsNotAMismatch is the blocker this guards. mcpsnoop's
// own redaction scrubs one side of a mirrored pair, and reporting that as a
// protocol disagreement invents a violation out of a privacy setting, on traffic
// that was correct, on a signal that fails a default check run.
func TestParamHeaderRedactionIsNotAMismatch(t *testing.T) {
	const schema = `{"type":"object","properties":{"authKey":{"type":"string","x-mcp-header":"Auth"}}}`
	for _, tc := range []struct {
		name         string
		header, body string
		wantWarnings int
	}{
		{"both scrubbed", redactedMarker, redactedMarker, 0},
		{"body scrubbed only", "sk-live-abcdef", redactedMarker, 0},
		{"header scrubbed only", redactedMarker, "sk-live-abcdef", 0},
		{"neither scrubbed and equal", "sk-live-abcdef", "sk-live-abcdef", 0},
		// A real disagreement on unredacted values must still be reported, or the
		// guard has switched the check off rather than scoping it.
		{"neither scrubbed and different", "sk-live-abcdef", "sk-live-999999", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := paramSession(t, "fetch", schema)
			got := mcpParamHeaderWarnings(sess,
				paramCallMsg("fetch", `{"authKey":`+quoteJSON(tc.body)+`}`),
				[]proxy.MCPParamHeader{{Name: "Mcp-Param-Auth", Value: tc.header}})
			if len(got) != tc.wantWarnings {
				t.Fatalf("warnings = %v, want %d", got, tc.wantWarnings)
			}
		})
	}
}

// TestParamHeaderBindingsSurviveAUnionType. A JSON Schema type may be a list,
// which is legal and common in generated schemas. Decoding it into a string
// failed the whole tree, and because the caller discarded the verdict, one such
// property anywhere in a schema silently switched off header checking for the
// entire tool, so a genuine violation went unreported.
func TestParamHeaderBindingsSurviveAUnionType(t *testing.T) {
	const schema = `{"type":"object","properties":{
		"region":{"type":"string","x-mcp-header":"Region"},
		"note":{"type":["string","null"]}}}`
	bindings, ok := mcpParamHeaderBindings(json.RawMessage(schema))
	if !ok {
		t.Fatal("a union type elsewhere in the schema must not invalidate the definition")
	}
	if len(bindings) != 1 || bindings[0].header != "Region" {
		t.Fatalf("bindings = %+v, want the one Region binding", bindings)
	}

	// The violation the old behaviour hid.
	got := mcpParamHeaderWarnings(paramSession(t, "route", schema),
		paramCallMsg("route", `{"region":"us-west1"}`),
		[]proxy.MCPParamHeader{{Name: "Mcp-Param-Region", Value: "eu-west1"}})
	if len(got) != 1 {
		t.Fatalf("a header disagreeing with the body must be reported, got %v", got)
	}

	// A union is still not a legal place for the annotation itself.
	if _, ok := mcpParamHeaderBindings(json.RawMessage(
		`{"properties":{"x":{"type":["string","null"],"x-mcp-header":"X"}}}`)); ok {
		t.Fatal("x-mcp-header on a union type is not permitted")
	}
}

// quoteJSON renders a Go string as a JSON string literal.
func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
