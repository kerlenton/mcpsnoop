package store

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestAnalyzeSchemaClean(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"query":{"type":"string"},
			"limit":{"type":"integer"}
		}
	}`)

	got := analyzeSchema(schema)

	if len(got) != 0 {
		t.Fatalf("got %v findings, want none", got)
	}
}

func TestAnalyzeSchemaOneOf(t *testing.T) {
	schema := json.RawMessage(`{
		"oneOf": [
			{"type":"string"},
			{"type":"integer"}
		]
	}`)

	got := analyzeSchema(schema)

	want := []SchemaFinding{
		{Kind: FindingOneOf},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAnalyzeSchemaAnyOf(t *testing.T) {
	schema := json.RawMessage(`{
		"anyOf": [
			{"type":"string"},
			{"type":"integer"}
		]
	}`)

	got := analyzeSchema(schema)

	want := []SchemaFinding{
		{Kind: FindingAnyOf},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAnalyzeSchemaAllOf(t *testing.T) {
	schema := json.RawMessage(`{
		"allOf": [
			{"type":"string"},
			{"type":"integer"}
		]
	}`)

	got := analyzeSchema(schema)

	want := []SchemaFinding{
		{Kind: FindingAllOf},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAnalyzeSchemaNot(t *testing.T) {
	schema := json.RawMessage(`{
		"not": {
			"type":"string"
		}
	}`)

	got := analyzeSchema(schema)

	want := []SchemaFinding{
		{Kind: FindingNot},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAnalyzeSchemaRef(t *testing.T) {
	schema := json.RawMessage(`{
		"$ref": "#/$defs/Foo"
	}`)

	got := analyzeSchema(schema)

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

	got := analyzeSchema(schema)

	if len(got) != 0 {
		t.Fatalf("got %v, want none", got)
	}
}

func TestAnalyzeSchemaExternalRef(t *testing.T) {
	schema := json.RawMessage(`{
		"$ref": "https://example.com/schema.json"
	}`)

	got := analyzeSchema(schema)

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

	got := analyzeSchema(schema)

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

	got := analyzeSchema(schema)

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

	got := analyzeSchema(schema)

	for _, finding := range got {
		if finding.Kind == FindingUntypedProperty {
			t.Fatalf("got %v, top-level schema must not be treated as a property", got)
		}
	}
}
