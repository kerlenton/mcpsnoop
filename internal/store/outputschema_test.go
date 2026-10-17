package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

const weatherOutputSchema = `{"type":"object","properties":{"temperature":{"type":"number"},"conditions":{"type":"string"}},"required":["temperature","conditions"]}`

// outputCall runs one stdio session on revision: the handshake, a listing of a
// get_weather tool with the given outputSchema, and one call to it answered
// with result. It returns the frame that answered the call.
func outputCall(t *testing.T, revision, outputSchema, result string, edit func(listing, answer *proxy.Envelope)) EventView {
	t.Helper()
	s := New()
	now := time.Now()
	frames := []proxy.Envelope{
		{Direction: proxy.ClientToServer, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + revision + `","clientInfo":{"name":"cli"},"capabilities":{}}}`)},
		{Direction: proxy.ServerToClient, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"` + revision + `","capabilities":{"tools":{}},"serverInfo":{"name":"srv"}}}`)},
		{Direction: proxy.ClientToServer, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)},
		{Direction: proxy.ServerToClient, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"get_weather","inputSchema":{"type":"object"},"outputSchema":` + outputSchema + `}]}}`)},
		{Direction: proxy.ClientToServer, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_weather","arguments":{}}}`)},
		{Direction: proxy.ServerToClient, Raw: json.RawMessage(`{"jsonrpc":"2.0","id":3,"result":` + result + `}`)},
	}
	if edit != nil {
		edit(&frames[3], &frames[5])
	}
	var last EventView
	for i, f := range frames {
		f.SessionID, f.ServerLabel, f.Seq, f.TS, f.Transport = "s1", "srv", uint64(i+1), now, proxy.TransportStdio
		last = s.Ingest(f)
	}
	return last
}

// TestAToolResultIsJudgedByItsOwnOutputSchema. A server that declares an
// outputSchema MUST return structuredContent that conforms to it, and the
// TypeScript and Python SDK clients reject a result that does not, or that
// leaves structuredContent out, so the agent sees the call fail. The warning
// names where the result breaks the schema and how.
func TestAToolResultIsJudgedByItsOwnOutputSchema(t *testing.T) {
	cases := []struct {
		name   string
		result string
		want   string // empty for no warning
	}{
		{"a conforming result", `{"content":[],"structuredContent":{"temperature":21.5,"conditions":"sunny"}}`, ""},
		{"a field of the wrong type", `{"content":[],"structuredContent":{"temperature":"warm","conditions":"sunny"}}`,
			`tool "get_weather" returned structuredContent that breaks its outputSchema at /temperature, got string, want number`},
		{"a required field left out", `{"content":[],"structuredContent":{"temperature":21}}`,
			`tool "get_weather" returned structuredContent that breaks its outputSchema at the root, missing property 'conditions'`},
		{"no structuredContent at all", `{"content":[{"type":"text","text":"21C"}]}`,
			`tool "get_weather" declares an outputSchema and returned no structuredContent, which SDK clients reject`},
		{"an error result", `{"content":[{"type":"text","text":"no such city"}],"isError":true,"structuredContent":{"oops":1}}`, ""},
		{"an error result with nothing structured", `{"content":[{"type":"text","text":"no such city"}],"isError":true}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := outputCall(t, "2025-11-25", weatherOutputSchema, tc.result, nil)
			if !strings.Contains(ev.Warning, tc.want) || (tc.want == "") != (ev.Warning == "") {
				t.Fatalf("warning = %q, want %q", ev.Warning, tc.want)
			}
			if ev.OutputMismatch != (tc.want != "") {
				t.Fatalf("OutputMismatch = %v for warning %q", ev.OutputMismatch, ev.Warning)
			}
		})
	}
}

// TestAnOutputSchemaIsNotJudgedWhereTheVerdictWouldBeAGuess. Every case here is
// a result that breaks the schema as written, and none is reported, because the
// fault could be mcpsnoop's own, or the schema cannot be read with certainty, or
// the result is not the call's answer yet.
func TestAnOutputSchemaIsNotJudgedWhereTheVerdictWouldBeAGuess(t *testing.T) {
	wrong := `{"content":[],"structuredContent":{"temperature":"warm","conditions":"sunny"}}`
	outside := filepath.Join(t.TempDir(), "weather.json")
	if err := os.WriteFile(outside, []byte(`{"type":"object","required":["never"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, revision, schema, result string
		edit                           func(listing, answer *proxy.Envelope)
	}{
		{name: "a revision before outputSchema existed", revision: "2025-03-26", schema: weatherOutputSchema, result: wrong},
		{name: "an answer mcpsnoop redacted", revision: "2025-11-25", schema: weatherOutputSchema, result: wrong,
			edit: func(_, answer *proxy.Envelope) { answer.Redacted = true }},
		{name: "a listing mcpsnoop redacted", revision: "2025-11-25", schema: weatherOutputSchema, result: wrong,
			edit: func(listing, _ *proxy.Envelope) { listing.Redacted = true }},
		{name: "a schema that refers to a file", revision: "2025-11-25",
			schema: `{"$ref":"file://` + filepath.ToSlash(outside) + `"}`, result: wrong},
		{name: "a schema that refers to the network", revision: "2025-11-25",
			schema: `{"$ref":"https://example.com/weather.json"}`, result: wrong},
		{name: "a dialect mcpsnoop does not know", revision: "2025-11-25",
			schema: `{"$schema":"https://example.com/my-dialect","type":"object","required":["never"]}`, result: wrong},
		{name: "a schema that does not compile", revision: "2025-11-25",
			schema: `{"type":"object","properties":{"temperature":{"type":"temperature"}}}`, result: wrong},
		{name: "a multi round-trip input request", revision: "2025-11-25", schema: weatherOutputSchema,
			result: `{"resultType":"input_required","inputRequests":{"city":{"method":"elicitation/create","params":{"message":"which city?"}}}}`},
		{name: "a task handle", revision: "2025-11-25", schema: weatherOutputSchema,
			result: `{"task":{"taskId":"t-1","status":"working"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := outputCall(t, tc.revision, tc.schema, tc.result, tc.edit)
			if strings.Contains(ev.Warning, "outputSchema") || ev.OutputMismatch {
				t.Fatalf("judged what it cannot judge: %q", ev.Warning)
			}
		})
	}
}

// TestAnOutputSchemaIsReadInTheDialectItsRevisionSets. A schema with no $schema
// is 2020-12 from 2025-11-25 on. Before that the dialect was not specified, and
// the two readings differ on keywords beside $ref, which draft-07 ignores and
// 2020-12 applies. So on the older revision a result is reported only when both
// readings reject it, and a schema that names its dialect is read in it.
func TestAnOutputSchemaIsReadInTheDialectItsRevisionSets(t *testing.T) {
	siblings := `{"type":"object","properties":{"code":{"$ref":"#/$defs/code","maxLength":2}},"$defs":{"code":{"type":"string"}}}`
	draft07 := `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"code":{"$ref":"#/definitions/code","maxLength":2}},"definitions":{"code":{"type":"string"}}}`
	long := `{"content":[],"structuredContent":{"code":"abcd"}}`
	notAString := `{"content":[],"structuredContent":{"code":7}}`
	cases := []struct {
		name, revision, schema, result string
		reported                       bool
	}{
		{"2020-12 applies the sibling", "2025-11-25", siblings, long, true},
		{"an unsettled dialect is not guessed", "2025-06-18", siblings, long, false},
		{"what both readings reject is reported", "2025-06-18", siblings, notAString, true},
		{"a declared draft-07 schema ignores the sibling", "2025-11-25", draft07, long, false},
		{"a declared draft-07 schema still holds its type", "2025-11-25", draft07, notAString, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := outputCall(t, tc.revision, tc.schema, tc.result, nil)
			if ev.OutputMismatch != tc.reported {
				t.Fatalf("reported = %v, want %v, warning %q", ev.OutputMismatch, tc.reported, ev.Warning)
			}
		})
	}
}

// TestFormatIsNeverAsserted. 2020-12 makes format an annotation, the Python SDK
// client asserts it in no dialect, and the library behind this check asserts it
// under draft-07 whatever it is told. A value that breaks only its format is
// therefore not reported in either dialect, and a schema using format regex,
// which the library checks with Go's own regular expressions, goes unjudged.
func TestFormatIsNeverAsserted(t *testing.T) {
	notAnEmail := `{"content":[],"structuredContent":{"mail":"not an address"}}`
	for _, schema := range []string{
		`{"type":"object","properties":{"mail":{"type":"string","format":"email"}}}`,
		`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"mail":{"type":"string","format":"email"}}}`,
		`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"mail":{"type":"string","format":"regex"}}}`,
		`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"format":{"type":"object"},"mail":{"type":"string","format":"email"}}}`,
	} {
		if ev := outputCall(t, "2025-11-25", schema, notAnEmail, nil); ev.OutputMismatch {
			t.Fatalf("schema %s asserted a format: %q", schema, ev.Warning)
		}
	}
	// A property named format is still a property, and still judged.
	ev := outputCall(t, "2025-11-25",
		`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"format":{"type":"object"}}}`,
		`{"content":[],"structuredContent":{"format":"a string"}}`, nil)
	if !strings.Contains(ev.Warning, "at /format, got string, want object") {
		t.Fatalf("warning = %q", ev.Warning)
	}
}

// TestABranchFailureNamesTheKeyword. A value that matches no branch of anyOf is
// reported at the keyword, since each branch's own complaint is about a shape
// the value was never meant to have.
func TestABranchFailureNamesTheKeyword(t *testing.T) {
	schema := `{"type":"object","properties":{"v":{"anyOf":[{"type":"string"},{"type":"number"}]}}}`
	ev := outputCall(t, "2025-11-25", schema, `{"content":[],"structuredContent":{"v":true}}`, nil)
	if !strings.Contains(ev.Warning, "at /v, 'anyOf' failed") {
		t.Fatalf("warning = %q", ev.Warning)
	}
}

// The outputSchema the TypeScript SDK's zod 4.6.7 generates for a weather tool,
// and the one the Python SDK 2.3.0 infers from a pydantic model, both taken from
// captures. One declares draft-07 and carries the regular expression zod uses
// for a datetime, the other reaches its fields through $defs, $ref and anyOf.
const (
	zodOutputSchema      = `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"temperature":{"type":"number"},"conditions":{"type":"string","enum":["sunny","rainy"]},"at":{"type":"string","format":"date-time","pattern":"^(?:(?:\\d\\d[2468][048]|\\d\\d[13579][26]|\\d\\d0[48]|[02468][048]00|[13579][26]00)-02-29|\\d{4}-(?:(?:0[13578]|1[02])-(?:0[1-9]|[12]\\d|3[01])|(?:0[469]|11)-(?:0[1-9]|[12]\\d|30)|(?:02)-(?:0[1-9]|1\\d|2[0-8])))T(?:(?:[01]\\d|2[0-3]):[0-5]\\d:[0-5]\\d(?:\\.\\d+)?(?:Z))$"},"tags":{"type":"array","items":{"type":"string"}},"nested":{"type":"object","properties":{"a":{"type":"integer","minimum":0,"maximum":9007199254740991}},"required":["a"],"additionalProperties":false},"maybe":{"type":["string","null"]}},"required":["temperature","conditions","at","nested","maybe"],"additionalProperties":false}`
	pydanticOutputSchema = `{"properties":{"name":{"title":"Name","type":"string"},"color":{"$ref":"#/$defs/Color"},"points":{"items":{"$ref":"#/$defs/Point"},"title":"Points","type":"array"},"label":{"anyOf":[{"type":"string"},{"type":"null"}],"default":null,"title":"Label"},"created":{"format":"date-time","title":"Created","type":"string"},"kind":{"default":"polygon","enum":["polygon","line"],"title":"Kind","type":"string"},"weight":{"maximum":100,"minimum":0,"title":"Weight","type":"integer"}},"required":["name","color","points","created","weight"],"type":"object","$defs":{"Color":{"enum":["red","blue"],"title":"Color","type":"string"},"Point":{"properties":{"x":{"title":"X","type":"number"},"y":{"title":"Y","type":"number"}},"required":["x","y"],"title":"Point","type":"object"}},"title":"Shape"}`
)

// TestOutputSchemasTheSDKsGenerateAreJudged. A schema mcpsnoop cannot compile
// goes unjudged, which is silence that looks like a pass. So each schema an SDK
// generates is shown to accept the result its own server returned and to catch
// one that breaks it.
func TestOutputSchemasTheSDKsGenerateAreJudged(t *testing.T) {
	cases := []struct {
		name, schema, result, want string
	}{
		{"zod, the result its server sent", zodOutputSchema,
			`{"content":[],"structuredContent":{"temperature":21.5,"conditions":"sunny","at":"2026-10-17T00:00:00Z","nested":{"a":3},"maybe":null}}`, ""},
		{"zod, a datetime its pattern refuses", zodOutputSchema,
			`{"content":[],"structuredContent":{"temperature":21.5,"conditions":"sunny","at":"yesterday","nested":{"a":3},"maybe":null}}`, "at /at, 'yesterday' does not match pattern"},
		{"zod, a field it does not allow", zodOutputSchema,
			`{"content":[],"structuredContent":{"temperature":21.5,"conditions":"sunny","at":"2026-10-17T00:00:00Z","nested":{"a":3,"b":1},"maybe":null}}`, "at /nested,"},
		{"pydantic, the result its server sent", pydanticOutputSchema,
			`{"content":[],"structuredContent":{"name":"tri","color":"red","points":[{"x":0.0,"y":0.0},{"x":1.5,"y":2.0}],"label":null,"created":"2026-10-17T00:00:00Z","kind":"polygon","weight":7}}`, ""},
		{"pydantic, a value its enum refuses", pydanticOutputSchema,
			`{"content":[],"structuredContent":{"name":"tri","color":"green","points":[],"created":"2026-10-17T00:00:00Z","weight":7}}`, "at /color,"},
		{"pydantic, a point of the wrong type", pydanticOutputSchema,
			`{"content":[],"structuredContent":{"name":"tri","color":"red","points":[{"x":"a","y":0}],"created":"2026-10-17T00:00:00Z","weight":7}}`, "at /points/0/x,"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := outputCall(t, "2025-11-25", tc.schema, tc.result, nil)
			if tc.want == "" && ev.Warning != "" || tc.want != "" && !strings.Contains(ev.Warning, tc.want) {
				t.Fatalf("warning = %q, want %q", ev.Warning, tc.want)
			}
		})
	}
}
