package store

import (
	"encoding/json"
	"math/big"
	"slices"
	"strconv"
	"strings"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

const (
	mcpParamHeaderPrefix = "Mcp-Param-"
	maxSafeMCPInteger    = int64(1<<53 - 1)
	// redactedMarker is what proxy.Redactor writes in place of a scrubbed value.
	// Duplicated rather than imported because the store must not depend on the
	// proxy's redaction internals, and a literal that drifts is caught by
	// TestParamHeaderRedactionMarkerMatchesTheProxy.
	redactedMarker = "[REDACTED]"
	// outsideSafeRange names the rule an integer breaks by its magnitude alone.
	// The spec states the range as its own constraint on an x-mcp-header integer,
	// separate from being an integer at all, and reporting 2^53 as "not a valid
	// integer" sends the reader after a type error that is not there.
	outsideSafeRange = "is outside the safe integer range x-mcp-header requires"
)

type paramHeaderSchema struct {
	// Type is raw because JSON Schema allows both "string" and ["string","null"],
	// and the union form is common in generated tool schemas. Decoding it into a
	// string made encoding/json fail the whole tree, and since the caller then
	// dropped every binding, one union-typed property anywhere in a schema
	// silently switched off header checking for the entire tool.
	Type   json.RawMessage `json:"type"`
	Header json.RawMessage `json:"x-mcp-header"`
	// Properties holds each subschema raw, and each is decoded on its own, because
	// a property value need not be an object. JSON Schema 2020-12 allows the
	// boolean subschemas true and false there, and mcpsnoop's own redaction
	// replaces a subschema under a key like "token" with a string. Decoding the
	// map in one go failed the whole tree on either, which took every binding with
	// it and, once the verdict was reported, accused the server of a violation it
	// had not committed.
	Properties map[string]json.RawMessage `json:"properties"`
}

// paramHeaderType is the single primitive type a property declares, or "" when
// it declares none, several, or something that is not a primitive. The spec
// permits x-mcp-header only on integer, string and boolean, so a union is not a
// legal place for the annotation, but it is a legal thing to appear elsewhere in
// the schema and must not poison the walk.
func paramHeaderType(raw json.RawMessage) string {
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return single
	}
	var union []string
	if json.Unmarshal(raw, &union) == nil && len(union) == 1 {
		return union[0]
	}
	return ""
}

type paramHeaderBinding struct {
	path   []string
	header string
	typ    string
}

// paramHeaderViolation is one tool whose advertised schema breaks an x-mcp-header
// constraint, and which one it breaks.
type paramHeaderViolation struct {
	tool   string
	reason string
}

func sortedMCPParamHeaders(headers []proxy.MCPParamHeader) []proxy.MCPParamHeader {
	out := slices.Clone(headers)
	slices.SortStableFunc(out, func(a, b proxy.MCPParamHeader) int {
		if n := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); n != 0 {
			return n
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

// mcpParamHeaderWarnings compares each bound header against the argument it
// mirrors. redacted says the frame passed through mcpsnoop's own redaction,
// which is what makes the placeholder below trustworthy as a placeholder.
func mcpParamHeaderWarnings(sess *session, msg proxy.RPCMessage, headers []proxy.MCPParamHeader, redacted bool) []string {
	var params struct {
		Name      string                     `json:"name"`
		Arguments map[string]json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(msg.Params, &params) != nil || params.Name == "" {
		return nil
	}
	definition, ok := sess.toolDefinitions[params.Name]
	if !ok {
		return nil
	}
	bindings := definition.paramHeaders
	if len(bindings) == 0 {
		return nil
	}

	// Every spelling is kept, not just the first. A repeated field with conflicting
	// values is a request a conforming server must reject, and keeping only the
	// first made the verdict depend on which line happened to arrive first.
	values := make(map[string][]string, len(headers))
	for _, header := range headers {
		key := strings.ToLower(header.Name)
		values[key] = append(values[key], header.Value)
	}

	var warnings []string
	for _, binding := range bindings {
		fullName := mcpParamHeaderPrefix + binding.header
		spellings, present := values[strings.ToLower(fullName)]
		raw, exists := lookupParamArgument(params.Arguments, binding.path)
		path := strings.Join(binding.path, ".")
		if !exists || string(raw) == "null" {
			if present {
				warnings = append(warnings, "routing header "+fullName+
					" is present but body parameter "+strconv.Quote(path)+" is absent or null")
			}
			continue
		}
		if !present {
			warnings = append(warnings, "required routing header "+fullName+
				" is missing for body parameter "+strconv.Quote(path))
			continue
		}
		// Either side scrubbed by mcpsnoop's own redaction makes the pair
		// unverifiable, not disagreeing. The proxy scrubs the header alongside the
		// body, but the two are matched by value and a rule can still reach only one
		// of them. Reporting that as a mismatch invents a protocol violation out of
		// the user's privacy setting, and it fails a default check run.
		//
		// Gated on the frame actually having been redacted. "[REDACTED]" is a legal
		// header value and a legal string argument, so skipping on those bytes alone
		// let either peer switch the check off by sending them.
		if redacted && (slices.Contains(spellings, redactedMarker) ||
			string(raw) == strconv.Quote(redactedMarker)) {
			continue
		}
		if len(spellings) > 1 && !mcpParamHeaderValuesAgree(spellings, binding.typ) {
			warnings = append(warnings, "routing header "+fullName+
				" is repeated with conflicting values, so body parameter "+
				strconv.Quote(path)+" cannot be checked")
			continue
		}

		decoded, ok := proxy.DecodeHeaderValue(spellings[0])
		if !ok {
			warnings = append(warnings, "routing header "+fullName+" has invalid Base64 encoding")
			continue
		}
		bodyValue, reason := mcpParamPrimitive(raw, binding.typ)
		if reason != "" {
			warnings = append(warnings, "routing header "+fullName+
				" cannot be compared because body parameter "+strconv.Quote(path)+" "+reason)
			continue
		}
		if bodyInteger, isInteger := bodyValue.(int64); isInteger {
			// Compared numerically, as the spec asks, so 42.0 and 42 agree. An integer
			// too large to be one gets the rule it actually breaks rather than being
			// reported as merely disagreeing.
			header := parseMCPInteger(decoded)
			if header.integer && !header.safe {
				warnings = append(warnings, "routing header "+fullName+" "+
					strconv.Quote(decoded)+" "+outsideSafeRange)
				continue
			}
			if header.safe && header.value == bodyInteger {
				continue
			}
		} else if decoded == mcpParamString(bodyValue) {
			continue
		}
		warnings = append(warnings, "routing header "+fullName+" "+strconv.Quote(decoded)+
			" disagrees with body parameter "+strconv.Quote(path)+" "+strconv.Quote(mcpParamString(bodyValue)))
	}
	return warnings
}

// mcpParamHeaderValuesAgree reports whether every spelling of one repeated header
// carries the same value. Decoded first, so the plain and Base64 forms of one
// value agree, and integers compared numerically, so 42 and 42.0 agree, because
// neither is a conflict about what the server was asked to do.
func mcpParamHeaderValuesAgree(spellings []string, typ string) bool {
	first, ok := proxy.DecodeHeaderValue(spellings[0])
	if !ok {
		return false
	}
	firstInteger := parseMCPInteger(first)
	for _, spelling := range spellings[1:] {
		next, ok := proxy.DecodeHeaderValue(spelling)
		if !ok {
			return false
		}
		if typ == "integer" && firstInteger.safe {
			other := parseMCPInteger(next)
			if !other.safe || other.value != firstInteger.value {
				return false
			}
			continue
		}
		if next != first {
			return false
		}
	}
	return true
}

// mcpParamHeaderBindings resolves a tool's x-mcp-header annotations. The second
// return is the violation to report, empty when there is none.
//
// A schema that will not decode returns no bindings and no violation. That is an
// observation limit rather than a server's fault, and the two must not share one
// answer: the spec makes a violating annotation a definition the client MUST
// reject, so saying so is right, while saying it about a schema mcpsnoop merely
// could not read accuses the server of something it did not do, on a signal that
// fails a default check run.
func mcpParamHeaderBindings(schema json.RawMessage) ([]paramHeaderBinding, string) {
	var root paramHeaderSchema
	if json.Unmarshal(schema, &root) != nil {
		return nil, ""
	}
	seen := make(map[string]struct{})
	bindings, violation := collectMCPParamHeaderBindings(root.Properties, nil, seen, nil)
	if violation != "" {
		return nil, violation
	}
	slices.SortFunc(bindings, func(a, b paramHeaderBinding) int {
		if n := strings.Compare(strings.ToLower(a.header), strings.ToLower(b.header)); n != 0 {
			return n
		}
		return strings.Compare(strings.Join(a.path, "."), strings.Join(b.path, "."))
	})
	return bindings, ""
}

func collectMCPParamHeaderBindings(properties map[string]json.RawMessage, prefix []string, seen map[string]struct{}, out []paramHeaderBinding) ([]paramHeaderBinding, string) {
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		var property paramHeaderSchema
		if json.Unmarshal(properties[name], &property) != nil {
			continue
		}
		path := append(slices.Clone(prefix), name)
		if len(property.Header) != 0 {
			binding, violation := paramHeaderAnnotation(property, path, seen)
			if violation != "" {
				return nil, violation
			}
			out = append(out, binding)
		}
		var violation string
		out, violation = collectMCPParamHeaderBindings(property.Properties, path, seen, out)
		if violation != "" {
			return nil, violation
		}
	}
	return out, ""
}

// paramHeaderAnnotation validates one x-mcp-header against the constraints the
// spec puts on it, naming the one it breaks. The name is what turns the report
// into something a reader can act on, since a single message covering every
// rejection reason sends them after the wrong rule.
func paramHeaderAnnotation(property paramHeaderSchema, path []string, seen map[string]struct{}) (paramHeaderBinding, string) {
	where := " on property " + strconv.Quote(strings.Join(path, "."))
	var header string
	if json.Unmarshal(property.Header, &header) != nil {
		return paramHeaderBinding{}, "an x-mcp-header" + where + " that is not a string"
	}
	if header == "" {
		return paramHeaderBinding{}, "an empty x-mcp-header" + where
	}
	if !validMCPParamHeaderName(header) {
		return paramHeaderBinding{}, "x-mcp-header " + strconv.Quote(header) + where +
			", which is not a valid header field name"
	}
	typ := paramHeaderType(property.Type)
	if !isMCPParamPrimitive(typ) {
		return paramHeaderBinding{}, "x-mcp-header " + strconv.Quote(header) + where +
			", whose type is not one of string, integer or boolean"
	}
	key := strings.ToLower(header)
	if _, duplicate := seen[key]; duplicate {
		return paramHeaderBinding{}, "x-mcp-header " + strconv.Quote(header) + " on more than one property"
	}
	seen[key] = struct{}{}
	return paramHeaderBinding{path: path, header: header, typ: typ}, ""
}

// isMCPParamPrimitive reports whether a declared type may carry an
// x-mcp-header. The spec names integer, string and boolean, and excludes number
// explicitly, because a float has no single decimal spelling to compare against.
func isMCPParamPrimitive(typ string) bool {
	return typ == "string" || typ == "integer" || typ == "boolean"
}

func validMCPParamHeaderName(name string) bool {
	for _, c := range name {
		switch {
		case c >= '0' && c <= '9', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
			continue
		}
		switch c {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return name != ""
}

func lookupParamArgument(arguments map[string]json.RawMessage, path []string) (json.RawMessage, bool) {
	if len(path) == 0 {
		return nil, false
	}
	current, ok := arguments[path[0]]
	for _, part := range path[1:] {
		if !ok {
			return nil, false
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(current, &object) != nil {
			return nil, false
		}
		current, ok = object[part]
	}
	return current, ok
}

// mcpParamPrimitive reads a body argument as the type its binding declares. The
// second return is why it could not, empty on success, phrased to follow the
// parameter's name.
func mcpParamPrimitive(raw json.RawMessage, typ string) (any, string) {
	switch typ {
	case "string":
		var value string
		if json.Unmarshal(raw, &value) != nil {
			return nil, "is not a valid string"
		}
		return value, ""
	case "boolean":
		var value bool
		if json.Unmarshal(raw, &value) != nil {
			return nil, "is not a valid boolean"
		}
		return value, ""
	case "integer":
		parsed := parseMCPInteger(string(raw))
		switch {
		case !parsed.integer:
			return nil, "is not a valid integer"
		case !parsed.safe:
			return nil, outsideSafeRange
		}
		return parsed.value, ""
	default:
		return nil, "has a type x-mcp-header does not cover"
	}
}

// mcpInteger is one integer spelling parsed twice over. integer says the value is
// an integer at all, safe says it also fits the range the spec requires of an
// x-mcp-header integer. value is meaningful only when safe.
type mcpInteger struct {
	value   int64
	integer bool
	safe    bool
}

func parseMCPInteger(value string) mcpInteger {
	value = strings.TrimSpace(value)
	// Three spellings big.Rat accepts that no conforming client can produce, the
	// underscore separator, hex-float notation and the fraction form, all NaN under
	// JavaScript's Number(). Everything else stays permissive on purpose: the spec
	// tells servers to compare these numerically, so 42.0, 042 and 4.2e1 must keep
	// matching a body of 42, and refusing them would warn on correct traffic, which
	// is the worse failure of the two.
	if strings.ContainsAny(value, "_xX/") {
		return mcpInteger{}
	}
	exact, ok := new(big.Rat).SetString(value)
	if !ok || !exact.IsInt() {
		return mcpInteger{}
	}
	number := exact.Num()
	if !number.IsInt64() {
		return mcpInteger{integer: true}
	}
	parsed := number.Int64()
	return mcpInteger{
		value:   parsed,
		integer: true,
		safe:    parsed >= -maxSafeMCPInteger && parsed <= maxSafeMCPInteger,
	}
}

func mcpParamString(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case bool:
		return strconv.FormatBool(value)
	case int64:
		return strconv.FormatInt(value, 10)
	default:
		return ""
	}
}
