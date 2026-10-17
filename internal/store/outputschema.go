package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

const (
	// outputSchemaFrom is the revision that gave a tool an outputSchema, and with
	// it the rule that "Servers MUST provide structured results that conform to
	// this schema".
	outputSchemaFrom = "2025-06-18"
	// defaultDialectFrom is the revision that made JSON Schema 2020-12 the dialect
	// of a schema that names none. Before it the dialect was not specified, and
	// draft-07 was what most generated schemas were written against.
	defaultDialectFrom = "2025-11-25"

	// The bounds the spec asks a validator to apply, so a hostile schema or an
	// outsized result cannot make the observer the slow part of the session.
	// Anything past them is left unjudged rather than judged in part.
	maxOutputSchemaBytes = 256 << 10
	maxStructuredBytes   = 4 << 20
	outputSchemaResource = "mcpsnoop:///output-schema.json"
)

// outputValidator is one tool's outputSchema, compiled the ways its revision
// can read it. A schema mcpsnoop could not compile is remembered as unusable, so
// it is tried once.
type outputValidator struct {
	unusable bool
	// declared says the schema names its dialect in $schema, which settles how
	// it is read whatever the revision.
	declared bool
	current  *jsonschema.Schema // as 2020-12, or as the dialect it declares
	draft07  *jsonschema.Schema // as draft-07, compiled when first needed
	triedOld bool
	raw      json.RawMessage
}

// outputSchemaWarning judges a tools/call result against the outputSchema its
// tool declared in this session's listing. It says nothing about what it cannot
// judge with certainty.
//
// What is judged is what the SDK clients reject, so the warning says what a real
// client will do with the result. The TypeScript and Python SDK clients both
// validate structuredContent against the outputSchema, both refuse a result
// that leaves structuredContent out, and both skip a result marked isError.
// format is not judged. 2020-12 makes it an annotation and the clients split on
// it, the TypeScript one asserting it and the Python one not, so a warning would
// accuse a server of breaking a rule the spec does not set.
//
// Left unjudged, and so never reported:
//   - a result that is not the call's answer yet, an input_required or a task
//   - a frame or a listing mcpsnoop's own redaction rewrote, since the
//     placeholder is not what the server sent
//   - a session known to speak a revision before outputSchema existed
//   - a schema that will not compile, names a dialect mcpsnoop does not know,
//     reaches outside itself with $ref, or is past the size bound
//   - a schema with no $schema in a session whose revision predates the default
//     dialect, unless it fails as both 2020-12 and draft-07
func (sess *session) outputSchemaWarning(c *call, result json.RawMessage, redacted bool) string {
	if c == nil || c.method != "tools/call" || c.toolName == "" || len(result) == 0 || redacted {
		return ""
	}
	revision := definitionRevision(sess, c)
	if revision != "" && !atLeastRevision(revision, outputSchemaFrom) {
		return ""
	}
	definition, listed := sess.toolDefinitions[c.toolName]
	if !listed || definition.listingRedacted || !declaresOutputSchema(definition.OutputSchema) {
		return ""
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(result, &fields) != nil {
		return ""
	}
	if isToolError(result) || !finalToolResult(fields) {
		return ""
	}
	tool := strconv.Quote(c.toolName)
	structured, has := fields["structuredContent"]
	if !has {
		return "tool " + tool + " declares an outputSchema and returned no structuredContent, which SDK clients reject"
	}
	if len(structured) > maxStructuredBytes {
		return ""
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(structured))
	if err != nil {
		return ""
	}

	v := sess.outputValidatorFor(c.toolName, definition.OutputSchema)
	if v.unusable {
		return ""
	}
	failure := v.current.Validate(instance)
	if failure == nil || refCycle(failure) {
		return ""
	}
	if !v.declared && (revision == "" || !atLeastRevision(revision, defaultDialectFrom)) {
		// The dialect is not settled for this revision, so the result breaks the
		// schema only if it breaks it however the schema is read.
		old := v.oldDialect()
		if old == nil || old.Validate(instance) == nil {
			return ""
		}
	}
	return "tool " + tool + " returned structuredContent that breaks its outputSchema" + describeFailure(failure)
}

// finalToolResult reports whether a result is the call's answer rather than a
// step towards one. A multi round-trip input request and a task handle, in the
// 2026 shape or the older one that wraps the task, carry their structuredContent
// in a later answer.
func finalToolResult(fields map[string]json.RawMessage) bool {
	if _, task := fields["task"]; task {
		return false
	}
	var resultType string
	if raw, has := fields["resultType"]; has && json.Unmarshal(raw, &resultType) != nil {
		return false
	}
	return resultType == "" || resultType == "complete"
}

// declaresOutputSchema reports whether a definition carries an outputSchema at
// all. An explicit null is a server saying it has none.
func declaresOutputSchema(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

// outputValidatorFor compiles a tool's outputSchema once per session. A tool's
// definition is the one its first listing gave, so the name is the key.
func (sess *session) outputValidatorFor(tool string, schema json.RawMessage) *outputValidator {
	if v, ok := sess.outputValidators[tool]; ok {
		return v
	}
	if sess.outputValidators == nil {
		sess.outputValidators = map[string]*outputValidator{}
	}
	v := &outputValidator{raw: schema}
	sess.outputValidators[tool] = v
	if len(schema) > maxOutputSchemaBytes {
		v.unusable = true
		return v
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(schema, &root) == nil {
		_, v.declared = root["$schema"]
	}
	compiled, err := compileOutputSchema(schema, jsonschema.Draft2020)
	if err != nil {
		v.unusable = true
		return v
	}
	v.current = compiled
	return v
}

// oldDialect is the schema read as draft-07, compiled the first time a result
// needs it. Nil when it will not compile that way, which leaves the result
// unjudged.
func (v *outputValidator) oldDialect() *jsonschema.Schema {
	if !v.triedOld {
		v.triedOld = true
		v.draft07, _ = compileOutputSchema(v.raw, jsonschema.Draft7)
	}
	return v.draft07
}

// compileOutputSchema compiles a schema as a document of its own, read as the
// given dialect unless it names one. Nothing outside it is ever loaded. The spec
// says a $ref that resolves to a network URI MUST NOT be dereferenced, and the
// library's default loader reads file URLs, which would let a server's schema
// read files off the machine mcpsnoop runs on. A reference to anything but the
// bundled metaschemas therefore fails the compile, and the schema goes unjudged.
//
// format is never asserted. The library asserts it under draft-07 and older
// whatever the compiler is told, and lets a registered format take the place of
// its own, so every format the schema names is registered as one that accepts
// anything. The one it will not let anyone replace is regex, which it checks
// with Go's own expressions rather than the ECMA-262 ones a schema means, so a
// schema that uses it goes unjudged.
func compileOutputSchema(schema json.RawMessage, dialect *jsonschema.Draft) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(dialect)
	c.UseLoader(refuseLoader{})
	for name := range formatNames(doc, map[string]bool{}) {
		if name == "regex" {
			return nil, errors.New("format regex is judged by the regular expressions of the library, not of ECMA-262")
		}
		c.RegisterFormat(&jsonschema.Format{Name: name, Validate: func(any) error { return nil }})
	}
	if err := c.AddResource(outputSchemaResource, doc); err != nil {
		return nil, err
	}
	return c.Compile(outputSchemaResource)
}

// formatNames collects every string a schema gives under a format key. A
// property that happens to be named format holds a schema rather than a string,
// so it adds nothing, and a name registered without being used costs nothing.
func formatNames(v any, names map[string]bool) map[string]bool {
	switch v := v.(type) {
	case map[string]any:
		for key, value := range v {
			if name, ok := value.(string); ok && key == "format" {
				names[name] = true
				continue
			}
			formatNames(value, names)
		}
	case []any:
		for _, value := range v {
			formatNames(value, names)
		}
	}
	return names
}

// refuseLoader loads nothing.
type refuseLoader struct{}

func (refuseLoader) Load(url string) (any, error) {
	return nil, errors.New("mcpsnoop loads nothing an outputSchema refers to, and was asked for " + url)
}

// pointerEscaper spells one JSON Pointer reference token.
var pointerEscaper = strings.NewReplacer("~", "~0", "/", "~1")

// describeFailure names the first place the result breaks the schema and how,
// so a reader goes straight to the field. A failure inside anyOf or oneOf stops
// at the keyword, since a branch's own complaint is about a branch the result
// was never meant to match.
func describeFailure(err error) string {
	var failure *jsonschema.ValidationError
	if !errors.As(err, &failure) {
		return ""
	}
	for len(failure.Causes) > 0 && wrapsCauses(failure.ErrorKind) {
		failure = failure.Causes[0]
	}
	where := "at the root"
	if len(failure.InstanceLocation) > 0 {
		var pointer strings.Builder
		for _, token := range failure.InstanceLocation {
			pointer.WriteByte('/')
			pointer.WriteString(pointerEscaper.Replace(token))
		}
		where = "at " + pointer.String()
	}
	return " " + where + ", " + failure.ErrorKind.LocalizedString(message.NewPrinter(language.English))
}

// refCycle reports whether validation ran into a $ref loop that consumes nothing
// of the result, which says the schema cannot be evaluated, not that the result
// breaks it.
func refCycle(err error) bool {
	var failure *jsonschema.ValidationError
	if !errors.As(err, &failure) {
		return false
	}
	pending := []*jsonschema.ValidationError{failure}
	for len(pending) > 0 {
		e := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if _, cycle := e.ErrorKind.(*kind.RefCycle); cycle {
			return true
		}
		pending = append(pending, e.Causes...)
	}
	return false
}

// wrapsCauses reports whether an error kind only gathers the errors under it,
// as the document root, a $ref and a group of keywords do.
func wrapsCauses(k jsonschema.ErrorKind) bool {
	switch k.(type) {
	case *kind.Schema, *kind.Reference, *kind.Group, *kind.AllOf:
		return true
	}
	return false
}
