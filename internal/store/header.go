package store

import (
	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// The Base64 sentinel a routing header value may be wrapped in. Defined by the
// transport, so the one implementation lives in the proxy and both the shim's
// redaction and the checks here share it. Aliased rather than re-spelled,
// because a second copy of the markers is a second thing to drift.
const (
	base64SentinelPrefix = proxy.Base64SentinelPrefix
	base64SentinelSuffix = proxy.Base64SentinelSuffix
)

// decodeHeaderValue is the Mcp-Name reading of a routing header. A value
// carrying the sentinel that will not decode is returned untouched. The header
// is then malformed, which is its own violation, but inventing a second
// diagnosis here would mean two signals for one frame; falling back to the
// literal leaves the caller reporting the disagreement it already reports.
//
// Mcp-Param-{Name} wants the other half of that answer, since it has no
// name-versus-body comparison to fall back on, so it calls
// proxy.DecodeHeaderValue directly and reports the malformed value itself.
func decodeHeaderValue(value string) string {
	decoded, ok := proxy.DecodeHeaderValue(value)
	if !ok {
		return value
	}
	return decoded
}
