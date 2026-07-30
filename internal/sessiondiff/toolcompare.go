package sessiondiff

import (
	"bytes"
	"encoding/json"
	"strconv"

	"github.com/kerlenton/mcpsnoop/internal/jsonwire"
)

// jsonSchemaDialect2020 is the dialect both inputSchema and outputSchema default
// to when a server declares no $schema of its own.
const jsonSchemaDialect2020 = "https://json-schema.org/draft/2020-12/schema"

// comparableSchema is the form a schema is compared in. It is canonical JSON
// with a redundant top-level $schema removed, because the spec makes 2020-12 the
// default for both schema fields: a server that starts spelling out the dialect
// it was already using has changed nothing, and reporting that as a contract
// change is the kind of false alarm that teaches people to ignore the real one.
//
// Only the top level, and only the exact default dialect. A nested $schema sits
// inside a subschema that may genuinely be another dialect, and a different
// dialect at the root is a real change in how the schema is interpreted.
//
// Deliberately not folded into canonicalJSON, which is shared with callArguments
// and whose output is printed to a terminal as the arguments of a real call.
func comparableSchema(raw json.RawMessage) string {
	var obj map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &obj) != nil {
		return canonicalJSON(raw) // absent, or not an object: nothing to strip
	}
	declared, ok := obj["$schema"]
	if !ok {
		return canonicalJSON(raw)
	}
	var dialect string
	if json.Unmarshal(declared, &dialect) != nil || !isDefaultDialect(dialect) {
		return canonicalJSON(raw)
	}
	delete(obj, "$schema")
	stripped, err := jsonwire.Marshal(obj)
	if err != nil {
		return canonicalJSON(raw)
	}
	return canonicalJSON(stripped)
}

// isDefaultDialect reports whether a $schema value names the dialect that would
// apply anyway. The trailing empty fragment is the same URI.
func isDefaultDialect(dialect string) bool {
	return dialect == jsonSchemaDialect2020 || dialect == jsonSchemaDialect2020+"#"
}

// annotationDefaults are the values the spec says apply when a hint is omitted.
// A server that starts sending one of these explicitly has said nothing new, so
// comparing the raw JSON would report drift on a routine refactor.
var annotationDefaults = map[string]bool{
	"readOnlyHint":    false,
	"destructiveHint": true,
	"idempotentHint":  false,
	"openWorldHint":   true,
}

// comparableAnnotations is the form annotations are compared in: every hint the
// spec gives a default is filled in, so absent, null, {} and the fully spelled
// out defaults all read as the same thing.
//
// destructiveHint and idempotentHint are resolved even though the spec calls
// them meaningful only when readOnlyHint is false. Skipping them there would
// mean a later readOnlyHint flip activates a hint nobody ever approved. The
// price is that editing an inert hint reports drift, which is the safe
// direction.
//
// Unknown keys, annotations.title among them, pass through canonicalised, so a
// change to one still reports.
func comparableAnnotations(raw json.RawMessage) string {
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || string(trimmed) == "null" {
		raw = json.RawMessage("{}")
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		// A string, an array, a number. Kept distinct from the all-defaults form
		// rather than resolved onto it: the server sent something, and saying it
		// matches an absent annotations block would hide the moment it appeared.
		return "malformed " + canonicalJSON(raw)
	}
	resolved := make(map[string]json.RawMessage, len(obj)+len(annotationDefaults))
	for key, value := range obj {
		resolved[key] = value
	}
	for hint, fallback := range annotationDefaults {
		value, present := obj[hint]
		if !present {
			resolved[hint] = boolJSON(fallback)
			continue
		}
		var flag bool
		if json.Unmarshal(value, &flag) != nil {
			// Present but not a boolean. Never fall back to the default here: a
			// baseline with readOnlyHint absent would then match a server now
			// sending the string "true", while a lenient client renders the tool as
			// read-only. The canonical bytes ride along so two different bad values
			// stay different.
			resolved[hint] = json.RawMessage(strconv.Quote("invalid " + canonicalJSON(value)))
			continue
		}
		resolved[hint] = boolJSON(flag)
	}
	filled, err := jsonwire.Marshal(resolved)
	if err != nil {
		return canonicalJSON(raw)
	}
	return canonicalJSON(filled)
}

func boolJSON(v bool) json.RawMessage {
	if v {
		return json.RawMessage("true")
	}
	return json.RawMessage("false")
}
