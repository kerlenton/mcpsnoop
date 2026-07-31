package proxy

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
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
	// Values a JSONPath rule removed from the body, so the same value can be
	// removed from a header that mirrors it.
	var pathReplaced map[string]struct{}
	if len(r.paths) > 0 && len(env.MCPParamHeaders) > 0 {
		pathReplaced = make(map[string]struct{}, len(r.paths))
	}
	if len(env.Raw) > 0 {
		if redacted, ok := r.redactRaw(env.Raw, pathReplaced); ok {
			env.Raw = redacted
		}
	}
	if env.Text != "" && len(r.valuePatterns) > 0 {
		env.Text = r.redactString(env.Text)
	}
	// Gated on redaction being on at all, not on keys or values specifically. A
	// path rule scrubs the body and used to leave the header untouched, which
	// leaked the secret and made the store report the pair as disagreeing.
	if len(env.MCPParamHeaders) > 0 {
		env.MCPParamHeaders = r.redactMCPParamHeaders(env.MCPParamHeaders, pathReplaced)
	}
	return env
}

// redactMCPParamHeaders scrubs a header three ways. By the annotation name, for
// a key rule; by the value a path rule already removed from the body, which is
// the only way a JSONPath can reach a header at all; and by the value patterns.
//
// The name a key rule matches here is the x-mcp-header annotation, while the
// body side matches the JSON property name, and the two are independent by
// design. The spec's own example pairs property "region" with header "Region",
// so a rule naming one does not name the other. Both spellings are tried for
// that reason, and the path-replaced set covers the pairs neither spelling hits.
func (r Redactor) redactMCPParamHeaders(headers []MCPParamHeader, pathReplaced map[string]struct{}) []MCPParamHeader {
	redacted := slices.Clone(headers)
	for i := range redacted {
		if _, mirrored := pathReplaced[redacted[i].Value]; mirrored {
			redacted[i].Value = redactedValue
			continue
		}
		name := strings.ToLower(redacted[i].Name)
		name, ok := strings.CutPrefix(name, "mcp-param-")
		_, exactKey := r.keys[name]
		_, normalizedKey := r.keys[strings.ReplaceAll(name, "-", "_")]
		if ok && (exactKey || normalizedKey) {
			redacted[i].Value = redactedValue
			continue
		}
		redacted[i].Value = r.redactString(redacted[i].Value)
	}
	return redacted
}

// redactRaw rewrites a frame's payload. replaced, when non-nil, records the
// scalar values a JSONPath rule removed, so the caller can scrub the same value
// out of an Mcp-Param header that mirrors it. A JSONPath addresses the body and
// has no expression for a header, so that list is the only link between them.
func (r Redactor) redactRaw(raw json.RawMessage, replaced map[string]struct{}) (json.RawMessage, bool) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	changed := r.redactPaths(&v, replaced)
	if r.redactValue(v) {
		changed = true
	}
	if !changed {
		return nil, false
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

// redactPaths rewrites every JSONPath match to the placeholder. replaced collects
// the original scalar spellings it removed, because an Mcp-Param header mirrors
// an argument value and a JSONPath has no expression for a header. Without that
// list the body is scrubbed while its mirror in the header keeps the secret, and
// the store then reports the pair as disagreeing on traffic that was correct.
func (r Redactor) redactPaths(value *any, replaced map[string]struct{}) bool {
	changed := false
	for _, path := range r.paths {
		matched := false
		modified, err := path.expr.Modify(*value, func(old any) (any, bool) {
			matched = true
			if replaced != nil {
				if s, ok := scalarSpelling(old); ok {
					replaced[s] = struct{}{}
				}
			}
			return redactedValue, true
		})
		// Only adopt the rewritten tree when the path actually hit something, so a
		// non-matching path leaves the original decoded value (and its exact
		// numbers) in place instead of a round-tripped copy.
		if err != nil || !matched {
			continue
		}
		*value = modified
		changed = true
	}
	return changed
}

// scalarSpelling is how a JSON scalar would appear as a header value, or false
// when the value is not a scalar. json.Number keeps the spelling the wire used,
// which is what the header carries.
func scalarSpelling(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	case bool:
		if t {
			return "true", true
		}
		return "false", true
	}
	return "", false
}

func (r Redactor) redactValue(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		changed := false
		for key, child := range x {
			if _, ok := r.keys[strings.ToLower(key)]; ok {
				x[key] = redactedValue
				changed = true
				continue
			}
			if s, ok := child.(string); ok {
				redacted := r.redactString(s)
				if redacted != s {
					x[key] = redacted
					changed = true
				}
				continue
			}
			if r.redactValue(child) {
				changed = true
			}
		}
		return changed
	case []any:
		changed := false
		for i := 0; i < len(x); i++ {
			s, ok := x[i].(string)
			if !ok {
				if r.redactValue(x[i]) {
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
