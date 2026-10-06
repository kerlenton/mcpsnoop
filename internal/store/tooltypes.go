package store

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// typeViolation is one value in a tools/list result whose JSON type is not the
// one the specification gives it. The official SDK clients check these types on
// the listing as a whole, so one value they cannot read costs a client built on
// them every tool the server offers rather than the one that carried it. mcpsnoop
// decodes leniently and keeps showing the rest, and that gap between what the
// capture shows and what the client got is why each one is reported.
type typeViolation struct {
	// subject names what carries the value, a tool by its name or, when the entry
	// or its name is itself the problem, by its index in the page.
	subject string
	// complaint says what was sent and what was expected, in the warning's words.
	complaint string
	// from is the first revision that defines the field, so a session known to
	// speak an older one is not held to a type its revision never gave it.
	from string
}

func (v typeViolation) warning() string { return v.subject + " " + v.complaint }

// The first revision that defines each field the type check reads, taken from
// the released schema files. name, description and the listing's own fields are
// in 2024-11-05, annotations arrived in 2025-03-26 and the tool's own title in
// 2025-06-18, and none has changed type since.
const (
	toolsListFrom   = "2024-11-05"
	annotationsFrom = "2025-03-26"
	toolTitleFrom   = "2025-06-18"
)

// definitionRevision is the revision a tools/list response is judged by for
// types, the one its own request declared or, when it declared none, the one the
// session's handshake settled on. Falling back to the session is what the call's
// own protocolVersion warns against for revision-gated rules, and it is safe
// here only because of which way the gate points. The version can do no more
// than excuse a session known to predate a field. It never makes a warning
// appear that the types alone would not.
func definitionRevision(sess *session, c *call) string {
	if c != nil && c.protocolVersion != "" {
		return c.protocolVersion
	}
	return sess.caps.protocolVersion
}

// listingTypeViolations judges the two fields of a tools/list result that decide
// whether a client reads any tool at all. Only a field that is present is judged,
// since a result can carry neither, as a multi round-trip answer does, and
// judging an absent one would accuse those.
func listingTypeViolations(result json.RawMessage) []typeViolation {
	var fields map[string]json.RawMessage
	if json.Unmarshal(result, &fields) != nil {
		return nil
	}
	var wrong []typeViolation
	if tools, has := fields["tools"]; has && jsonKind(tools) != '[' {
		wrong = append(wrong, typeViolation{"the result", "sends tools as " + jsonType(tools) + " instead of an array", toolsListFrom})
	}
	if cursor, has := fields["nextCursor"]; has && jsonKind(cursor) != '"' {
		wrong = append(wrong, typeViolation{"the result", "sends nextCursor as " + jsonType(cursor) + " instead of a string", toolsListFrom})
	}
	return wrong
}

// entryTypeViolation judges one element of the tools array before anything else
// is read from it, returning the tool's name when it has a usable one. An entry
// that is not an object, or whose name is missing or not a string, has nothing
// to be reported under but its index.
func entryTypeViolation(rawTool, rawName json.RawMessage, index int) (name string, violation *typeViolation) {
	entry := "tools[" + strconv.Itoa(index) + "]"
	if jsonKind(rawTool) != '{' {
		return "", &typeViolation{entry, "is " + jsonType(rawTool) + " instead of a tool object", toolsListFrom}
	}
	if len(rawName) == 0 {
		return "", &typeViolation{entry, "is missing the name every tool requires", toolsListFrom}
	}
	if jsonKind(rawName) != '"' || json.Unmarshal(rawName, &name) != nil {
		return "", &typeViolation{entry, "sends name as " + jsonType(rawName) + " instead of a string", toolsListFrom}
	}
	return name, nil
}

// definitionTypeViolations judges the values of one named tool that mcpsnoop
// reads as values, its title, its description and its annotations. The input
// schema has its own rule in schemaRootViolation. A value mcpsnoop's own
// redaction rewrote is unreadable rather than wrong, so it is never reported.
func definitionTypeViolations(name string, title, description, annotations json.RawMessage, redacted bool) []typeViolation {
	subject := "tool " + strconv.Quote(name)
	var wrong []typeViolation
	for _, field := range []struct {
		key   string
		value json.RawMessage
		from  string
	}{{"title", title, toolTitleFrom}, {"description", description, toolsListFrom}} {
		if len(field.value) > 0 && jsonKind(field.value) != '"' && !partlyRedacted(redacted, string(field.value)) {
			wrong = append(wrong, typeViolation{subject, "sends " + field.key + " as " + jsonType(field.value) + " instead of a string", field.from})
		}
	}
	for _, complaint := range annotationViolations(annotations, redacted) {
		wrong = append(wrong, typeViolation{subject, "sends " + complaint, annotationsFrom})
	}
	return wrong
}

// jsonKind is the first byte of a JSON value, which is all it takes to tell its
// type once the value has decoded, or 0 for nothing at all.
func jsonKind(raw json.RawMessage) byte {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return 0
	}
	return trimmed[0]
}

// jsonType names the type of a JSON value for a warning, article included.
func jsonType(raw json.RawMessage) string {
	switch jsonKind(raw) {
	case 0:
		return "nothing"
	case '"':
		return "a string"
	case '{':
		return "an object"
	case '[':
		return "an array"
	case 't', 'f':
		return "a boolean"
	case 'n':
		return "null"
	}
	return "a number"
}
