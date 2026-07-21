package store

import "strings"

// deprecatedMethodNote returns a heads-up when method belongs to a feature
// deprecated in the 2026-07-28 MCP release (SEP-2577). It is a structured
// marker, not a protocol warning, so it never fails check.
func deprecatedMethodNote(method string) string {
	switch {
	case strings.HasPrefix(method, "roots/"):
		return "roots is deprecated (2026-07-28); use tool parameters, resource URIs, or server configuration instead"
	case strings.HasPrefix(method, "notifications/roots/"):
		return "roots is deprecated (2026-07-28); use tool parameters, resource URIs, or server configuration instead"
	case strings.HasPrefix(method, "sampling/"):
		return "sampling is deprecated (2026-07-28); integrate directly with an LLM provider API instead"
	case strings.HasPrefix(method, "logging/"):
		return "logging is deprecated (2026-07-28); use stderr for stdio or OpenTelemetry instead"
	case method == "notifications/message":
		return "logging notifications/message is deprecated (2026-07-28); use stderr for stdio or OpenTelemetry instead"
	default:
		return ""
	}
}

// DeprecatedCapabilityNote names the migration path when a side still advertises a
// capability deprecated in the 2026-07-28 MCP release.
func DeprecatedCapabilityNote(name string) string {
	switch name {
	case "roots":
		return "use tool parameters, resource URIs, or server configuration"
	case "sampling":
		return "integrate directly with an LLM provider API"
	case "logging":
		return "use stderr for stdio or OpenTelemetry"
	default:
		return ""
	}
}

// deprecatedNestedNote reports the deprecated features named by the
// server-to-client requests inside an InputRequiredResult. Since the 2026-07-28
// revision removed server-initiated requests, MRTR is the only way a server can
// still reach for sampling or roots, and there the method sits inside the
// inputRequests map. Each feature is named once however many requests use it,
// so a result asking for two samplings does not say so twice.
func deprecatedNestedNote(methods []string) string {
	var notes []string
	seen := make(map[string]bool, len(methods))
	for _, m := range methods {
		note := deprecatedMethodNote(m)
		if note == "" || seen[note] {
			continue
		}
		seen[note] = true
		notes = append(notes, note)
	}
	return strings.Join(notes, "; ")
}
