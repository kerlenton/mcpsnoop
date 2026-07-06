package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
)

const redactedValue = "[REDACTED]"

// RedactConfig configures best-effort scrubbing for observed trace copies.
type RedactConfig struct {
	// Keys are JSON object field names whose values should be replaced.
	Keys []string
}

// Enabled reports whether cfg has any key-based redaction rule.
func (cfg RedactConfig) Enabled() bool {
	for _, key := range cfg.Keys {
		if strings.TrimSpace(key) != "" {
			return true
		}
	}
	return false
}

// Redactor redacts JSON payloads according to a prepared config.
type Redactor struct {
	keys map[string]struct{}
}

// NewRedactor prepares cfg for repeated use.
func NewRedactor(cfg RedactConfig) Redactor {
	keys := make(map[string]struct{})
	for _, key := range cfg.Keys {
		key = strings.ToLower(strings.TrimSpace(key))
		if key != "" {
			keys[key] = struct{}{}
		}
	}
	return Redactor{keys: keys}
}

func (r Redactor) enabled() bool { return len(r.keys) > 0 }

// RedactEnvelope returns a copy of env with matching JSON Raw fields scrubbed.
func (r Redactor) RedactEnvelope(env Envelope) Envelope {
	if !r.enabled() || len(env.Raw) == 0 {
		return env
	}
	redacted, ok := r.redactRaw(env.Raw)
	if !ok {
		return env
	}
	env.Raw = redacted
	return env
}

func (r Redactor) redactRaw(raw json.RawMessage) (json.RawMessage, bool) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if !r.redactValue(v) {
		return nil, false
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	return b, true
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
			if r.redactValue(child) {
				changed = true
			}
		}
		return changed
	case []any:
		changed := false
		for _, child := range x {
			if r.redactValue(child) {
				changed = true
			}
		}
		return changed
	default:
		return false
	}
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
