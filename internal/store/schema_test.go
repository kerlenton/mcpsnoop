package store

import (
	"encoding/json"
	"reflect"
	"strconv"
	"testing"
)

func TestAnalyzeInputSchemaClean(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"query":{"type":"string"},
			"limit":{"type":"integer"}
		}
	}`)

	got := analyzeInputSchema(schema)

	if len(got) != 0 {
		t.Fatalf("got %v findings, want none", got)
	}
}

func TestAnalyzeInputSchemaOneOf(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"value":{"oneOf":[
				{"type":"string"},
				{"type":"integer"}
			]}
		}
	}`)

	got := analyzeInputSchema(schema)

	want := []SchemaFinding{
		{Kind: FindingOneOf},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAnalyzeInputSchemaAnyOf(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"value":{"anyOf":[
				{"type":"string"},
				{"type":"integer"}
			]}
		}
	}`)

	got := analyzeInputSchema(schema)

	want := []SchemaFinding{
		{Kind: FindingAnyOf},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAnalyzeInputSchemaAllOf(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"value":{"allOf":[
				{"type":"string"},
				{"type":"integer"}
			]}
		}
	}`)

	got := analyzeInputSchema(schema)

	want := []SchemaFinding{
		{Kind: FindingAllOf},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAnalyzeInputSchemaNot(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"value":{"not":{"type":"string"}}
		}
	}`)

	got := analyzeInputSchema(schema)

	want := []SchemaFinding{
		{Kind: FindingNot},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAnalyzeInputSchemaRef(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"u":{"$ref":"#/$defs/Foo"}
		}
	}`)

	got := analyzeInputSchema(schema)

	want := []SchemaFinding{
		{Kind: FindingRef},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAnalyzeInputSchemaPropertyNamedLikeKeyword(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"oneOf": {
				"type": "string"
			}
		}
	}`)

	got := analyzeInputSchema(schema)

	if len(got) != 0 {
		t.Fatalf("got %v, want none", got)
	}
}

func TestAnalyzeInputSchemaExternalRef(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"u":{"$ref":"https://example.com/schema.json"}
		}
	}`)

	got := analyzeInputSchema(schema)

	want := []SchemaFinding{
		{Kind: FindingExternalRef},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAnalyzeInputSchemaUntypedProperty(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {}
		}
	}`)

	got := analyzeInputSchema(schema)

	want := []SchemaFinding{
		{Kind: FindingUntypedProperty},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAnalyzeInputSchemaNestedUntypedProperty(t *testing.T) {
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

	got := analyzeInputSchema(schema)

	want := []SchemaFinding{
		{Kind: FindingUntypedProperty},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAnalyzeInputSchemaTopLevelWithoutTypeIsNotFlaggedAsUntypedProperty(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"config":{"oneOf":[
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
			]}
		}
	}`)

	got := analyzeInputSchema(schema)

	for _, finding := range got {
		if finding.Kind == FindingUntypedProperty {
			t.Fatalf("got %v, top-level schema must not be treated as a property", got)
		}
	}
}

func TestAnalyzeInputSchemaConstructOnAPropertyIsNotAlsoUntyped(t *testing.T) {
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
			got := analyzeInputSchema(json.RawMessage(tc.schema))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAnalyzeInputSchemaEnumAndConstAreNotUntyped(t *testing.T) {
	for _, schema := range []string{
		`{"type":"object","properties":{"mode":{"enum":["fast","slow"]}}}`,
		`{"type":"object","properties":{"version":{"const":2}}}`,
	} {
		if got := analyzeInputSchema(json.RawMessage(schema)); len(got) != 0 {
			t.Fatalf("analyzeInputSchema(%s) = %v, want no findings", schema, got)
		}
	}
}

func TestAnalyzeInputSchemaDeduplicatesByKind(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"a":{"description":"no type"},
			"b":{"description":"also no type"},
			"c":{"description":"still no type"}
		}
	}`)

	got := analyzeInputSchema(schema)
	want := []SchemaFinding{{Kind: FindingUntypedProperty}}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAnalyzeInputSchemaReachesEveryContainer(t *testing.T) {
	cases := []struct {
		name   string
		schema string
		want   SchemaFindingKind
	}{
		{"additionalProperties", `{"type":"object","additionalProperties":{"oneOf":[{"type":"string"}]}}`, FindingOneOf},
		{"if", `{"type":"object","if":{"oneOf":[{"type":"string"}]}}`, FindingOneOf},
		{"then", `{"type":"object","then":{"$ref":"https://example.com/x.json"}}`, FindingExternalRef},
		{"else", `{"type":"object","else":{"anyOf":[{"type":"string"}]}}`, FindingAnyOf},
		{"contains", `{"type":"object","properties":{"items":{"type":"array","contains":{"allOf":[{"type":"string"}]}}}}`, FindingAllOf},
		{"prefixItems", `{"type":"object","properties":{"items":{"type":"array","prefixItems":[{"oneOf":[{"type":"string"}]}]}}}`, FindingOneOf},
		{"patternProperties", `{"type":"object","patternProperties":{"^a":{"$ref":"#/$defs/A"}}}`, FindingRef},
		{"$defs", `{"type":"object","$defs":{"X":{"oneOf":[{"type":"string"}]}}}`, FindingOneOf},
		{"definitions", `{"type":"object","definitions":{"X":{"anyOf":[{"type":"string"}]}}}`, FindingAnyOf},
		{"dependentSchemas", `{"type":"object","dependentSchemas":{"a":{"allOf":[{"type":"string"}]}}}`, FindingAllOf},
		{"propertyNames", `{"type":"object","propertyNames":{"$ref":"#/$defs/N"}}`, FindingRef},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := analyzeInputSchema(json.RawMessage(tc.schema))
			if len(got) != 1 || got[0].Kind != tc.want {
				t.Fatalf("got %v, want exactly [%s]", got, tc.want)
			}
		})
	}
}

func TestAnalyzeInputSchemaClassifiesRefsByFormOnly(t *testing.T) {
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
		got := analyzeInputSchema(schema)
		if len(got) != 1 || got[0].Kind != want {
			t.Fatalf("$ref %q: got %v, want [%s]", ref, got, want)
		}
	}
}

func TestAnalyzeInputSchemaNonObjectRoot(t *testing.T) {
	cases := []string{
		`{"$schema":"http://json-schema.org/draft-07/schema#"}`,
		`{}`,
		`{"type":"array","items":{"type":"string"}}`,
		`true`,
		`null`,
	}
	for _, schema := range cases {
		got := analyzeInputSchema(json.RawMessage(schema))
		if len(got) == 0 || got[0].Kind != FindingNonObjectRoot {
			t.Fatalf("analyzeInputSchema(%s) = %v, want nonObjectRoot", schema, got)
		}
	}
}

func TestAnalyzeInputSchemaAbsentIsNonObjectRoot(t *testing.T) {
	got := analyzeInputSchema(nil)
	if len(got) != 1 || got[0].Kind != FindingNonObjectRoot {
		t.Fatalf("got %v, want nonObjectRoot for absent schema", got)
	}
}

func TestAnalyzeInputSchemaValidNoParamForms(t *testing.T) {
	for _, schema := range []string{
		`{"type":"object"}`,
		`{"type":"object","additionalProperties":false}`,
	} {
		if got := analyzeInputSchema(json.RawMessage(schema)); len(got) != 0 {
			t.Fatalf("analyzeInputSchema(%s) = %v, want no findings", schema, got)
		}
	}
}

func TestAnalyzeOutputSchemaDoesNotReportNonObjectRoot(t *testing.T) {
	schema := json.RawMessage(`{"type":"array","items":{"type":"string"}}`)
	got := analyzeOutputSchema(schema)
	for _, f := range got {
		if f.Kind == FindingNonObjectRoot {
			t.Fatalf("output schema must not report nonObjectRoot, got %v", got)
		}
	}
}

func TestAnalyzeSchemaNonDefaultDialect(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"$schema":"http://json-schema.org/draft-07/schema#",
		"properties":{"path":{"type":"string"}}
	}`)
	got := analyzeInputSchema(schema)
	found := false
	for _, f := range got {
		if f.Kind == FindingNonDefaultDialect {
			found = true
		}
	}
	if !found {
		t.Fatalf("got %v, want nonDefaultDialect", got)
	}
}

func TestAnalyzeSchemaDefaultDialectIsSilent(t *testing.T) {
	for _, schema := range []string{
		`{"type":"object","properties":{"q":{"type":"string"}}}`,
		`{"type":"object","$schema":"https://json-schema.org/draft/2020-12/schema","properties":{"q":{"type":"string"}}}`,
		`{"type":"object","$schema":"https://json-schema.org/draft/2020-12/schema#","properties":{"q":{"type":"string"}}}`,
	} {
		got := analyzeInputSchema(json.RawMessage(schema))
		for _, f := range got {
			if f.Kind == FindingNonDefaultDialect {
				t.Fatalf("analyzeInputSchema(%s) = %v, want no dialect finding", schema, got)
			}
		}
	}
}

func TestAnalyzeOutputSchemaReportsDialect(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"array",
		"$schema":"http://json-schema.org/draft-07/schema#",
		"items":{"type":"string"}
	}`)
	got := analyzeOutputSchema(schema)
	found := false
	for _, f := range got {
		if f.Kind == FindingNonDefaultDialect {
			found = true
		}
		if f.Kind == FindingNonObjectRoot {
			t.Fatalf("output schema must not report nonObjectRoot")
		}
	}
	if !found {
		t.Fatalf("got %v, want nonDefaultDialect on outputSchema", got)
	}
}
