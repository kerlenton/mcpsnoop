package store

import (
	"encoding/json"
	"strconv"
	"strings"
)

// SchemaFindingKind names something worth knowing about a tool's advertised
// input schema. All but one are observations rather than verdicts: a schema
// using oneOf is not wrong, only likely to be interpreted differently across
// clients. FindingNonObjectRoot is the exception and says so on its own doc.
type SchemaFindingKind string

const (
	// FindingNonObjectRoot is the one kind here that is a violation rather than
	// an observation. Every other finding says a schema may travel badly; this
	// one says a conforming client rejects the tool outright, so it is reported
	// as a warning on the tools/list frame as well as here.
	FindingNonObjectRoot SchemaFindingKind = "nonObjectRoot"
	// FindingNonDefaultDialect is an observation like the rest. A schema may
	// declare any dialect it likes; saying so only means a client has to read it
	// as that dialect rather than the 2020-12 the revision defaults to.
	FindingNonDefaultDialect SchemaFindingKind = "nonDefaultDialect"
	FindingOneOf             SchemaFindingKind = "oneOf"
	FindingAnyOf             SchemaFindingKind = "anyOf"
	FindingAllOf             SchemaFindingKind = "allOf"
	FindingNot               SchemaFindingKind = "not"
	FindingRef               SchemaFindingKind = "ref"
	FindingExternalRef       SchemaFindingKind = "externalRef"
	FindingUntypedProperty   SchemaFindingKind = "untypedProperty"
)

// jsonSchema2020_12 is the dialect the 2026-07-28 Tool definition defaults to,
// "Defaults to JSON Schema 2020-12 when no explicit `$schema` is provided", so a
// schema that declares nothing declares this.
const jsonSchema2020_12 = "https://json-schema.org/draft/2020-12/schema"

// SchemaFindingKinds is every kind analyzeSchema can report, in the order the
// constants declare them. Exported for the same reason ToolDriftKinds is: a
// renderer that ranks or labels kinds from a hand-kept list of its own has no
// way to notice a kind added here, and would show the raw enum name for it.
var SchemaFindingKinds = []SchemaFindingKind{
	FindingNonObjectRoot,
	FindingNonDefaultDialect,
	FindingOneOf,
	FindingAnyOf,
	FindingAllOf,
	FindingNot,
	FindingRef,
	FindingExternalRef,
	FindingUntypedProperty,
}

// ObservationalSchemaKinds are the kinds that never fail check unless the schema
// signal is selected. It is SchemaFindingKinds minus the one violation, which
// reaches the default gate as a warning on the frame that advertised the tool.
var ObservationalSchemaKinds = SchemaFindingKinds[1:]

// IsObservationalSchemaKind reports whether a kind is an observation rather than
// the violation.
func IsObservationalSchemaKind(kind SchemaFindingKind) bool {
	return kind != FindingNonObjectRoot
}

type SchemaFinding struct {
	Kind SchemaFindingKind
}

// analyzeInputSchema reports what is worth knowing about a tool's inputSchema,
// the root-object rule included, since that one is a rule only inputSchema has.
func analyzeInputSchema(raw json.RawMessage, redacted bool) []SchemaFinding {
	return analyzeSchema(raw, redacted, true)
}

// analyzeOutputSchema reports the same, minus the root-object rule. On
// 2026-07-28 the outputSchema entry carries no "required" and no const on its
// type, and the Tools page shows an array-rooted outputSchema in its list_users
// example, so a root that is not an object is legal there. An absent
// outputSchema is nothing at all, since the field is optional.
func analyzeOutputSchema(raw json.RawMessage, redacted bool) []SchemaFinding {
	if len(raw) == 0 {
		return nil
	}
	return analyzeSchema(raw, redacted, false)
}

// analyzeSchema reports the constructs a tool's advertised schema uses that are
// known to travel badly. Nothing is resolved or fetched: an external $ref is
// recognized by its form alone.
//
// Findings are deduplicated by kind. A finding carries only its kind, so two
// entries of the same kind are indistinguishable and add nothing; collapsing
// them lets a caller treat "more than one finding" as "more than one kind of
// problem", which is the question a reader actually has.
func analyzeSchema(raw json.RawMessage, redacted, checkRoot bool) []SchemaFinding {
	var findings []SchemaFinding
	seen := make(map[SchemaFindingKind]bool, len(SchemaFindingKinds))

	// First, so the violation leads the list the walk order would otherwise
	// decide. A rootless schema still gets walked, because naming the construct
	// it uses is worth having next to the reason a client refused it.
	if checkRoot && schemaRootViolation(raw, redacted) != "" {
		addFinding(&findings, seen, FindingNonObjectRoot)
	}

	if len(raw) == 0 {
		return findings
	}

	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return findings
	}

	if declaresNonDefaultDialect(v, redacted) {
		addFinding(&findings, seen, FindingNonDefaultDialect)
	}
	walkSchema(v, &findings, seen)
	return findings
}

// declaresNonDefaultDialect reports whether the schema names a $schema other
// than the 2020-12 the revision defaults to. Declaring one is legal, so this is
// an observation: it says a client has to read the document as that dialect, and
// a client that does not is reading it as something else.
//
// A scrubbed $schema is not a dialect. Reading the placeholder as one would
// report a conforming server for the user's own redaction, the same way reading
// a scrubbed root type as a missing one did.
func declaresNonDefaultDialect(v any, redacted bool) bool {
	node, ok := v.(map[string]any)
	if !ok {
		return false
	}
	uri, ok := node["$schema"].(string)
	if !ok || uri == "" {
		return false
	}
	if unverifiableAfterRedaction(redacted, uri) {
		return false
	}
	// The trailing "#" is the empty fragment, which names the same document, and
	// both spellings are in the wild.
	return strings.TrimSuffix(uri, "#") != jsonSchema2020_12
}

// schemaRootViolation reports how a tool's inputSchema fails the root-object
// rule, in words naming what was seen, or "" when it satisfies it or when
// mcpsnoop cannot tell. Nothing is resolved and no dialect is interpreted: the
// verdict is decided by the bytes.
//
// $defs.Tool.inputSchema carries "required": ["type"] with "type" a const of
// "object", and $defs.Tool.required is ["inputSchema", "name"], so an absent
// inputSchema fails the same way a rootless one does. The identical constraint
// appears in the 2026-07-28, 2025-11-25 and 2025-06-18 schema files, which is
// why this needs no revision gate.
//
// The rule is an inputSchema rule only. On 2026-07-28 outputSchema's entry has
// no "required" and no const, and the Tools page shows an array-rooted
// outputSchema in its list_users example, so this is never asked about one.
// Extending it there would need the revision gate this does not, since
// 2025-11-25 and 2025-06-18 do constrain an outputSchema root to "object".
//
// redacted says the frame passed through mcpsnoop's own redaction, which is what
// makes the placeholder below trustworthy as a placeholder rather than a value
// a peer chose to send.
func schemaRootViolation(raw json.RawMessage, redacted bool) string {
	if len(raw) == 0 {
		return "no inputSchema"
	}
	// A schema the user asked mcpsnoop to scrub is unreadable, not wrong. Saying
	// otherwise turns a privacy setting into an accusation against a server that
	// was conforming, which is the answer the sibling x-mcp-header check already
	// gives for the same input and the reason it gives it. --redact-path is the
	// documented way to name something inside a schema, so this is a supported
	// workflow rather than a misuse.
	if unverifiableAfterRedaction(redacted, redactedSchemaRoot(raw)) {
		return ""
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

// redactedSchemaRoot returns whichever of the two things a redaction rule can
// replace decides the root verdict, the whole schema or just its type, so one
// call covers both. Anything else it returns is a value that was never going to
// match the placeholder anyway.
func redactedSchemaRoot(raw json.RawMessage) string {
	var whole string
	if json.Unmarshal(raw, &whole) == nil {
		return whole
	}
	var root struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &root)
	return root.Type
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

// mergeSchemaFindings appends the kinds of extra that base does not already
// carry, so a tool's findings stay one entry per kind across the two schemas it
// can advertise.
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

// findingKinds is the kinds present in findings, in the order they were found.
// Named for what it returns rather than for the type, since SchemaFindingKinds
// is the list of every kind that exists.
func findingKinds(findings []SchemaFinding) []SchemaFindingKind {
	kinds := make([]SchemaFindingKind, 0, len(findings))
	for _, f := range findings {
		kinds = append(kinds, f.Kind)
	}
	return kinds
}
