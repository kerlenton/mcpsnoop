package proxy

import (
	"bytes"
	"encoding/json"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ohler55/ojg/jp"

	"github.com/kerlenton/mcpsnoop/internal/jsonwire"
)

const redactedValue = "[REDACTED]"

var commonSecretRedactKeys = []string{
	"token",
	"api_key",
	"apikey",
	"password",
	"passwd",
	"secret",
	"authorization",
	"access_token",
	"refresh_token",
	"client_secret",
}

// RedactConfig configures best-effort scrubbing for observed trace copies.
type RedactConfig struct {
	// CommonSecrets enables a built-in preset of common secret field names.
	CommonSecrets bool

	// Keys are JSON object field names whose values should be replaced.
	Keys []string

	// ValuePatterns are regular expressions whose matches inside observed
	// string payloads should be replaced.
	ValuePatterns []string

	// Paths identify JSON values that should be replaced.
	Paths []RedactPath
}

// RedactPath is a validated JSONPath expression used for trace redaction.
type RedactPath struct {
	raw  string
	expr jp.Expr
}

// ParseRedactPath validates path for use as a modifying JSONPath expression.
func ParseRedactPath(path string) (RedactPath, error) {
	path = strings.TrimSpace(path)
	expr, err := jp.ParseString(path)
	if err != nil {
		return RedactPath{}, err
	}
	if _, err := expr.Modify(nil, func(value any) (any, bool) { return value, false }); err != nil {
		return RedactPath{}, err
	}
	return RedactPath{raw: path, expr: expr}, nil
}

func (p RedactPath) String() string { return p.raw }

// Enabled reports whether cfg has any redaction rule.
func (cfg RedactConfig) Enabled() bool {
	if cfg.CommonSecrets {
		return true
	}
	for _, key := range cfg.Keys {
		if strings.TrimSpace(key) != "" {
			return true
		}
	}
	for _, pattern := range cfg.ValuePatterns {
		if strings.TrimSpace(pattern) != "" {
			return true
		}
	}
	return len(cfg.Paths) > 0
}

// Redactor redacts JSON payloads according to a prepared config.
type Redactor struct {
	keys          map[string]struct{}
	valuePatterns []*regexp.Regexp
	paths         []RedactPath
}

// NewRedactor prepares cfg for repeated use.
func NewRedactor(cfg RedactConfig) Redactor {
	keys := make(map[string]struct{})
	if cfg.CommonSecrets {
		addRedactKeys(keys, commonSecretRedactKeys)
	}
	addRedactKeys(keys, cfg.Keys)
	return Redactor{
		keys:          keys,
		valuePatterns: compileRedactPatterns(cfg.ValuePatterns),
		paths:         cfg.Paths,
	}
}

func addRedactKeys(keys map[string]struct{}, candidates []string) {
	for _, key := range candidates {
		key = strings.ToLower(strings.TrimSpace(key))
		if key != "" {
			keys[key] = struct{}{}
		}
	}
}

func compileRedactPatterns(candidates []string) []*regexp.Regexp {
	var patterns []*regexp.Regexp
	for _, pattern := range candidates {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if re, err := regexp.Compile(pattern); err == nil {
			patterns = append(patterns, re)
		}
	}
	return patterns
}

func (r Redactor) enabled() bool {
	return len(r.keys) > 0 || len(r.valuePatterns) > 0 || len(r.paths) > 0
}

// RedactEnvelope returns a copy of env with matching JSON Raw fields scrubbed.
func (r Redactor) RedactEnvelope(env Envelope) Envelope {
	if !r.enabled() {
		return env
	}
	// The two halves of the link between a scrubbed body value and the header
	// that mirrors it. A rule addresses the body and has no expression for a
	// header, so matching the value is the only link there is; see mirrorsScrubbed
	// for why the surviving half is needed to keep that link honest. Both are
	// collected for every rule, not only for paths, because a key rule reaches a
	// mirrored header no more directly than a path does.
	var scrubbed, survived map[string]struct{}
	if len(env.MCPParamHeaders) > 0 || env.MCPName != "" {
		scrubbed = make(map[string]struct{})
		survived = make(map[string]struct{})
	}
	changed := false
	if len(env.Raw) > 0 {
		if redacted, ok := r.redactRaw(env.Raw, scrubbed, survived); ok {
			env.Raw = redacted
			changed = true
		}
	}
	if env.Text != "" && len(r.valuePatterns) > 0 {
		if redacted := r.redactString(env.Text); redacted != env.Text {
			env.Text = redacted
			changed = true
		}
	}
	// Gated on redaction being on at all, not on keys or values specifically. A
	// path rule scrubs the body and used to leave the header untouched, which
	// leaked the secret and made the store report the pair as disagreeing.
	if len(env.MCPParamHeaders) > 0 {
		headers, headersChanged := r.redactMCPParamHeaders(env.MCPParamHeaders, scrubbed, survived)
		env.MCPParamHeaders = headers
		changed = changed || headersChanged
	}
	// Mcp-Name mirrors params.name for a tool or prompt and params.uri for a
	// resource, which is exactly the shape a user reaches for --redact-path over,
	// since a resource URI can be a filesystem path. Leaving it alone kept the
	// value in the log after the body had lost it.
	//
	// Mcp-Method and MCP-Protocol-Version are deliberately not scrubbed. They
	// carry protocol constants rather than anything of the user's, and both feed
	// gates that decide which revision's rules a request is judged by, so
	// replacing them would quietly switch off checks to hide a value that was
	// never sensitive. Their comparisons are gated in the store instead.
	if env.MCPName != "" && mirrorsScrubbed(env.MCPName, scrubbed, survived) {
		env.MCPName = redactedValue
		changed = true
	}
	// Recorded on the frame so the store can tell mcpsnoop's own placeholder from
	// a client that sent those bytes itself.
	env.Redacted = env.Redacted || changed
	return env
}

// redactMCPParamHeaders scrubs a header three ways. By the value a body rule
// already removed, which is the only way a rule addressing the body can reach a
// header at all; by the annotation name, for a key rule; and by the value
// patterns.
//
// The name a key rule matches here is the x-mcp-header annotation, while the
// body side matches the JSON property name, and the two are independent by
// design. The spec's own example pairs property "region" with header "Region",
// so a rule naming one does not name the other. Both spellings are tried for
// that reason, and the scrubbed-value set covers the pairs neither spelling hits.
func (r Redactor) redactMCPParamHeaders(headers []MCPParamHeader, scrubbed, survived map[string]struct{}) ([]MCPParamHeader, bool) {
	out := slices.Clone(headers)
	changed := false
	for i := range out {
		if mirrorsScrubbed(out[i].Value, scrubbed, survived) {
			out[i].Value, out[i].Redacted = redactedValue, true
			changed = true
			continue
		}
		name := strings.ToLower(out[i].Name)
		name, ok := strings.CutPrefix(name, "mcp-param-")
		_, exactKey := r.keys[name]
		_, normalizedKey := r.keys[strings.ReplaceAll(name, "-", "_")]
		if ok && (exactKey || normalizedKey) {
			out[i].Value, out[i].Redacted = redactedValue, true
			changed = true
			continue
		}
		if scrubbedValue := r.redactString(out[i].Value); scrubbedValue != out[i].Value {
			out[i].Value, out[i].Redacted = scrubbedValue, true
			changed = true
		}
	}
	return out, changed
}

// mirrorsScrubbed reports whether a header carries a value a body rule removed
// and nothing else in the redacted body still spells.
//
// The second half is what keeps the link honest. Two arguments routinely share a
// spelling, which is the normal case for booleans and small integers, and
// scrubbing every header that matches would wipe headers bound to properties no
// rule named, hiding a real disagreement on those. When the spelling is still in
// the stored body in clear, removing it from the header protects nothing and
// only costs a check, so the two cases are exactly the ones to separate.
//
// The header is decoded first, because a value outside visible ASCII may only
// travel wrapped in the Base64 sentinel while the body holds it plain.
//
// What this deliberately does not do. The match is on the spelling alone, over
// the whole frame, with no idea which property a header is bound to, because the
// binding lives in the tool's inputSchema and the shim has never seen it. So a
// header whose value happens to equal something an unrelated rule removed is
// scrubbed too, which can erase the evidence of a genuine header-versus-body
// disagreement. That is the deliberate trade. A secret left in a file the user
// was told is scrubbed is worse than a check lost on one frame. Making it exact
// means carrying the redacted argument paths on the envelope and pairing them
// with the bindings in the store, which is tracked separately.
func mirrorsScrubbed(value string, scrubbed, survived map[string]struct{}) bool {
	decoded, ok := DecodeHeaderValue(value)
	if !ok {
		return false
	}
	if _, removed := scrubbed[decoded]; !removed {
		return false
	}
	_, kept := survived[decoded]
	return !kept
}

// redactRaw rewrites a frame's payload. scrubbed, when non-nil, records the
// scalar values the rules removed and survived records which of those spellings
// are still somewhere in the rewritten body, so the caller can decide whether
// scrubbing a header that mirrors one of them buys anything.
func (r Redactor) redactRaw(raw json.RawMessage, scrubbed, survived map[string]struct{}) (json.RawMessage, bool) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	changed := r.redactPaths(&v, scrubbed)
	if r.redactValue(v, scrubbed) {
		changed = true
	}
	if !changed {
		return nil, false
	}
	if len(scrubbed) > 0 {
		collectSurviving(v, scrubbed, survived)
	}
	// Not json.Marshal: it would escape <, > and & and hand back a payload the
	// server never sent. The sink no longer re-escapes either, so this survives
	// all the way to the file.
	b, err := jsonwire.Marshal(v)
	if err != nil {
		return nil, false
	}
	return b, true
}

// redactPaths rewrites every JSONPath match to the placeholder. scrubbed collects
// the original scalar spellings it removed, because an Mcp-Param header mirrors
// an argument value and a JSONPath has no expression for a header. Without that
// list the body is scrubbed while its mirror in the header keeps the secret, and
// the store then reports the pair as disagreeing on traffic that was correct.
func (r Redactor) redactPaths(value *any, scrubbed map[string]struct{}) bool {
	changed := false
	for _, path := range r.paths {
		matched := false
		// Held aside until the rewrite is adopted below. Modify can still fail after
		// the callback ran, and recording straight into scrubbed would then claim a
		// value was removed from a body that still holds it, which scrubs the header
		// mirroring it for nothing.
		removed := make(map[string]struct{}, 1)
		modified, err := path.expr.Modify(*value, func(old any) (any, bool) {
			matched = true
			recordSpellings(old, removed)
			return redactedValue, true
		})
		// Only adopt the rewritten tree when the path actually hit something, so a
		// non-matching path leaves the original decoded value (and its exact
		// numbers) in place instead of a round-tripped copy.
		if err != nil || !matched {
			continue
		}
		for spelling := range removed {
			if scrubbed != nil {
				scrubbed[spelling] = struct{}{}
			}
		}
		*value = modified
		changed = true
	}
	return changed
}

// maxNumericSpelling bounds how far a number is expanded before being compared.
// 2^53-1 is sixteen digits, so anything near this is already far outside the
// range an x-mcp-header integer may carry.
const maxNumericSpelling = 512

// recordSpellings adds every way a JSON value could appear as a header value to
// set, and does nothing for a nil set.
//
// It descends into objects and arrays. A rule that removes a whole object
// removes every scalar under it, and a binding may sit on a nested property, so
// recording only the top value left the header mirroring an inner one still
// holding the secret.
//
// A number contributes two spellings when they differ, the one the wire used and
// its plain integer form. The spec has servers compare an x-mcp-header integer
// numerically, so a body of 4.2e1 and a header of 42 are the same value, and
// matching only the wire spelling would leave that header holding a number the
// body no longer shows.
func recordSpellings(v any, set map[string]struct{}) {
	if set == nil {
		return
	}
	switch t := v.(type) {
	case string:
		set[t] = struct{}{}
	case bool:
		set[strconv.FormatBool(t)] = struct{}{}
	case json.Number:
		set[t.String()] = struct{}{}
		if normalized, ok := normalizedInteger(t.String()); ok {
			set[normalized] = struct{}{}
		}
	case map[string]any:
		for _, child := range t {
			recordSpellings(child, set)
		}
	case []any:
		for _, child := range t {
			recordSpellings(child, set)
		}
	}
}

// normalizedInteger is the plain integer spelling of a number the wire wrote
// with a fraction or an exponent. A spelling that is already plain needs no
// answer, which keeps big.Rat off the common path entirely.
//
// The magnitude is bounded before expanding. 1e1000000 is nine bytes on the wire
// and a million digits once big.Rat has it, and this runs inline on the proxy
// path where a stall delays the traffic mcpsnoop is supposed to be observing
// rather than merely slowing mcpsnoop down. Nothing refused here could be a
// legal x-mcp-header integer anyway.
func normalizedInteger(spelling string) (string, bool) {
	if !strings.ContainsAny(spelling, ".eE") {
		return "", false
	}
	if len(spelling) > maxNumericSpelling || !numericExponentFits(spelling) {
		return "", false
	}
	exact, ok := new(big.Rat).SetString(spelling)
	if !ok || !exact.IsInt() {
		return "", false
	}
	return exact.Num().String(), true
}

func numericExponentFits(spelling string) bool {
	_, exponent, found := strings.Cut(spelling, "e")
	if !found {
		_, exponent, found = strings.Cut(spelling, "E")
	}
	if !found {
		return true
	}
	// Bounded before parsing, so a thousand-digit exponent cannot overflow the
	// int it would be parsed into.
	if len(exponent) > 6 {
		return false
	}
	value, err := strconv.Atoi(exponent)
	if err != nil {
		return false
	}
	return value <= maxNumericSpelling && value >= -maxNumericSpelling
}

// collectSurviving records which of the scrubbed spellings the rewritten body
// still contains. Only those are of interest, so a large payload cannot grow the
// set beyond what was removed.
func collectSurviving(v any, scrubbed, survived map[string]struct{}) {
	switch x := v.(type) {
	case map[string]any:
		for _, child := range x {
			collectSurviving(child, scrubbed, survived)
		}
	case []any:
		for _, child := range x {
			collectSurviving(child, scrubbed, survived)
		}
	default:
		spellings := make(map[string]struct{}, 2)
		recordSpellings(v, spellings)
		for spelling := range spellings {
			if _, removed := scrubbed[spelling]; removed {
				survived[spelling] = struct{}{}
			}
		}
	}
}

// redactPosition is where in an MCP message the walk currently stands. It exists
// so the schema exemption below can be scoped to the one position a tool schema
// actually occupies. Keying it on the name alone exempted any key called
// inputSchema anywhere, including a tools/call argument of that name, which left
// a secret in a log the user was told was scrubbed.
type redactPosition int

const (
	positionData redactPosition = iota
	positionResult
	positionResultTools
	positionToolDefinition
	positionSchema
)

// instanceKeywords hold example or default data rather than subschemas. A value
// inside one is the server's data, so the schema exemption stops there and the
// ordinary rules apply again.
var instanceKeywords = map[string]struct{}{
	"default":  {},
	"const":    {},
	"examples": {},
	"enum":     {},
}

// parsedSchemaKeywords are the schema fields mcpsnoop itself reads. Rewriting one
// does not hide anything of the user's, since both hold protocol identifiers, and
// it makes the store report the server for the user's own privacy setting. They
// are exempt from every rule except a path, which names them outright.
var parsedSchemaKeywords = map[string]struct{}{
	"x-mcp-header": {},
	"type":         {},
}

// next is the position a child key leads to. A tool schema is reached only as
// result.tools[].inputSchema or .outputSchema, and once inside, an instance
// keyword drops back to ordinary data.
func (p redactPosition) next(key string) redactPosition {
	switch {
	case p == positionData && key == "result":
		return positionResult
	case p == positionResult && key == "tools":
		return positionResultTools
	case p == positionToolDefinition && (key == "inputSchema" || key == "outputSchema"):
		return positionSchema
	case p == positionSchema:
		if _, instance := instanceKeywords[key]; instance {
			return positionData
		}
		return positionSchema
	default:
		return positionData
	}
}

// A tool schema is structure rather than data. The names under its "properties"
// are type declarations, so scrubbing the subschema under a property called
// "token" hides what the argument is while leaving the name itself in plain
// sight, which protects nothing and costs every check that reads the schema,
// x-mcp-header validation most of all. A user who does mean to scrub something
// inside a schema still has --redact-path, which names it exactly.
func (p redactPosition) exemptFromKeyRules() bool { return p == positionSchema }

func (r Redactor) redactValue(v any, scrubbed map[string]struct{}) bool {
	return r.redactValueIn(v, scrubbed, positionData)
}

func (r Redactor) redactValueIn(v any, scrubbed map[string]struct{}, at redactPosition) bool {
	switch x := v.(type) {
	case map[string]any:
		changed := false
		for key, child := range x {
			into := at.next(key)
			if _, ok := r.keys[strings.ToLower(key)]; ok && !into.exemptFromKeyRules() {
				// Recorded for the same reason a path match is. A key rule reaches a
				// mirrored Mcp-Param header no more directly than a path does, and
				// leaving it unrecorded left --redact-key and --redact-secrets scrubbing
				// the body while the header kept the secret.
				recordSpellings(child, scrubbed)
				x[key] = redactedValue
				changed = true
				continue
			}
			if s, ok := child.(string); ok {
				_, parsed := parsedSchemaKeywords[key]
				redacted := s
				if !(at == positionSchema && parsed) {
					redacted = r.redactString(s)
				}
				if redacted != s {
					// Recorded like a key match. A value pattern is written against the
					// plaintext, so it cannot match the Base64 sentinel a header carries
					// the same value in, and without this the body was scrubbed while the
					// encoded mirror decoded straight back to the secret.
					recordSpellings(s, scrubbed)
					x[key] = redacted
					changed = true
				}
				continue
			}
			if r.redactValueIn(child, scrubbed, into) {
				changed = true
			}
		}
		return changed
	case []any:
		changed := false
		// An element of result.tools is a tool definition, which is the only place
		// a tool schema hangs off. Every other array keeps the position it was in.
		elementAt := at
		if at == positionResultTools {
			elementAt = positionToolDefinition
		}
		for i := 0; i < len(x); i++ {
			s, ok := x[i].(string)
			if !ok {
				if r.redactValueIn(x[i], scrubbed, elementAt) {
					changed = true
				}
				continue
			}
			// Best-effort argv redaction so a wrapped server started as
			// `npx server --api-key=sk-x` does not write the secret in clear text.
			// The "--flag=value" form redacts the value and keeps the flag.
			if flag, _, found := strings.Cut(s, "="); found && r.argvFlagKey(flag) {
				x[i] = flag + "=" + redactedValue
				changed = true
				continue
			}
			// The "--flag" form with its value in the next element redacts that one.
			if r.argvFlagKey(s) && i+1 < len(x) {
				if _, isStr := x[i+1].(string); isStr {
					x[i+1] = redactedValue
					changed = true
					i++ // the value element is consumed
					continue
				}
			}
			if redacted := r.redactString(s); redacted != s {
				x[i] = redacted
				changed = true
			}
		}
		return changed
	default:
		return false
	}
}

// argvFlagKey reports whether arg is a command-line flag whose name is a redact
// key. It only matches dashed tokens (so plain array strings are left to value
// patterns), and normalizes the name the way object keys are, stripping the
// leading dashes, turning dashes into underscores, and lowercasing, so `--api-key`
// hits the api_key entry. It is best effort, an argument without a recognizable
// flag name cannot be detected.
func (r Redactor) argvFlagKey(arg string) bool {
	if !strings.HasPrefix(arg, "-") {
		return false
	}
	name := strings.ToLower(strings.ReplaceAll(strings.TrimLeft(arg, "-"), "-", "_"))
	if name == "" {
		return false
	}
	_, ok := r.keys[name]
	return ok
}

func (r Redactor) redactString(s string) string {
	for _, re := range r.valuePatterns {
		s = re.ReplaceAllString(s, redactedValue)
	}
	return s
}

type redactingSink struct {
	next     Sink
	redactor Redactor
}

// NewRedactingSink wraps next and scrubs envelopes before forwarding them.
func NewRedactingSink(next Sink, cfg RedactConfig) Sink {
	if next == nil {
		next = NopSink()
	}
	redactor := NewRedactor(cfg)
	if !redactor.enabled() {
		return next
	}
	return &redactingSink{next: next, redactor: redactor}
}

func (s *redactingSink) Emit(env Envelope) {
	s.next.Emit(s.redactor.RedactEnvelope(env))
}

func (s *redactingSink) Close() error { return s.next.Close() }

// Dropped forwards the wrapped sink's drop count, so redaction does not hide it.
func (s *redactingSink) Dropped() uint64 {
	if d, ok := s.next.(DropCounter); ok {
		return d.Dropped()
	}
	return 0
}
