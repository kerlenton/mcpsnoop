package proxy

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/ohler55/ojg/jp"
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
	if len(env.Raw) > 0 {
		if redacted, ok := r.redactRaw(env.Raw); ok {
			env.Raw = redacted
		}
	}
	if env.Text != "" && len(r.valuePatterns) > 0 {
		env.Text = r.redactString(env.Text)
	}
	return env
}

func (r Redactor) redactRaw(raw json.RawMessage) (json.RawMessage, bool) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	changed := r.redactPaths(&v)
	if r.redactValue(v) {
		changed = true
	}
	if !changed {
		return nil, false
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	return b, true
}

func (r Redactor) redactPaths(value *any) bool {
	changed := false
	for _, path := range r.paths {
		matched := false
		modified, err := path.expr.Modify(*value, func(any) (any, bool) {
			matched = true
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
