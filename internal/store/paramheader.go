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

// paramHeaderType is the type a property declares and whether it declares one at
// all. The two answers are separate because the spec constrains x-mcp-header to
// "parameters with primitive types" and says nothing about a schema that names
// no type. An undeclared type is a constraint mcpsnoop cannot evaluate, not a
// violation to report, so a property whose type is expressed only through enum
// or const must not be accused of one.
//
// A union is read by dropping "null". The spec's own table has a row for a
// parameter whose value is null, telling the client to omit the header, so a
// nullable parameter is a legal place for the annotation, and ["string","null"]
// is what a generated schema spells that as.
func paramHeaderType(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return single, true
	}
	var union []string
	if json.Unmarshal(raw, &union) != nil {
		return "", false
	}
	named := ""
	for _, member := range union {
		if member == "null" {
			continue
		}
		if named != "" {
			return "", false
		}
		named = member
	}
	return named, named != ""
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

// unverifiableAfterRedaction reports whether a header-versus-body comparison
// cannot be judged because mcpsnoop's own redaction scrubbed one side of it.
// Reporting that as a disagreement invents a protocol violation out of the
// user's privacy setting, on a signal that fails a default check run.
//
// Gated on the frame having actually been rewritten, because "[REDACTED]" is a
// value either peer may send and a check that stops at those bytes alone is a
// check the traffic can switch off.
func unverifiableAfterRedaction(redacted bool, values ...string) bool {
	return redacted && slices.Contains(values, redactedMarker)
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
		raw, exists, ancestorScrubbed := lookupParamArgument(params.Arguments, binding.path)
		path := strings.Join(binding.path, ".")
		if !exists || string(raw) == "null" {
			// An ancestor mcpsnoop replaced with the placeholder hides the parameter
			// from the walk. Reporting that as the client omitting a value invents a
			// violation out of the user's privacy setting, on a signal that fails a
			// default check run.
			if present && !(redacted && ancestorScrubbed) {
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
	// The static-reachability rule, checked after the walk because the walk is
	// what defines reachable. Anything the walk could not arrive at is an
	// annotation the spec says makes the whole definition invalid.
	if note := unreachableAnnotation(schema); note != "" {
		return nil, note
	}
	slices.SortFunc(bindings, func(a, b paramHeaderBinding) int {
		if n := strings.Compare(strings.ToLower(a.header), strings.ToLower(b.header)); n != 0 {
			return n
		}
		return strings.Compare(strings.Join(a.path, "."), strings.Join(b.path, "."))
	})
	return bindings, ""
}

// unreachableAnnotation looks for an x-mcp-header the binding walk cannot arrive
// at, and names the keyword that put it out of reach. The spec allows the
// annotation only on a property reachable through a chain consisting solely of
// "properties" keys, and says of anything else that it "makes the annotation, and
// thus the tool definition, invalid".
//
// Written as a scan for the unreachable rather than a list of forbidden
// keywords. items, oneOf, anyOf, allOf, not, if, then, else and the $defs a $ref
// points into all fall out of the one rule, and so does any keyword a later
// revision adds, which a hand-kept list would silently miss.
//
// The keys inside a "properties" object are parameter names, not schema
// keywords, so they are stepped over rather than read. Without that, a tool with
// an argument genuinely named x-mcp-header would be reported for an annotation it
// does not have.
func unreachableAnnotation(schema json.RawMessage) string {
	var document any
	if json.Unmarshal(schema, &document) != nil {
		return ""
	}
	return scanForUnreachable(document, "")
}

func scanForUnreachable(node any, under string) string {
	switch value := node.(type) {
	case map[string]any:
		if under != "" {
			if _, annotated := value["x-mcp-header"]; annotated {
				return "an x-mcp-header under " + strconv.Quote(under) +
					", which 2026-07-28 does not allow in the chain to an annotated property"
			}
		}
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			if key == "properties" {
				// One step further along the reachable chain. Its keys are names, so
				// descend past them into each subschema keeping the current reach.
				if names, ok := value[key].(map[string]any); ok {
					childKeys := make([]string, 0, len(names))
					for name := range names {
						childKeys = append(childKeys, name)
					}
					slices.Sort(childKeys)
					for _, name := range childKeys {
						if note := scanForUnreachable(names[name], under); note != "" {
							return note
						}
					}
					continue
				}
			}
			// Any other keyword leaves the chain, and once left it is never rejoined.
			// The annotation's own key needs no special case, since a reachable one
			// is not a violation and its value is a scalar the walk bottoms out on.
			reach := under
			if reach == "" {
				reach = key
			}
			if note := scanForUnreachable(value[key], reach); note != "" {
				return note
			}
		}
	case []any:
		for _, item := range value {
			if note := scanForUnreachable(item, under); note != "" {
				return note
			}
		}
	}
	return ""
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
		// An explicit null is an absent annotation, not an empty one. A serializer
		// that emits every unset optional field, which Jackson without NON_NULL and
		// a plain dataclass dump both do, would otherwise take the whole tool down
		// over a property that carries no annotation at all.
		if len(property.Header) != 0 && string(property.Header) != "null" {
			binding, bind, violation := paramHeaderAnnotation(property, path, seen)
			if violation != "" {
				return nil, violation
			}
			if bind {
				out = append(out, binding)
			}
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
//
// The three returns are binding, whether to bind it, and the violation. A
// property can be all three ways round: legal and bound, illegal and reported,
// or legal as far as the spec is concerned while declaring a type mcpsnoop
// cannot read, which is bound to nothing and reported as nothing.
func paramHeaderAnnotation(property paramHeaderSchema, path []string, seen map[string]struct{}) (paramHeaderBinding, bool, string) {
	where := " on property " + strconv.Quote(strings.Join(path, "."))
	var header string
	if json.Unmarshal(property.Header, &header) != nil {
		return paramHeaderBinding{}, false, "an x-mcp-header" + where + " that is not a string"
	}
	if header == "" {
		return paramHeaderBinding{}, false, "an empty x-mcp-header" + where
	}
	if !validMCPParamHeaderName(header) {
		return paramHeaderBinding{}, false, "x-mcp-header " + strconv.Quote(header) + where +
			", which is not a valid header field name"
	}
	// Recorded before the walk can drop this property for an unjudgeable type, so
	// a duplicate hiding behind one is still caught. The rule is about the
	// annotation values in an inputSchema and does not depend on what they sit on.
	key := strings.ToLower(header)
	if _, duplicate := seen[key]; duplicate {
		return paramHeaderBinding{}, false, "x-mcp-header " + strconv.Quote(header) + " on more than one property"
	}
	seen[key] = struct{}{}
	typ, declared := paramHeaderType(property.Type)
	if declared && !isMCPParamPrimitive(typ) {
		return paramHeaderBinding{}, false, "x-mcp-header " + strconv.Quote(header) + where +
			", whose type " + strconv.Quote(typ) + " is not one of string, integer or boolean"
	}
	if !declared {
		// Legal as far as the spec goes, since it constrains the parameter's type
		// rather than the presence of a type keyword. Without one there is nothing
		// to compare a header value against, so the binding is dropped in silence,
		// the same answer an unreadable schema gets.
		return paramHeaderBinding{}, false, ""
	}
	return paramHeaderBinding{path: path, header: header, typ: typ}, true, ""
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

// lookupParamArgument walks a binding's property path through the call
// arguments. scrubbed reports that the walk stopped on mcpsnoop's own
// placeholder rather than on a value the client left out, which the caller needs
// because the two look identical from the leaf and only one of them is the
// client's doing.
func lookupParamArgument(arguments map[string]json.RawMessage, path []string) (value json.RawMessage, found, scrubbed bool) {
	if len(path) == 0 {
		return nil, false, false
	}
	current, ok := arguments[path[0]]
	for _, part := range path[1:] {
		if !ok {
			return nil, false, false
		}
		if string(current) == strconv.Quote(redactedMarker) {
			return nil, false, true
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(current, &object) != nil {
			return nil, false, false
		}
		current, ok = object[part]
	}
	return current, ok, false
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
	if !decimalMCPNumber(value) {
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

// maxMCPNumberDigits bounds how far a spelling is expanded. 2^53-1 is sixteen
// digits, so this is already far past anything an x-mcp-header integer may hold.
const maxMCPNumberDigits = 512

// decimalMCPNumber reports whether value is a decimal number worth handing to
// big.Rat, and is the guard in front of it.
//
// It does two jobs. It fixes the grammar to the decimal forms the spec asks for,
// which keeps hex, octal, binary, underscore separators and big.Rat's fraction
// form out with one consistent rule rather than a character scan that let 0o52
// through while refusing 0x2A. Within decimal it stays permissive on purpose,
// since the spec tells servers to compare numerically, so 42.0, 042, +42 and
// 4.2e1 all keep matching a body of 42 and none of them warns on correct
// traffic.
//
// And it refuses a magnitude big.Rat would expand into megabytes. 1e1000000 is
// nine bytes on the wire and a million digits once expanded, and this runs under
// the store's lock, so the expansion has to be refused rather than survived.
// Anything refused that way is far outside the safe range, which is the answer
// the exact parse would have reached anyway.
func decimalMCPNumber(value string) bool {
	i := 0
	if i < len(value) && (value[i] == '+' || value[i] == '-') {
		i++
	}
	integerDigits := countDigits(value, &i)
	fractionDigits := 0
	if i < len(value) && value[i] == '.' {
		i++
		fractionDigits = countDigits(value, &i)
	}
	if integerDigits == 0 && fractionDigits == 0 {
		return false
	}
	exponent := 0
	if i < len(value) && (value[i] == 'e' || value[i] == 'E') {
		i++
		sign := 1
		if i < len(value) && (value[i] == '+' || value[i] == '-') {
			if value[i] == '-' {
				sign = -1
			}
			i++
		}
		start := i
		for i < len(value) && value[i] >= '0' && value[i] <= '9' {
			// Capped as it accumulates, so a thousand-digit exponent cannot overflow.
			if exponent <= maxMCPNumberDigits {
				exponent = exponent*10 + int(value[i]-'0')
			}
			i++
		}
		if i == start {
			return false
		}
		exponent *= sign
	}
	if i != len(value) {
		return false
	}
	return integerDigits+fractionDigits <= maxMCPNumberDigits &&
		integerDigits+exponent <= maxMCPNumberDigits &&
		exponent >= -maxMCPNumberDigits
}

func countDigits(value string, i *int) int {
	start := *i
	for *i < len(value) && value[*i] >= '0' && value[*i] <= '9' {
		*i++
	}
	return *i - start
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
