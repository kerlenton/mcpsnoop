package proxy

import (
	"encoding/base64"
	"strings"
)

// Streamable HTTP carries the target operation in the Mcp-Name header and one
// tool argument per Mcp-Param-{Name} header, but an HTTP field value may hold
// only visible ASCII, space and tab (RFC 9110). A value outside that set
// therefore travels Base64, wrapped in a sentinel:
//
//	Mcp-Name: =?base64?SGVsbG8sIOS4lueVjA==?=
//
// The markers are case-sensitive and must appear exactly as shown, and the spec
// requires anyone comparing such a header to a body value to decode it first.
// They live in the proxy because the encoding belongs to the transport, and
// because both the shim (which scrubs headers) and the store (which compares
// them) need the same answer.
const (
	Base64SentinelPrefix = "=?base64?"
	Base64SentinelSuffix = "?="
)

// DecodeHeaderValue returns the value a routing header carries, undoing the
// Base64 sentinel when one is present. ok is false only when the value claims
// the sentinel and its payload will not decode, which makes the header
// malformed; a plain value is returned as it stands with ok true.
//
// It decodes exactly once, never recursively. A plain value that happens to look
// like the sentinel is itself encoded by the client for precisely that reason,
// so decoding the result again would corrupt it back into something the body
// never said.
func DecodeHeaderValue(value string) (string, bool) {
	inner, wrapped := strings.CutPrefix(value, Base64SentinelPrefix)
	if !wrapped {
		return value, true
	}
	inner, wrapped = strings.CutSuffix(inner, Base64SentinelSuffix)
	if !wrapped {
		return value, true
	}
	if decoded, err := base64.StdEncoding.DecodeString(inner); err == nil {
		return string(decoded), true
	}
	// Padded standard Base64 is what the spec shows and what every mainstream
	// encoder emits, but accept the unpadded form too. The cost of being strict
	// is a false mismatch on a client whose only sin is padding; the cost of being
	// lenient is nil, since a genuinely corrupt value still fails to match.
	if decoded, err := base64.RawStdEncoding.DecodeString(inner); err == nil {
		return string(decoded), true
	}
	return "", false
}

// EncodeHeaderValue renders a value for a routing header, wrapping it in the
// Base64 sentinel when it cannot travel as it stands.
//
// This is the other half of DecodeHeaderValue and it exists for the one caller
// that has to build a header rather than read one: a replay derives Mcp-Name
// from the body it is about to send, because the header "MUST match" that body
// and a value copied from the capture would not when the body has been edited.
//
// The rule is the transport's. A field value may hold only visible ASCII, space
// and tab, so anything outside that set is wrapped, and so is a value with
// leading or trailing whitespace, which a reader would strip. A plain value that
// happens to spell the sentinel is wrapped too, for the same reason the decoder
// only unwraps once: left alone it would decode into something the body never
// said.
func EncodeHeaderValue(value string) string {
	if needsSentinel(value) {
		return Base64SentinelPrefix + base64.StdEncoding.EncodeToString([]byte(value)) + Base64SentinelSuffix
	}
	return value
}

func needsSentinel(value string) bool {
	if value == "" {
		return false
	}
	if strings.TrimSpace(value) != value {
		return true
	}
	if strings.HasPrefix(value, Base64SentinelPrefix) && strings.HasSuffix(value, Base64SentinelSuffix) {
		return true
	}
	for i := range len(value) {
		if c := value[i]; c != '\t' && (c < 0x20 || c > 0x7e) {
			return true
		}
	}
	return false
}
