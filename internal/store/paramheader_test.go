package store

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
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

// listExchangeView is listExchange keeping the response frame, which is where a
// verdict about an advertised definition has to land.
func listExchangeView(s *Store, result string) EventView {
	now := time.Now()
	s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: now, Direction: proxy.ClientToServer,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`),
	})
	return s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: now, Direction: proxy.ServerToClient,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":` + result + `}`),
	})
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

// TestMCPParamHeaderRejectsNonStaticSchemaPath. The annotation is permitted only
// on a property reachable through a chain consisting solely of "properties" keys,
// and the spec says of anything else that it "makes the annotation, and thus the
// tool definition, invalid". Two halves to that. The definition is reported on
// the frame that advertised it, and no header of that tool is compared, which is
// what a conforming client excluding the tool would be doing anyway.
func TestMCPParamHeaderRejectsNonStaticSchemaPath(t *testing.T) {
	tests := map[string]struct {
		schema  string
		args    string
		keyword string
	}{
		"items": {
			schema: `{"type":"object","properties":{"items":{"type":"array","items":` +
				`{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"}}}}}}`,
			args:    `{"items":[{"region":"us-west1"}]}`,
			keyword: "items",
		},
		"oneOf": {
			schema: `{"oneOf":[{"type":"object","properties":` +
				`{"region":{"type":"string","x-mcp-header":"Region"}}}]}`,
			args:    `{"region":"us-west1"}`,
			keyword: "oneOf",
		},
		"ref": {
			schema: `{"$ref":"#/$defs/Input","$defs":{"Input":{"type":"object","properties":` +
				`{"region":{"type":"string","x-mcp-header":"Region"}}}}}`,
			args:    `{"region":"us-west1"}`,
			keyword: "$defs",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			tool := `{"name":"route","inputSchema":` + test.schema + `}`
			s := New()
			list := listExchangeView(s, toolsListResult("", tool))
			if !strings.Contains(list.Warning, test.keyword) ||
				!strings.Contains(list.Warning, `tool "route" declares`) {
				t.Fatalf("the advertising frame must name the keyword, got %q", list.Warning)
			}
			ev := s.Ingest(paramCall(3, proxy.TransportHTTP, false, test.args,
				proxy.MCPParamHeader{Name: "Mcp-Param-Region", Value: "wrong"},
			))
			if ev.RoutingMismatch || strings.Contains(ev.Warning, "Mcp-Param") {
				t.Fatalf("a rejected definition must have no headers compared: mismatch=%v warning=%q",
					ev.RoutingMismatch, ev.Warning)
			}
		})
	}
}

// TestMCPParamHeaderStaticReachabilityBoundaries pins what the scan must not
// report. A legal nested chain, an unused $defs carrying no annotation, and a
// tool whose argument is genuinely named x-mcp-header, which a scan keyed on the
// literal string alone would accuse of an annotation it does not have.
func TestMCPParamHeaderStaticReachabilityBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, schema string
		wantBindings int
	}{
		{
			name: "nested properties chain is reachable",
			schema: `{"type":"object","properties":{"options":{"type":"object","properties":` +
				`{"region":{"type":"string","x-mcp-header":"Region"}}}}}`,
			wantBindings: 1,
		},
		{
			name: "unused defs carrying no annotation",
			schema: `{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"}},` +
				`"$defs":{"Unused":{"type":"string"}}}`,
			wantBindings: 1,
		},
		{
			name:         "an argument named x-mcp-header",
			schema:       `{"type":"object","properties":{"x-mcp-header":{"type":"string"}}}`,
			wantBindings: 0,
		},
		{
			name: "an argument named x-mcp-header beside a real annotation",
			schema: `{"type":"object","properties":{"x-mcp-header":{"type":"string"},` +
				`"region":{"type":"string","x-mcp-header":"Region"}}}`,
			wantBindings: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bindings, violation := mcpParamHeaderBindings(json.RawMessage(tc.schema))
			if violation != "" {
				t.Fatalf("a legal schema was reported: %s", violation)
			}
			if len(bindings) != tc.wantBindings {
				t.Fatalf("bindings = %+v, want %d", bindings, tc.wantBindings)
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
	bindings, violation := mcpParamHeaderBindings(json.RawMessage(schema))
	if violation != "" {
		t.Fatalf("schema %s reported %s", schema, violation)
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
		name           string
		header, body   string
		headerScrubbed bool
		wantWarnings   int
	}{
		{"both scrubbed", redactedMarker, redactedMarker, true, 0},
		{"body scrubbed only", "sk-live-abcdef", redactedMarker, false, 0},
		{"header scrubbed only", redactedMarker, "sk-live-abcdef", true, 0},
		{"neither scrubbed and equal", "sk-live-abcdef", "sk-live-abcdef", false, 0},
		// A real disagreement on unredacted values must still be reported, or the
		// guard has switched the check off rather than scoping it.
		{"neither scrubbed and different", "sk-live-abcdef", "sk-live-999999", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := paramSession(t, "fetch", schema)
			got := mcpParamHeaderWarnings(sess,
				paramCallMsg("fetch", `{"authKey":`+quoteJSON(tc.body)+`}`),
				[]proxy.MCPParamHeader{{Name: "Mcp-Param-Auth", Value: tc.header, Redacted: tc.headerScrubbed}}, true)
			if len(got) != tc.wantWarnings {
				t.Fatalf("warnings = %v, want %d", got, tc.wantWarnings)
			}
		})
	}
}

// TestParamHeaderRedactionGuardIsNotForgeable. "[REDACTED]" is a legal header
// value and a legal string argument, so the header side is decided by the flag
// the shim set and never by the bytes. The frame-wide flag is not enough either,
// since any rule matching anything on the frame would otherwise unlock the skip
// for a header no rule touched.
func TestParamHeaderRedactionGuardIsNotForgeable(t *testing.T) {
	const schema = `{"type":"object","properties":{"authKey":{"type":"string","x-mcp-header":"Auth"}}}`
	for _, tc := range []struct {
		name           string
		frameRedacted  bool
		headerScrubbed bool
		wantWarnings   int
	}{
		{"mcpsnoop scrubbed this header", true, true, 0},
		{"a peer sent the placeholder itself", true, false, 1},
		{"no redaction anywhere", false, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mcpParamHeaderWarnings(paramSession(t, "fetch", schema),
				paramCallMsg("fetch", `{"authKey":"sk-live-abcdef"}`),
				[]proxy.MCPParamHeader{{Name: "Mcp-Param-Auth", Value: redactedMarker, Redacted: tc.headerScrubbed}},
				tc.frameRedacted)
			if len(got) != tc.wantWarnings {
				t.Fatalf("warnings = %v, want %d", got, tc.wantWarnings)
			}
		})
	}
}

// TestParamHeaderRedactionFlagSurvivesTheProxy keeps the two sides in step. The
// store now trusts a flag rather than the bytes, so the flag has to be the thing
// the shim actually sets on a header it scrubbed.
func TestParamHeaderRedactionFlagSurvivesTheProxy(t *testing.T) {
	// One case per way redactMCPParamHeaders can scrub a header, since each is its
	// own branch and any one of them left unflagged is a header the store then
	// judges by its bytes.
	for _, tc := range []struct {
		name string
		cfg  proxy.RedactConfig
		args string
	}{
		{
			name: "linked to a value a body rule removed",
			cfg:  proxy.RedactConfig{Keys: []string{"authKey"}},
			args: `{"authKey":"sk-live-abcdef"}`,
		},
		{
			name: "matched by the annotation name",
			cfg:  proxy.RedactConfig{Keys: []string{"auth"}},
			args: `{"region":"us-west1"}`,
		},
		{
			name: "matched by a value pattern",
			cfg:  proxy.RedactConfig{ValuePatterns: []string{`sk-live-\S+`}},
			args: `{"region":"us-west1"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &paramCaptureSink{}
			proxy.NewRedactingSink(sink, tc.cfg).Emit(proxy.Envelope{
				Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` +
					`{"name":"fetch","arguments":` + tc.args + `}}`),
				MCPParamHeaders: []proxy.MCPParamHeader{
					{Name: "Mcp-Param-Auth", Value: "sk-live-abcdef"},
					{Name: "Mcp-Param-Zone", Value: "eu-central1"},
				},
			})
			got := sink.envs[0].MCPParamHeaders
			if !got[0].Redacted || got[0].Value != redactedMarker {
				t.Fatalf("a scrubbed header must carry the flag, got %+v", got[0])
			}
			if got[1].Redacted || got[1].Value != "eu-central1" {
				t.Fatalf("a header no rule touched must be left alone, got %+v", got[1])
			}
		})
	}
}

// TestParamHeaderRepeatedFieldConflict. HTTP lets a client repeat a field, the
// spec never blesses it, and a conforming server must reject a request whose
// header disagrees with the body. Keeping only the first line made the verdict
// follow wire order, so the same conflicting request passed or failed depending
// on which line arrived first.
func TestParamHeaderRepeatedFieldConflict(t *testing.T) {
	const schema = `{"type":"object","properties":{` +
		`"region":{"type":"string","x-mcp-header":"Region"},` +
		`"count":{"type":"integer","x-mcp-header":"Count"}}}`
	greeting := "=?base64?" + base64.StdEncoding.EncodeToString([]byte("us-west1")) + "?="
	for _, tc := range []struct {
		name    string
		args    string
		headers []proxy.MCPParamHeader
		want    string
	}{
		{
			name: "conflicting, agreeing line first",
			args: `{"region":"us-west1"}`,
			headers: []proxy.MCPParamHeader{
				{Name: "Mcp-Param-Region", Value: "us-west1"},
				{Name: "Mcp-Param-Region", Value: "eu-west1"},
			},
			want: "repeated with conflicting values",
		},
		{
			name: "conflicting, disagreeing line first",
			args: `{"region":"us-west1"}`,
			headers: []proxy.MCPParamHeader{
				{Name: "Mcp-Param-Region", Value: "eu-west1"},
				{Name: "Mcp-Param-Region", Value: "us-west1"},
			},
			want: "repeated with conflicting values",
		},
		{
			name: "conflicting across spellings of the name",
			args: `{"region":"us-west1"}`,
			headers: []proxy.MCPParamHeader{
				{Name: "Mcp-Param-Region", Value: "us-west1"},
				{Name: "mcp-param-region", Value: "eu-west1"},
			},
			want: "repeated with conflicting values",
		},
		{
			name: "repeated with the same value is not a conflict",
			args: `{"region":"us-west1"}`,
			headers: []proxy.MCPParamHeader{
				{Name: "Mcp-Param-Region", Value: "us-west1"},
				{Name: "Mcp-Param-Region", Value: greeting},
			},
		},
		{
			name: "repeated integer agreeing numerically is not a conflict",
			args: `{"count":42}`,
			headers: []proxy.MCPParamHeader{
				{Name: "Mcp-Param-Count", Value: "42"},
				{Name: "Mcp-Param-Count", Value: "42.0"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mcpParamHeaderWarnings(paramSession(t, "route", schema),
				paramCallMsg("route", tc.args), tc.headers, false)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("warnings = %v, want none", got)
				}
				return
			}
			if len(got) != 1 || !strings.Contains(got[0], tc.want) {
				t.Fatalf("warnings = %v, want one containing %q", got, tc.want)
			}
		})
	}
}

// TestParamHeaderOutOfRangeIntegerNamesItsRule. 2^53 is a perfectly valid JSON
// integer. What it breaks is the separate rule that an x-mcp-header integer must
// fit the JavaScript safe range, and reporting it as "not a valid integer" sent
// the reader after a type error that is not there.
func TestParamHeaderOutOfRangeIntegerNamesItsRule(t *testing.T) {
	const schema = `{"type":"object","properties":{"count":{"type":"integer","x-mcp-header":"Count"}}}`
	for _, tc := range []struct {
		name         string
		args, header string
		want         string
	}{
		{"body out of range", `{"count":9007199254740992}`, "9007199254740992", outsideSafeRange},
		{"header out of range", `{"count":9007199254740991}`, "9007199254740992", outsideSafeRange},
		{"body at the limit", `{"count":9007199254740991}`, "9007199254740991", ""},
		{"body is not an integer", `{"count":42.5}`, "42", "is not a valid integer"},
		{"body is a string", `{"count":"42"}`, "42", "is not a valid integer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mcpParamHeaderWarnings(paramSession(t, "route", schema),
				paramCallMsg("route", tc.args),
				[]proxy.MCPParamHeader{{Name: "Mcp-Param-Count", Value: tc.header}}, false)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("warnings = %v, want none", got)
				}
				return
			}
			if len(got) != 1 || !strings.Contains(got[0], tc.want) {
				t.Fatalf("warnings = %v, want one containing %q", got, tc.want)
			}
		})
	}
}

// TestParamHeaderBindingsReportOnlyRealViolations is the blocker this guards.
// mcpParamHeaderBindings used to answer "invalid" for anything it could not
// decode, and the caller turned that into an accusation. A boolean subschema is
// legal JSON Schema, a tool may carry no inputSchema at all, and mcpsnoop's own
// --redact-secrets rewrites a subschema under a key like "token" into a string.
// None of those is a server declaring a forbidden annotation.
func TestParamHeaderBindingsReportOnlyRealViolations(t *testing.T) {
	for _, tc := range []struct {
		name, schema string
		wantReason   string
		wantBindings int
	}{
		{name: "no schema at all", schema: ``},
		{name: "schema is not an object", schema: `"gone"`},
		{name: "boolean subschema", schema: `{"type":"object","properties":{"anything":true}}`},
		{
			name:         "redacted subschema beside a live annotation",
			schema:       `{"type":"object","properties":{"token":"[REDACTED]","region":{"type":"string","x-mcp-header":"Region"}}}`,
			wantBindings: 1,
		},
		{
			name:         "annotation on a number",
			schema:       `{"type":"object","properties":{"ratio":{"type":"number","x-mcp-header":"Ratio"}}}`,
			wantReason:   `whose type "number" is not one of string, integer or boolean`,
			wantBindings: 0,
		},
		// The spec constrains the parameter's type, not the presence of a type
		// keyword, and a client that excluded these tools would be excluding tools
		// no rule forbids.
		{
			name:   "annotation on an enum with no type keyword",
			schema: `{"type":"object","properties":{"region":{"enum":["us","eu"],"x-mcp-header":"Region"}}}`,
		},
		{
			name:   "explicit null annotation on a property that declares none",
			schema: `{"type":"object","properties":{"ratio":{"type":"number","x-mcp-header":null}}}`,
		},
		{
			name:         "nullable union carries the annotation",
			schema:       `{"type":"object","properties":{"region":{"type":["string","null"],"x-mcp-header":"Region"}}}`,
			wantBindings: 1,
		},
		{
			name: "duplicate is caught even behind an unjudgeable type",
			schema: `{"type":"object","properties":{"a":{"enum":["x"],"x-mcp-header":"Region"},` +
				`"b":{"type":"string","x-mcp-header":"region"}}}`,
			wantReason: "on more than one property",
		},
		{
			name: "same annotation twice",
			schema: `{"type":"object","properties":{"a":{"type":"string","x-mcp-header":"Region"},` +
				`"b":{"type":"string","x-mcp-header":"region"}}}`,
			wantReason: "on more than one property",
		},
		{
			name:       "annotation is not a valid field name",
			schema:     `{"type":"object","properties":{"a":{"type":"string","x-mcp-header":"A B"}}}`,
			wantReason: "not a valid header field name",
		},
		{
			name:       "annotation is empty",
			schema:     `{"type":"object","properties":{"a":{"type":"string","x-mcp-header":""}}}`,
			wantReason: "an empty x-mcp-header",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bindings, reason := mcpParamHeaderBindings(json.RawMessage(tc.schema))
			if tc.wantReason == "" {
				if reason != "" {
					t.Fatalf("reason = %q, want none", reason)
				}
			} else if !strings.Contains(reason, tc.wantReason) {
				t.Fatalf("reason = %q, want one containing %q", reason, tc.wantReason)
			}
			if len(bindings) != tc.wantBindings {
				t.Fatalf("bindings = %+v, want %d", bindings, tc.wantBindings)
			}
		})
	}
}

// TestParamHeaderViolationLandsOnTheAdvertisingFrame drives the whole path,
// since the verdict is parked on the session inside completeCall and drained by
// Ingest, and asserts the two halves that matter: a real violation is named on
// the frame that advertised it, and a schema mcpsnoop merely could not read is
// not reported at all.
func TestParamHeaderViolationLandsOnTheAdvertisingFrame(t *testing.T) {
	t.Run("forbidden annotation is named", func(t *testing.T) {
		s := New()
		ev := listExchangeView(s, toolsListResult("", `{"name":"route","inputSchema":`+
			`{"type":"object","properties":{"ratio":{"type":"number","x-mcp-header":"Ratio"}}}}`))
		if !strings.Contains(ev.Warning, `tool "route" declares`) ||
			!strings.Contains(ev.Warning, "Ratio") ||
			!strings.Contains(ev.Warning, "not one of string, integer or boolean") {
			t.Fatalf("violation not named on the advertising frame: %q", ev.Warning)
		}
	})

	t.Run("unreadable schema stays silent", func(t *testing.T) {
		s := New()
		ev := listExchangeView(s, toolsListResult("", `{"name":"route","inputSchema":`+
			`{"type":"object","properties":{"token":"[REDACTED]"}}}`))
		if strings.Contains(ev.Warning, "x-mcp-header") {
			t.Fatalf("a schema mcpsnoop could not read was reported as a violation: %q", ev.Warning)
		}
	})

	t.Run("a legal schema stays silent", func(t *testing.T) {
		s := New()
		ev := listExchangeView(s, toolsListResult("", paramHeaderTool))
		if strings.Contains(ev.Warning, "x-mcp-header") {
			t.Fatalf("a legal schema was reported as a violation: %q", ev.Warning)
		}
	})
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
	bindings, violation := mcpParamHeaderBindings(json.RawMessage(schema))
	if violation != "" {
		t.Fatalf("a union type elsewhere in the schema must not invalidate the definition: %s", violation)
	}
	if len(bindings) != 1 || bindings[0].header != "Region" {
		t.Fatalf("bindings = %+v, want the one Region binding", bindings)
	}

	// The violation the old behaviour hid.
	got := mcpParamHeaderWarnings(paramSession(t, "route", schema),
		paramCallMsg("route", `{"region":"us-west1"}`),
		[]proxy.MCPParamHeader{{Name: "Mcp-Param-Region", Value: "eu-west1"}}, false)
	if len(got) != 1 {
		t.Fatalf("a header disagreeing with the body must be reported, got %v", got)
	}

	// A nullable union is a legal place for the annotation. The spec's own table
	// has a row for a parameter whose value is null, telling the client to omit
	// the header, which only makes sense if the parameter may be nullable, and
	// ["string","null"] is how a generated schema spells that.
	nullable, violation := mcpParamHeaderBindings(json.RawMessage(
		`{"properties":{"x":{"type":["string","null"],"x-mcp-header":"X"}}}`))
	if violation != "" {
		t.Fatalf("a nullable parameter may carry the annotation, got %s", violation)
	}
	if len(nullable) != 1 || nullable[0].typ != "string" {
		t.Fatalf("bindings = %+v, want one string binding", nullable)
	}

	// A union naming two real types is something mcpsnoop cannot judge, so it
	// binds nothing and accuses nobody.
	both, violation := mcpParamHeaderBindings(json.RawMessage(
		`{"properties":{"x":{"type":["string","number"],"x-mcp-header":"X"}}}`))
	if violation != "" || len(both) != 0 {
		t.Fatalf("bindings = %+v violation = %q, want neither", both, violation)
	}
}

// TestParamHeaderNullableParameterOmitsItsHeader closes the loop on the nullable
// case. The spec's table says a client MUST omit the header when the value is
// null and the server MUST NOT expect it, which the old union rejection made
// unreachable for the one schema spelling that legitimately permits null.
func TestParamHeaderNullableParameterOmitsItsHeader(t *testing.T) {
	const schema = `{"type":"object","properties":{"region":{"type":["string","null"],"x-mcp-header":"Region"}}}`
	sess := paramSession(t, "route", schema)
	if got := mcpParamHeaderWarnings(sess, paramCallMsg("route", `{"region":null}`), nil, false); len(got) != 0 {
		t.Fatalf("a null value with no header must be silent, got %v", got)
	}
	got := mcpParamHeaderWarnings(sess, paramCallMsg("route", `{"region":"us-west1"}`),
		[]proxy.MCPParamHeader{{Name: "Mcp-Param-Region", Value: "eu-west1"}}, false)
	if len(got) != 1 {
		t.Fatalf("a nullable parameter must still be compared when it has a value, got %v", got)
	}
}

// TestParamHeaderRedactedAncestorIsNotAMissingParameter. A scrubbed ancestor
// hides the parameter from the walk, and reporting that as the client omitting a
// value invents a violation out of the user's privacy setting on a signal that
// fails a default check run.
func TestParamHeaderRedactedAncestorIsNotAMissingParameter(t *testing.T) {
	const schema = `{"type":"object","properties":{"options":{"type":"object","properties":` +
		`{"count":{"type":"integer","x-mcp-header":"Count"}}}}}`
	headers := []proxy.MCPParamHeader{{Name: "Mcp-Param-Count", Value: "42"}}
	for _, tc := range []struct {
		name     string
		args     string
		redacted bool
		want     int
	}{
		{"scrubbed ancestor on a redacted frame", `{"options":"[REDACTED]"}`, true, 0},
		{"same bytes with no redaction", `{"options":"[REDACTED]"}`, false, 1},
		{"genuinely absent parameter still warns", `{"options":{}}`, true, 1},
		{"present and matching stays silent", `{"options":{"count":42}}`, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mcpParamHeaderWarnings(paramSession(t, "route", schema),
				paramCallMsg("route", tc.args), headers, tc.redacted)
			if len(got) != tc.want {
				t.Fatalf("warnings = %v, want %d", got, tc.want)
			}
		})
	}
}

// TestParseMCPIntegerGrammarAndMagnitude. The old character scan refused 0x2A
// while letting 0o52 and 0b101 through, and its comment claimed all three were
// NaN under JavaScript's Number(), which is false for two of them. The magnitude
// bound is the other half: 1e1000000 is nine bytes and a million digits once
// big.Rat expands it, under the store's lock.
func TestParseMCPIntegerGrammarAndMagnitude(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"42", true}, {"-7", true}, {"+42", true}, {"042", true},
		{"42.0", true}, {"4.2e1", true}, {"0.42e2", true}, {"-0", true},
		{"42.5", false}, {"", false}, {"-", false}, {"42abc", false},
		{"0x2A", false}, {"0o52", false}, {"0b101", false},
		{"1_000", false}, {"84/2", false}, {"0x1p+5", false},
		{"٤٢", false}, {"NaN", false}, {"Inf", false}, {"1e", false},
		{"1e1000000", false}, {"1e-1000000", false}, {"1e999999999999999999999", false},
	} {
		if got := parseMCPInteger(tc.value); got.safe != tc.want {
			t.Errorf("parseMCPInteger(%q).safe = %v, want %v", tc.value, got.safe, tc.want)
		}
	}

	// The point of the bound. Without it this expands to a million digits.
	done := make(chan struct{})
	go func() {
		defer close(done)
		parseMCPInteger("1e1000000")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("parseMCPInteger expanded a magnitude it should have refused outright")
	}
}

// quoteJSON renders a Go string as a JSON string literal.
func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestRoutingComparisonsRespectRedaction. The Mcp-Param comparison was gated on
// mcpsnoop's own placeholder, and the three routing fields beside it were not,
// so scrubbing a tool name or a protocol version made a correct exchange read as
// a routing mismatch. That is a warning on correct traffic, which fails a
// default check run.
func TestRoutingComparisonsRespectRedaction(t *testing.T) {
	const meta = `"_meta":{"io.modelcontextprotocol/protocolVersion":%s,` +
		`"io.modelcontextprotocol/clientCapabilities":{}}`
	for _, tc := range []struct {
		name    string
		mutate  func(*proxy.Envelope)
		warning string
	}{
		{
			name: "Mcp-Name",
			mutate: func(e *proxy.Envelope) {
				e.Raw = json.RawMessage(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{` +
					`"name":"[REDACTED]","arguments":{},` + fmt.Sprintf(meta, `"2026-07-28"`) + `}}`)
			},
			warning: "Mcp-Name",
		},
		{
			name:    "Mcp-Method",
			mutate:  func(e *proxy.Envelope) { e.MCPMethod = redactedMarker },
			warning: "Mcp-Method",
		},
		{
			name: "MCP-Protocol-Version",
			mutate: func(e *proxy.Envelope) {
				e.Raw = json.RawMessage(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{` +
					`"name":"route","arguments":{},` + fmt.Sprintf(meta, `"[REDACTED]"`) + `}}`)
			},
			warning: "MCP-Protocol-Version",
		},
	} {
		for _, redacted := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s redacted=%v", tc.name, redacted), func(t *testing.T) {
				s := New()
				e := proxy.Envelope{
					SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: time.Now(),
					Direction: proxy.ClientToServer, Transport: proxy.TransportHTTP,
					MCPMethod: "tools/call", MCPName: "route", MCPProtocolVersion: "2026-07-28",
					Redacted: redacted,
					Raw: json.RawMessage(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{` +
						`"name":"route","arguments":{},` + fmt.Sprintf(meta, `"2026-07-28"`) + `}}`),
				}
				tc.mutate(&e)
				ev := s.Ingest(e)
				got := strings.Contains(ev.Warning, tc.warning)
				if redacted && got {
					t.Fatalf("a scrubbed side was reported as a disagreement: %q", ev.Warning)
				}
				if !redacted && !got {
					t.Fatalf("the same bytes with no redaction must still be reported, got %q", ev.Warning)
				}
			})
		}
	}
}

// TestParamHeaderDuplicateSurvivesRedaction. A duplicate x-mcp-header is only
// visible while both annotations are, and scrubbing a subschema removed one of
// them outright, so mcpsnoop accepted an invalid definition and then validated
// the surviving half of the duplicate against the wrong property.
func TestParamHeaderDuplicateSurvivesRedaction(t *testing.T) {
	const tool = `{"name":"route","inputSchema":{"type":"object","properties":{` +
		`"token":{"type":"string","x-mcp-header":"Token"},` +
		`"auth":{"type":"string","x-mcp-header":"token"}}}}`
	sink := &paramCaptureSink{}
	proxy.NewRedactingSink(sink, proxy.RedactConfig{CommonSecrets: true}).Emit(proxy.Envelope{
		Direction: proxy.ServerToClient,
		Raw:       json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":` + toolsListResult("", tool) + `}`),
	})

	s := New()
	s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: time.Now(),
		Direction: proxy.ClientToServer,
		Raw:       json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`),
	})
	ev := s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: time.Now(),
		Direction: proxy.ServerToClient, Raw: sink.envs[0].Raw, Redacted: sink.envs[0].Redacted,
	})
	if !strings.Contains(ev.Warning, "on more than one property") {
		t.Fatalf("a duplicate must survive redaction to be reported, got %q", ev.Warning)
	}
}

// TestParamHeaderReachabilityReadsSchemaNotData. The scan walked arbitrary JSON,
// so a JSON object sitting in default, const, examples or enum was read as a
// subschema and a key spelled x-mcp-header in it as an annotation. A registry
// server whose tool takes a schema as an argument and documents it is an ordinary
// shape, and it was accused of a violation and had every real check switched off.
func TestParamHeaderReachabilityReadsSchemaNotData(t *testing.T) {
	t.Run("instance keywords hold data", func(t *testing.T) {
		for _, kw := range []string{"default", "const", "examples", "enum"} {
			inner := `{"type":"string","x-mcp-header":"Region"}`
			if kw == "examples" || kw == "enum" {
				inner = "[" + inner + "]"
			}
			schema := `{"type":"object","properties":{"s":{"type":"object","` + kw + `":` + inner + `}}}`
			if _, violation := mcpParamHeaderBindings(json.RawMessage(schema)); violation != "" {
				t.Errorf("%s holds data, not a subschema, got %q", kw, violation)
			}
		}
	})

	// The keys of these are names the server chose, not schema keywords.
	t.Run("a definition named x-mcp-header is a name", func(t *testing.T) {
		for _, kw := range []string{"$defs", "definitions", "dependentSchemas", "patternProperties"} {
			schema := `{"type":"object","` + kw + `":{"x-mcp-header":{"type":"string"}},` +
				`"properties":{"r":{"type":"string","x-mcp-header":"R"}}}`
			bindings, violation := mcpParamHeaderBindings(json.RawMessage(schema))
			if violation != "" || len(bindings) != 1 {
				t.Errorf("%s: bindings=%d violation=%q, want one binding and no violation",
					kw, len(bindings), violation)
			}
		}
	})

	// And the rule it exists for still fires, including through a $defs whose
	// entry really does carry an annotation.
	t.Run("a genuinely unreachable annotation still fires", func(t *testing.T) {
		for _, tc := range []struct{ keyword, schema string }{
			{"items", `{"type":"object","properties":{"xs":{"items":{"properties":{"r":{"type":"string","x-mcp-header":"R"}}}}}}`},
			{"oneOf", `{"oneOf":[{"properties":{"r":{"type":"string","x-mcp-header":"R"}}}]}`},
			{"$defs", `{"$defs":{"I":{"properties":{"r":{"type":"string","x-mcp-header":"R"}}}}}`},
		} {
			_, violation := mcpParamHeaderBindings(json.RawMessage(tc.schema))
			if !strings.Contains(violation, tc.keyword) {
				t.Errorf("%s: violation = %q, want it named", tc.keyword, violation)
			}
		}
	})
}
