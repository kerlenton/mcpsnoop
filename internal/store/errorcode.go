package store

import "strconv"

// JSON-RPC 2.0 reserves -32000 to -32099 for implementation-defined server
// errors, and 2026-07-28 partitions that range: -32000 to -32019 stays
// implementation-defined and the specification will never allocate there, while
// -32020 to -32099 is reserved for spec-defined codes, allocated sequentially
// from -32020.
//
// That partition is why this file names some codes and refuses to name others.
// A code in the implementation-defined sub-range means whatever the server that
// sent it decided, and the spec is explicit that receivers must not assign
// cross-implementation semantics to them. Naming one would put words in the
// sender's mouth, which is the opposite of what a wire debugger is for.

// ErrorCodeName returns the name the specification gives an error code, or ""
// when it gives none. The four JSON-RPC codes are named alongside the MCP ones
// because a reader looking at -32602 is helped as much as one looking at -32020.
func ErrorCodeName(code int) string {
	switch code {
	case -32700:
		return "parse error"
	case -32600:
		return "invalid request"
	case -32601:
		return "method not found"
	case -32602:
		return "invalid params"
	case -32603:
		return "internal error"
	case -32020:
		return "header mismatch"
	case -32021:
		return "missing required client capability"
	case -32022:
		return "unsupported protocol version"
	}
	return ""
}

// ErrorCodeHeaderMismatch is the server's verdict that a request's headers did
// not match its body, or that a required one was missing or malformed. It is
// the same condition mcpsnoop checks itself, reported by the party that is
// authoritative about it and that also validates what mcpsnoop cannot see.
const ErrorCodeHeaderMismatch = -32020

// ErrorCodeText renders a code with its name for a one-line preview: the number
// leads, because that is what a reader matches against the spec and against a
// server's own documentation, and the name follows when there is one.
func ErrorCodeText(code int) string {
	if name := ErrorCodeName(code); name != "" {
		return strconv.Itoa(code) + " " + name
	}
	return strconv.Itoa(code)
}
