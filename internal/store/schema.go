package store

import (
	"encoding/json"
	"strings"
)

// SchemaFindingKind names a JSON Schema construct that clients handle
// inconsistently. A finding is an observation, not a verdict: a schema using
// oneOf is not wrong, only likely to be interpreted differently across clients.
type SchemaFindingKind string

const (
	FindingOneOf           SchemaFindingKind = "oneOf"
	FindingAnyOf           SchemaFindingKind = "anyOf"
	FindingAllOf           SchemaFindingKind = "allOf"
	FindingNot             SchemaFindingKind = "not"
	FindingRef             SchemaFindingKind = "ref"
	FindingExternalRef     SchemaFindingKind = "externalRef"
	FindingUntypedProperty SchemaFindingKind = "untypedProperty"
)

type SchemaFinding struct {
	Kind SchemaFindingKind
}

// analyzeSchema reports the constructs a tool's advertised input schema uses
// that are known to travel badly. Nothing is resolved or fetched: an external
// $ref is recognized by its form alone.
//
// Findings are deduplicated by kind. A finding carries only its kind, so two
// entries of the same kind are indistinguishable and add nothing; collapsing
// them lets a caller treat "more than one finding" as "more than one kind of
// problem", which is the question a reader actually has.
func analyzeSchema(raw json.RawMessage) []SchemaFinding {
	if len(raw) == 0 {
		return nil
	}

	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}

	var findings []SchemaFinding
	seen := make(map[SchemaFindingKind]bool, 7)
	walkSchema(v, &findings, seen)
	return findings
}

func addFinding(findings *[]SchemaFinding, seen map[SchemaFindingKind]bool, kind SchemaFindingKind) {
	if seen[kind] {
		return
	}
	seen[kind] = true
	*findings = append(*findings, SchemaFinding{Kind: kind})
}

// typingKeywords are the ways a subschema can say what it accepts other than
// with an explicit "type". A property using any of them is typed, just not
// directly, so flagging it as untyped would be wrong and would also bury the
// construct it actually uses.
var typingKeywords = []string{"$ref", "oneOf", "anyOf", "allOf", "not", "enum", "const"}

func isTyped(schema map[string]any) bool {
	if _, ok := schema["type"]; ok {
		return true
	}
	for _, k := range typingKeywords {
		if _, ok := schema[k]; ok {
			return true
		}
	}
	return false
}

func walkSchema(v any, findings *[]SchemaFinding, seen map[SchemaFindingKind]bool) {
	node, ok := v.(map[string]any)
	if !ok {
		return
	}

	for _, key := range []struct {
		name string
		kind SchemaFindingKind
	}{
		{"oneOf", FindingOneOf},
		{"anyOf", FindingAnyOf},
		{"allOf", FindingAllOf},
		{"not", FindingNot},
	} {
		if _, ok := node[key.name]; ok {
			addFinding(findings, seen, key.kind)
		}
	}
	if ref, ok := node["$ref"].(string); ok {
		// A reference starting with # points inside this document. Anything else
		// points outside it, which is both a portability problem and the case the
		// spec warns implementers not to follow blindly.
		kind := FindingExternalRef
		if strings.HasPrefix(ref, "#") {
			kind = FindingRef
		}
		addFinding(findings, seen, kind)
	}

	if props, ok := node["properties"].(map[string]any); ok {
		for _, child := range props {
			if schema, ok := child.(map[string]any); ok {
				if !isTyped(schema) {
					addFinding(findings, seen, FindingUntypedProperty)
				}
				walkSchema(schema, findings, seen)
			}
		}
	}

	// Subschemas that hold a list of schemas.
	for _, key := range []string{"oneOf", "anyOf", "allOf", "prefixItems"} {
		if arr, ok := node[key].([]any); ok {
			for _, child := range arr {
				walkSchema(child, findings, seen)
			}
		}
	}

	// Subschemas that hold a single schema. A construct nested in any of these
	// is just as real as one at the top, so the walk has to reach them.
	for _, key := range []string{"not", "items", "additionalProperties", "if", "then", "else", "contains", "propertyNames"} {
		if child, ok := node[key]; ok {
			walkSchema(child, findings, seen)
		}
	}

	// Subschemas held in a map keyed by name.
	for _, key := range []string{"$defs", "definitions", "patternProperties", "dependentSchemas"} {
		if group, ok := node[key].(map[string]any); ok {
			for _, child := range group {
				walkSchema(child, findings, seen)
			}
		}
	}
}
