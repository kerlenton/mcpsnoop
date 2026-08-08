package store

import (
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

func TestAnalyzeSchemaClean(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"query":{"type":"string"},
			"limit":{"type":"integer"}
		}
	}`)

	got := analyzeSchema(schema, false)

	if len(got) != 0 {
		t.Fatalf("got %v findings, want none", got)
	}
}

func TestAnalyzeSchemaOneOf(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"oneOf": [
			{"type":"string"},
			{"type":"integer"}
		]
	}`)

	got := analyzeSchema(schema, false)

	want := []SchemaFinding{
		{Kind: FindingOneOf},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAnalyzeSchemaAnyOf(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"anyOf": [
			{"type":"string"},
			{"type":"integer"}
		]
	}`)

	got := analyzeSchema(schema, false)

	want := []SchemaFinding{
		{Kind: FindingAnyOf},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAnalyzeSchemaAllOf(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"allOf": [
			{"type":"string"},
			{"type":"integer"}
		]
	}`)

	got := analyzeSchema(schema, false)

	want := []SchemaFinding{
		{Kind: FindingAllOf},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAnalyzeSchemaNot(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"not": {
			"type":"string"
		}
	}`)

	got := analyzeSchema(schema, false)

	want := []SchemaFinding{
		{Kind: FindingNot},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAnalyzeSchemaRef(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"$ref": "#/$defs/Foo"
	}`)

	got := analyzeSchema(schema, false)

	want := []SchemaFinding{
		{Kind: FindingRef},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAnalyzeSchemaPropertyNamedLikeKeyword(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"oneOf": {
				"type": "string"
			}
		}
	}`)

	got := analyzeSchema(schema, false)

	if len(got) != 0 {
		t.Fatalf("got %v, want none", got)
	}
}

func TestAnalyzeSchemaExternalRef(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"$ref": "https://example.com/schema.json"
	}`)

	got := analyzeSchema(schema, false)

	want := []SchemaFinding{
		{Kind: FindingExternalRef},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAnalyzeSchemaUntypedProperty(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {}
		}
	}`)

	got := analyzeSchema(schema, false)

	want := []SchemaFinding{
		{Kind: FindingUntypedProperty},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAnalyzeSchemaNestedUntypedProperty(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"filter":{
				"type":"object",
				"properties":{
					"value":{
						"description":"whatever"
					}
				}
			}
		}
	}`)

	got := analyzeSchema(schema, false)

	want := []SchemaFinding{
		{Kind: FindingUntypedProperty},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAnalyzeSchemaTopLevelWithoutTypeIsNotFlaggedAsUntypedProperty(t *testing.T) {
	schema := json.RawMessage(`{
		"oneOf":[
			{
				"type":"object",
				"properties":{
					"a":{"type":"string"}
				}
			},
			{
				"type":"object",
				"properties":{
					"b":{"type":"string"}
				}
			}
		]
	}`)

	got := analyzeSchema(schema, false)

	for _, finding := range got {
		if finding.Kind == FindingUntypedProperty {
			t.Fatalf("got %v, top-level schema must not be treated as a property", got)
		}
	}
}

// A real MCP inputSchema is an object whose parameters live under properties, so
// that is where a construct actually appears. Flagging such a property as
// untyped would be wrong (its type comes from the reference or the branches) and
// would also hide the construct behind the untyped label.
func TestAnalyzeSchemaConstructOnAPropertyIsNotAlsoUntyped(t *testing.T) {
	cases := []struct {
		name   string
		schema string
		want   []SchemaFinding
	}{
		{
			name:   "oneOf",
			schema: `{"type":"object","properties":{"config":{"oneOf":[{"type":"string"},{"type":"number"}]}}}`,
			want:   []SchemaFinding{{Kind: FindingOneOf}},
		},
		{
			name:   "internal ref",
			schema: `{"type":"object","properties":{"user":{"$ref":"#/$defs/User"}}}`,
			want:   []SchemaFinding{{Kind: FindingRef}},
		},
		{
			name:   "external ref",
			schema: `{"type":"object","properties":{"user":{"$ref":"https://example.com/user.json"}}}`,
			want:   []SchemaFinding{{Kind: FindingExternalRef}},
		},
		{
			name:   "anyOf",
			schema: `{"type":"object","properties":{"v":{"anyOf":[{"type":"string"}]}}}`,
			want:   []SchemaFinding{{Kind: FindingAnyOf}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := analyzeSchema(json.RawMessage(tc.schema), false)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// enum and const pin the accepted values more tightly than a type would, so a
// property using either is not ambiguous and must not be reported as untyped.
func TestAnalyzeSchemaEnumAndConstAreNotUntyped(t *testing.T) {
	for _, schema := range []string{
		`{"type":"object","properties":{"mode":{"enum":["fast","slow"]}}}`,
		`{"type":"object","properties":{"version":{"const":2}}}`,
	} {
		if got := analyzeSchema(json.RawMessage(schema), false); len(got) != 0 {
			t.Fatalf("analyzeSchema(%s) = %v, want no findings", schema, got)
		}
	}
}

// Findings carry only a kind, so repeats of one kind are indistinguishable and
// collapsing them keeps "more than one finding" meaning "more than one kind".
func TestAnalyzeSchemaDeduplicatesByKind(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"a":{"description":"no type"},
			"b":{"description":"also no type"},
			"c":{"description":"still no type"}
		}
	}`)

	got := analyzeSchema(schema, false)
	want := []SchemaFinding{{Kind: FindingUntypedProperty}}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// A construct is just as real inside these containers as at the top level, so
// the walk has to reach them or a schema reads as clean when it is not.
func TestAnalyzeSchemaReachesEveryContainer(t *testing.T) {
	cases := []struct {
		name   string
		schema string
		want   SchemaFindingKind
	}{
		{"additionalProperties", `{"type":"object","additionalProperties":{"oneOf":[{"type":"string"}]}}`, FindingOneOf},
		{"if", `{"type":"object","if":{"oneOf":[{"type":"string"}]}}`, FindingOneOf},
		{"then", `{"type":"object","then":{"$ref":"https://example.com/x.json"}}`, FindingExternalRef},
		{"else", `{"type":"object","else":{"anyOf":[{"type":"string"}]}}`, FindingAnyOf},
		{"contains", `{"type":"object","properties":{"xs":{"type":"array","contains":{"allOf":[{"type":"string"}]}}}}`, FindingAllOf},
		{"prefixItems", `{"type":"object","properties":{"xs":{"type":"array","prefixItems":[{"oneOf":[{"type":"string"}]}]}}}`, FindingOneOf},
		{"patternProperties", `{"type":"object","patternProperties":{"^a":{"$ref":"#/$defs/A"}}}`, FindingRef},
		{"$defs", `{"type":"object","$defs":{"X":{"oneOf":[{"type":"string"}]}}}`, FindingOneOf},
		{"definitions", `{"type":"object","definitions":{"X":{"anyOf":[{"type":"string"}]}}}`, FindingAnyOf},
		{"dependentSchemas", `{"type":"object","dependentSchemas":{"a":{"allOf":[{"type":"string"}]}}}`, FindingAllOf},
		{"propertyNames", `{"type":"object","propertyNames":{"$ref":"#/$defs/N"}}`, FindingRef},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := analyzeSchema(json.RawMessage(tc.schema), false)
			if len(got) != 1 || got[0].Kind != tc.want {
				t.Fatalf("got %v, want exactly [%s]", got, tc.want)
			}
		})
	}
}

// Nothing may be resolved or fetched: an external reference is recognized by its
// form alone, and the schema it points at is never read.
func TestAnalyzeSchemaClassifiesRefsByFormOnly(t *testing.T) {
	cases := map[string]SchemaFindingKind{
		"#":                          FindingRef,
		"#/$defs/User":               FindingRef,
		"#/definitions/User":         FindingRef,
		"https://example.com/u.json": FindingExternalRef,
		"./shared.json#/User":        FindingExternalRef,
		"urn:example:user":           FindingExternalRef,
		"file:///etc/schema.json":    FindingExternalRef,
	}
	for ref, want := range cases {
		schema := json.RawMessage(`{"type":"object","properties":{"u":{"$ref":` + strconv.Quote(ref) + `}}}`)
		got := analyzeSchema(schema, false)
		if len(got) != 1 || got[0].Kind != want {
			t.Fatalf("$ref %q: got %v, want [%s]", ref, got, want)
		}
	}
}

// A tool's inputSchema must be a JSON Schema object whose root type is "object".
// The constraint is in $defs.Tool of every schema revision mcpsnoop can observe,
// so none of these cases needs a revision gate.
func TestAnalyzeSchemaNonObjectRoot(t *testing.T) {
	cases := map[string]string{
		"dialect marker only":  `{"$schema":"http://json-schema.org/draft-07/schema#"}`,
		"empty object":         `{}`,
		"array root":           `{"type":"array","items":{"type":"string"}}`,
		"boolean schema":       `true`,
		"null":                 `null`,
		"string root":          `{"type":"string"}`,
		"type is not a string": `{"type":["object","null"]}`,
	}
	for name, schema := range cases {
		t.Run(name, func(t *testing.T) {
			got := analyzeSchema(json.RawMessage(schema), false)
			if !slices.Contains(got, SchemaFinding{Kind: FindingNonObjectRoot}) {
				t.Fatalf("got %v, want a %s finding", got, FindingNonObjectRoot)
			}
		})
	}
}

// An absent inputSchema fails the same rule: $defs.Tool.required is
// ["inputSchema", "name"], so there is nothing for a client to validate against.
func TestAnalyzeSchemaAbsentInputSchemaIsANonObjectRoot(t *testing.T) {
	got := analyzeSchema(nil, false)

	want := []SchemaFinding{{Kind: FindingNonObjectRoot}}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// The two forms the Tools page names for a tool that takes no parameters. Both
// are correct and must stay silent, or every parameterless tool reports.
func TestAnalyzeSchemaObjectRootIsClean(t *testing.T) {
	for _, schema := range []string{
		`{"type":"object"}`,
		`{"type":"object","additionalProperties":false}`,
		`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object"}`,
	} {
		got := analyzeSchema(json.RawMessage(schema), false)
		if slices.Contains(got, SchemaFinding{Kind: FindingNonObjectRoot}) {
			t.Fatalf("schema %s: got %v, want no %s finding", schema, got, FindingNonObjectRoot)
		}
	}
}

// The reason is what a warning will carry, so it has to name what was actually
// on the wire rather than just say the schema was wrong. Bisecting a listing is
// the cost of a warning that does not. Pinned now so the wording is settled
// before anything renders it.
func TestSchemaRootViolationNamesWhatWasSeen(t *testing.T) {
	cases := map[string]string{
		``:                                "no inputSchema",
		`{}`:                              "an inputSchema with no root type",
		`{"type":null}`:                   "an inputSchema with a null root type",
		`{"type":"array"}`:                `an inputSchema with root type "array"`,
		`{"type":["object"]}`:             `an inputSchema whose root type is not the string "object"`,
		`true`:                            "an inputSchema that is not a JSON object",
		`null`:                            "an inputSchema that is not a JSON object",
		`[{"type":"object"}]`:             "an inputSchema that is not a JSON object",
		`{"not valid json`:                "an inputSchema that is not a JSON object",
		`{"type":"object"}`:               "",
		`{"$schema":"x","type":"object"}`: "",
	}
	for schema, want := range cases {
		if got := schemaRootViolation(json.RawMessage(schema), false); got != want {
			t.Fatalf("schema %q: got %q, want %q", schema, got, want)
		}
	}
}

// TestSchemaRootIsUnverifiableAfterRedaction. --redact-path is the documented
// way to scrub something inside a schema, and it can replace the schema itself
// or just its root type. Reading either placeholder as a missing root type
// reports a conforming server for the user's own privacy setting, on the axis
// the follow-up wires to a warning that fails a default check run. The sibling
// x-mcp-header check answers the same input the same way and says why.
func TestSchemaRootIsUnverifiableAfterRedaction(t *testing.T) {
	for name, schema := range map[string]string{
		"the whole schema":  `"[REDACTED]"`,
		"the root type":     `{"type":"[REDACTED]","properties":{"q":{"type":"string"}}}`,
		"type and the rest": `{"$schema":"[REDACTED]","type":"[REDACTED]"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if got := schemaRootViolation(json.RawMessage(schema), true); got != "" {
				t.Fatalf("redacted schema reported %q, want silence", got)
			}
			// Only mcpsnoop's own rewriting earns that silence. The placeholder is a
			// string either peer may send, so a check that stops at the bytes alone is
			// a check the traffic can switch off.
			if got := schemaRootViolation(json.RawMessage(schema), false); got == "" {
				t.Fatal("an unredacted capture spelling the placeholder must still be judged")
			}
		})
	}

	// Redaction excuses the root verdict and nothing else. A scrubbed root does
	// not hide what the rest of the document uses.
	got := analyzeSchema(json.RawMessage(`{"type":"[REDACTED]","properties":{"a":{"oneOf":[{"type":"string"}]}}}`), true)
	if slices.Contains(got, SchemaFinding{Kind: FindingNonObjectRoot}) {
		t.Fatalf("got %v, want no %s finding on a redacted root", got, FindingNonObjectRoot)
	}
	if !slices.Contains(got, SchemaFinding{Kind: FindingOneOf}) {
		t.Fatalf("got %v, want the oneOf the walk can still see", got)
	}
}

// TestToolsListVerdictSurvivesRedaction drives the whole ingest path, since the
// flag has to travel from the envelope through completeCall and applyToolsList
// to reach the check. A unit test on schemaRootViolation alone would pass with
// the wiring cut.
func TestToolsListVerdictSurvivesRedaction(t *testing.T) {
	const listing = `"result":{"tools":[{"name":"search","inputSchema":{"type":"object","properties":{"q":{"type":"string"}}}}]}`
	findings := func(redacted bool, raw string) []SchemaFinding {
		t.Helper()
		s := New()
		t0 := time.Unix(0, 0)
		s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/list", `{}`))
		e := resp(2, t0, proxy.ServerToClient, "1", raw)
		e.Redacted = redacted
		s.Ingest(e)
		definitions, ok := s.ToolDefinitions("s1")
		if !ok || len(definitions) != 1 {
			t.Fatalf("tool definitions = %v, ok = %v", definitions, ok)
		}
		return definitions[0].Findings
	}

	if got := findings(false, listing); len(got) != 0 {
		t.Fatalf("a conforming listing reported %v", got)
	}
	// What --redact-path '$.result.tools[*].inputSchema' leaves behind.
	if got := findings(true, `"result":{"tools":[{"name":"search","inputSchema":"[REDACTED]"}]}`); len(got) != 0 {
		t.Fatalf("a scrubbed schema reported %v, which accuses the server of the user's own redaction", got)
	}
	// What --redact-path '$..type' leaves behind.
	if got := findings(true, `"result":{"tools":[{"name":"search","inputSchema":{"type":"[REDACTED]"}}]}`); len(got) != 0 {
		t.Fatalf("a scrubbed root type reported %v", got)
	}
	// A genuinely rootless schema on a redacted frame is still the server's doing.
	if got := findings(true, `"result":{"tools":[{"name":"search","inputSchema":{"type":"array"}}]}`); len(got) == 0 {
		t.Fatal("redaction must not excuse a root type the server really sent")
	}
}
