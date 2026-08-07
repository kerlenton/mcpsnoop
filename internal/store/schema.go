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
	FindingNonObjectRoot   SchemaFindingKind = "nonObjectRoot"
	FindingNonDefaultDialect SchemaFindingKind = "nonDefaultDialect"
	FindingOneOf           SchemaFindingKind = "oneOf"
	FindingAnyOf           SchemaFindingKind = "anyOf"
	FindingAllOf           SchemaFindingKind = "allOf"
	FindingNot             SchemaFindingKind = "not"
	FindingRef             SchemaFindingKind = "ref"
	FindingExternalRef     SchemaFindingKind = "externalRef"
	FindingUntypedProperty SchemaFindingKind = "untypedProperty"
)

// jsonSchema2020_12 is the dialect MCP defaults to when $schema is absent.
const jsonSchema2020_12 = "https://json-schema.org/draft/2020-12/schema"

// ObservationalSchemaKinds are findings that never fail check unless the schema
// signal is selected. nonObjectRoot is a violation and is surfaced as a warning
// on the tools/list frame instead.
var ObservationalSchemaKinds = []SchemaFindingKind{
	FindingNonDefaultDialect,
	FindingOneOf,
	FindingAnyOf,
	FindingAllOf,
	FindingNot,
	FindingRef,
	FindingExternalRef,
	FindingUntypedProperty,
}

type SchemaFinding struct {
	Kind SchemaFindingKind
}

// analyzeInputSchema reports constructs in a tool's inputSchema, including a
// root that is not type object, which the MCP spec requires.
func analyzeInputSchema(raw json.RawMessage) []SchemaFinding {
	if len(raw) == 0 {
		return []SchemaFinding{{Kind: FindingNonObjectRoot}}
	}
	return analyzeSchema(raw, true)
}

// analyzeOutputSchema reports constructs in a tool's outputSchema. The spec
// places no root-type constraint on outputSchema, so only dialect and the
// composition walk apply.
func analyzeOutputSchema(raw json.RawMessage) []SchemaFinding {
	if len(raw) == 0 {
		return nil
	}
	return analyzeSchema(raw, false)
}

// analyzeSchema reports the constructs a tool's advertised schema uses that are
// known to travel badly. Nothing is resolved or fetched: an external $ref is
// recognized by its form alone.
//
// Findings are deduplicated by kind. A finding carries only its kind, so two
// entries of the same kind are indistinguishable and add nothing; collapsing
// them lets a caller treat "more than one finding" as "more than one kind of
// problem", which is the question a reader actually has.
func analyzeSchema(raw json.RawMessage, checkRoot bool) []SchemaFinding {
	if len(raw) == 0 {
		return nil
	}

	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		if checkRoot {
			return []SchemaFinding{{Kind: FindingNonObjectRoot}}
		}
		return nil
	}

	var findings []SchemaFinding
	seen := make(map[SchemaFindingKind]bool, 9)
	if checkRoot && !hasObjectRoot(v) {
		addFinding(&findings, seen, FindingNonObjectRoot)
	}
	if dialect := nonDefaultDialect(v); dialect {
		addFinding(&findings, seen, FindingNonDefaultDialect)
	}
	walkSchema(v, &findings, seen)
	return findings
}

// hasObjectRoot is true when v is a JSON object whose type is the string
// "object". Absent type, any other type value, or a non-object node all fail.
func hasObjectRoot(v any) bool {
	node, ok := v.(map[string]any)
	if !ok {
		return false
	}
	t, ok := node["type"]
	if !ok {
		return false
	}
	s, ok := t.(string)
	return ok && s == "object"
}

// nonDefaultDialect is true when the schema object carries a $schema URI that
// is not the 2020-12 dialect MCP defaults to.
func nonDefaultDialect(v any) bool {
	node, ok := v.(map[string]any)
	if !ok {
		return false
	}
	schemaURI, ok := node["$schema"].(string)
	if !ok || schemaURI == "" {
		return false
	}
	return !isJSONSchema2020_12(schemaURI)
}

func isJSONSchema2020_12(uri string) bool {
	uri = strings.TrimSuffix(uri, "#")
	return uri == jsonSchema2020_12
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

// mergeSchemaFindings appends kinds from extra that are not already in base.
func mergeSchemaFindings(base, extra []SchemaFinding) []SchemaFinding {
	if len(extra) == 0 {
		return base
	}
	seen := make(map[SchemaFindingKind]bool, len(base)+len(extra))
	for _, f := range base {
		seen[f.Kind] = true
	}
	for _, f := range extra {
		if seen[f.Kind] {
			continue
		}
		seen[f.Kind] = true
		base = append(base, f)
	}
	return base
}

// SchemaFindingKinds returns the kind strings present in findings, in a stable
// order for display and export.
func SchemaFindingKinds(findings []SchemaFinding) []SchemaFindingKind {
	seen := make(map[SchemaFindingKind]bool, len(findings))
	var kinds []SchemaFindingKind
	for _, f := range findings {
		if seen[f.Kind] {
			continue
		}
		seen[f.Kind] = true
		kinds = append(kinds, f.Kind)
	}
	return kinds
}

// IsObservationalSchemaKind is true for findings that never fail check unless
// the schema signal is selected.
func IsObservationalSchemaKind(kind SchemaFindingKind) bool {
	for _, k := range ObservationalSchemaKinds {
		if k == kind {
			return true
		}
	}
	return false
}
