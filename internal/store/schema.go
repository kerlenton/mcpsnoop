package store

import (
	"encoding/json"
	"strconv"
	"strings"
)

// SchemaFindingKind names a JSON Schema construct that clients handle
// inconsistently. A finding is an observation, not a verdict: a schema using
// oneOf is not wrong, only likely to be interpreted differently across clients.
type SchemaFindingKind string

const (
	// FindingNonObjectRoot is the one kind here that is a violation rather than
	// an observation. Every other finding says a schema may travel badly; this
	// one says a conforming client rejects the tool outright.
	FindingNonObjectRoot   SchemaFindingKind = "nonObjectRoot"
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
	var findings []SchemaFinding
	seen := make(map[SchemaFindingKind]bool, 8)

	// First, so the violation leads the list the walk order would otherwise
	// decide. A rootless schema still gets walked, because naming the construct
	// it uses is worth having next to the reason a client refused it.
	if schemaRootViolation(raw) != "" {
		addFinding(&findings, seen, FindingNonObjectRoot)
	}

	if len(raw) == 0 {
		return findings
	}

	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return findings
	}

	walkSchema(v, &findings, seen)
	return findings
}

// schemaRootViolation reports how a tool's inputSchema fails the root-object
// rule, in words naming what was seen, or "" when it satisfies it. Nothing is
// resolved and no dialect is interpreted: the verdict is decided by the bytes.
//
// $defs.Tool.inputSchema carries "required": ["type"] with "type" a const of
// "object", and $defs.Tool.required is ["inputSchema", "name"], so an absent
// inputSchema fails the same way a rootless one does. The identical constraint
// appears in the 2026-07-28, 2025-11-25 and 2025-06-18 schema files, which is
// why this needs no revision gate.
//
// The rule is an inputSchema rule only. outputSchema's entry has no "required"
// and no const, and the Tools page shows an array-rooted outputSchema in its
// list_users example, so this is never asked about one.
func schemaRootViolation(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "no inputSchema"
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil || root == nil {
		// Covers true, false, null, an array, a number, a string, and bytes that
		// are not JSON at all. A boolean schema is valid JSON Schema in the
		// abstract and still not a tool inputSchema, which must be an object.
		return "an inputSchema that is not a JSON object"
	}
	switch t := root["type"].(type) {
	case nil:
		if _, present := root["type"]; present {
			return "an inputSchema with a null root type"
		}
		return "an inputSchema with no root type"
	case string:
		if t == "object" {
			return ""
		}
		return "an inputSchema with root type " + strconv.Quote(t)
	default:
		// A type union like ["object","null"] is not the const the schema demands,
		// and a client validating against $defs.Tool rejects it the same way.
		return `an inputSchema whose root type is not the string "object"`
	}
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
