package store

import (
	"encoding/base64"
	"strings"
)

// Streamable HTTP carries the target operation in the Mcp-Name header, but an
// HTTP field value may hold only visible ASCII, space and tab (RFC 9110). A
// name or resource URI outside that set therefore travels Base64, wrapped in a
// sentinel:
//
//	Mcp-Name: =?base64?SGVsbG8sIOS4lueVjA==?=
//
// The markers are case-sensitive and must appear exactly as shown, and the spec
// requires anyone comparing such a header to a body value to decode it first.
const (
	base64SentinelPrefix = "=?base64?"
	base64SentinelSuffix = "?="
)

// decodeHeaderValue returns the value a routing header carries, undoing the
// Base64 sentinel when one is present. Mcp-Param-{Name} uses the same encoding,
// so this is the shared entry point for that check when it lands.
//
// It decodes exactly once, never recursively. A plain value that happens to look
// like the sentinel is itself encoded by the client for precisely that reason,
// so decoding the result again would corrupt it back into something the body
// never said.
//
// A value carrying the sentinel that will not decode is returned untouched. The
// header is then malformed, which is its own violation, but inventing a second
// diagnosis here would mean two signals for one frame; falling back to the
// literal leaves the caller reporting the disagreement it already reports.
func decodeHeaderValue(value string) string {
	inner, ok := strings.CutPrefix(value, base64SentinelPrefix)
	if !ok {
		return value
	}
	inner, ok = strings.CutSuffix(inner, base64SentinelSuffix)
	if !ok {
		return value
	}
	if decoded, err := base64.StdEncoding.DecodeString(inner); err == nil {
		return string(decoded)
	}
	// Padded standard Base64 is what the spec shows and what every mainstream
	// encoder emits, but accept the unpadded form too. The cost of being strict
	// is reintroducing the exact bug this fixes, a false mismatch on a client
	// whose only sin is padding; the cost of being lenient is nil, since a
	// genuinely corrupt value still fails to match the body.
	if decoded, err := base64.RawStdEncoding.DecodeString(inner); err == nil {
		return string(decoded)
	}
	return value
}
