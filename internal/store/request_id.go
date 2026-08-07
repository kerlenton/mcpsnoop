package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// isValidRequestID reports whether id is a legal MCP request id: a JSON string or
// integer, never null, float, boolean, array, or object.
func isValidRequestID(id json.RawMessage) bool {
	if len(id) == 0 {
		return false
	}
	if bytes.Equal(bytes.TrimSpace(id), []byte("null")) {
		return false
	}
	var v any
	if err := json.Unmarshal(id, &v); err != nil {
		return false
	}
	switch x := v.(type) {
	case string:
		return true
	case float64:
		return x == math.Trunc(x)
	default:
		return false
	}
}

// invalidRequestIDKind names what arrived on the wire for warning text.
func invalidRequestIDKind(id json.RawMessage) string {
	if bytes.Equal(bytes.TrimSpace(id), []byte("null")) {
		return "null"
	}
	var v any
	if err := json.Unmarshal(id, &v); err != nil {
		return "malformed"
	}
	switch x := v.(type) {
	case float64:
		if x != math.Trunc(x) {
			return "a floating-point number"
		}
		return fmt.Sprintf("%v", x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// requestIDWarning reports when a request carries an id MCP forbids.
func requestIDWarning(msg proxy.RPCMessage) string {
	if msg.Method == "" || len(msg.ID) == 0 {
		return ""
	}
	if isValidRequestID(msg.ID) {
		return ""
	}
	kind := invalidRequestIDKind(msg.ID)
	if kind == "null" {
		return "request id is null, and 2026-07-28 requires a string or integer id"
	}
	return fmt.Sprintf("request id is %s, and 2026-07-28 requires a string or integer id", kind)
}

// correlateID is the key used to match a request to its response. Wire ids that
// are legal correlate normally; illegal ids identify nothing and each request
// is keyed by its envelope sequence instead.
func correlateID(wireID json.RawMessage, seq uint64) string {
	if isValidRequestID(wireID) {
		return string(wireID)
	}
	return fmt.Sprintf("@seq:%d", seq)
}
