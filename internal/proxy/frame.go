package proxy

import (
	"encoding/json"
	"time"
)

// Direction describes which way an observed event was travelling.
type Direction string

const (
	// ClientToServer is a frame read from our stdin (the client) on its way to
	// the wrapped server's stdin.
	ClientToServer Direction = "c2s"
	// ServerToClient is a frame read from the wrapped server's stdout on its way
	// to our stdout (the client).
	ServerToClient Direction = "s2c"
	// ServerStderr is a line the wrapped server wrote to stderr.
	ServerStderr Direction = "stderr"
	// DirectionMeta is a control frame the shim emits once at startup describing
	// the session (the server command), so the hub can replay against an
	// isolated copy of the server. Raw holds a marshalled SessionMeta.
	DirectionMeta Direction = "meta"
)

// SessionMeta describes a proxied session so it can be replayed later (even from
// a backfilled log). It travels as the first envelope of a session.
type SessionMeta struct {
	Command []string `json:"command"`
	CWD     string   `json:"cwd,omitempty"`
}

// Envelope wraps a single observed event for transport to the hub and for the
// on-disk session log. The shim stays dumb, it never correlates or interprets,
// it just timestamps and labels. All correlation/timing lives in the hub.
type Envelope struct {
	SessionID   string    `json:"session_id"`
	ServerLabel string    `json:"server_label"`
	Seq         uint64    `json:"seq"`
	TS          time.Time `json:"ts"`
	Direction   Direction `json:"direction"`
	Transport   string    `json:"transport"`
	// Raw holds the original JSON-RPC message bytes, untouched. Empty for stderr.
	Raw json.RawMessage `json:"raw,omitempty"`
	// Text holds a plain line (used for stderr). Empty for JSON-RPC frames.
	Text string `json:"text,omitempty"`
	// MCPMethod and MCPName are the Streamable HTTP routing headers (Mcp-Method,
	// Mcp-Name; SEP-2243), set on client requests so a gateway can route without
	// reading the body. Empty for stdio and pre-2026 HTTP servers.
	MCPMethod string `json:"mcp_method,omitempty"`
	MCPName   string `json:"mcp_name,omitempty"`
	// Batch marks a frame that was one element of a JSON-RPC batch array. Routing
	// headers address a single operation, so they cannot describe a batch.
	Batch bool `json:"batch,omitempty"`
}

// RPCError is the JSON-RPC error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// RPCMessage is a best-effort peek at a JSON-RPC frame. We never re-marshal it
// onto the wire (the raw bytes are forwarded verbatim). This is only used by
// the hub to classify and correlate.
type RPCMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// IsRequest reports whether the message is a request or notification (has a
// method). A request has an id, a notification does not.
func (m *RPCMessage) IsRequest() bool { return m.Method != "" }

// IsNotification reports whether the message is a notification (method, no id).
func (m *RPCMessage) IsNotification() bool { return m.Method != "" && len(m.ID) == 0 }

// IsResponse reports whether the message is a response (result or error, no method).
func (m *RPCMessage) IsResponse() bool {
	return m.Method == "" && (len(m.Result) > 0 || m.Error != nil)
}

// ParseRPC attempts to decode raw bytes as a JSON-RPC message. ok is false when
// the bytes are not a JSON object we recognise. Callers must still forward the
// raw bytes regardless.
func ParseRPC(raw []byte) (msg RPCMessage, ok bool) {
	if err := json.Unmarshal(raw, &msg); err != nil {
		return RPCMessage{}, false
	}
	return msg, msg.JSONRPC != "" || msg.Method != "" || len(msg.Result) > 0 || msg.Error != nil
}

// splitObserved routes an observed protocol line into an envelope's Raw or Text
// field. Valid JSON goes to Raw, so it can be parsed, redacted, and replayed.
// Anything else goes to Text, since json.RawMessage cannot round-trip non-JSON bytes
// through the envelope encoder (the encoder validates it and the frame would be
// silently dropped), and a non-JSON line on the protocol channel is exactly the
// stdout-corruption case the store flags as invalid.
func splitObserved(line []byte) (raw json.RawMessage, text string) {
	if json.Valid(line) {
		return line, ""
	}
	return nil, string(line)
}
