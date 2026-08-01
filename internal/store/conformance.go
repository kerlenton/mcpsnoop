package store

import (
	"encoding/json"
	"slices"
	"strconv"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// statelessFrom is the revision that removed server-initiated requests and made
// the per-request _meta fields mandatory.
const statelessFrom = "2026-07-28"

// mrtrCapableMethods are the client requests a server may answer with an
// InputRequiredResult. The spec lists exactly these three and then says
// "Servers MUST NOT send InputRequiredResult responses on any other client
// requests."
var mrtrCapableMethods = map[string]struct{}{
	"prompts/get":    {},
	"resources/read": {},
	"tools/call":     {},
}

// inputRequestMethods are the request objects an inputRequests value may be.
// The spec names ElicitRequest, CreateMessageRequest and ListRootsRequest, which
// are these three methods.
var inputRequestMethods = map[string]struct{}{
	"elicitation/create":     {},
	"sampling/createMessage": {},
	"roots/list":             {},
}

// wrongDirectionWarning reports a frame travelling a way 2026-07-28 removed.
//
// The revision routes every server-to-client request through MRTR instead, and
// says so as flatly as it says anything: "Servers MUST send server-to-client
// requests (such as roots/list, sampling/createMessage, or elicitation/create)
// using the MRTR pattern. The previous pattern of server-initiated requests is
// no longer supported. This is a breaking change." Both transports repeat it,
// stdio with "The server MUST NOT write JSON-RPC requests to stdout" and
// Streamable HTTP with "The server MUST NOT send independent JSON-RPC requests
// on this stream", and each states the mirror half for the client.
//
// Gated on the frame's own direction and on a revision the session was actually
// observed declaring, since a legacy server doing this is conforming to its own
// revision. A capture that never saw a client request declaring 2026-07-28 says
// nothing, which is the answer for one that starts mid-session.
func wrongDirectionWarning(dir proxy.Direction, msg proxy.RPCMessage, stateless bool) string {
	if !stateless {
		return ""
	}
	switch {
	case dir == proxy.ServerToClient && msg.Method != "" && len(msg.ID) > 0:
		return "server sent a JSON-RPC request " + strconv.Quote(msg.Method) +
			", which 2026-07-28 replaced with the MRTR pattern"
	case dir == proxy.ClientToServer && msg.Method == "" && (len(msg.Result) > 0 || msg.Error != nil):
		return "client sent a JSON-RPC response, which 2026-07-28 no longer defines"
	}
	return ""
}

// inputRequiredWarnings reports what an InputRequiredResult breaks. c is the
// request it answers, nil when mcpsnoop never saw that half, in which case the
// method rule cannot be decided and stays silent.
func inputRequiredWarnings(state inputRequired, c *call) []string {
	var warnings []string
	if c != nil {
		if _, allowed := mrtrCapableMethods[c.method]; !allowed {
			warnings = append(warnings, "InputRequiredResult on "+strconv.Quote(c.method)+
				", which 2026-07-28 allows only on prompts/get, resources/read and tools/call")
		}
	}
	// "Servers MUST include at least one of inputRequests or requestState in
	// every InputRequiredResult response."
	if !state.hasRequestState && len(state.methods) == 0 && state.keys == "" {
		warnings = append(warnings, "InputRequiredResult carries neither inputRequests nor requestState, "+
			"and 2026-07-28 requires at least one")
	}
	for _, method := range state.methods {
		if _, ok := inputRequestMethods[method]; !ok {
			warnings = append(warnings, "inputRequests names "+strconv.Quote(method)+
				", and 2026-07-28 allows only elicitation/create, sampling/createMessage and roots/list")
		}
	}
	return warnings
}

// serverCancelWarning reports a server cancelling something it may not. The spec
// gives the server exactly one use for the notification, "A server MUST send
// notifications/cancelled referencing a subscriptions/listen request ID when it
// tears down that subscription stream", and then "Servers MUST NOT send
// notifications/cancelled for any other purpose."
//
// Decided from the referenced request, so an id mcpsnoop never observed produces
// nothing, and judged by the revision that request itself declared, since earlier
// revisions let either party cancel anything.
func serverCancelWarning(dir proxy.Direction, c *call) string {
	if dir != proxy.ServerToClient || c == nil {
		return ""
	}
	if !atLeastRevision(c.protocolVersion, statelessFrom) {
		return ""
	}
	if c.method == "subscriptions/listen" {
		return ""
	}
	return "server cancelled " + strconv.Quote(c.method) +
		", and 2026-07-28 lets a server cancel only a subscriptions/listen request"
}

// progressTracker holds the last progress value seen for a token, and whether
// the request that issued the token has finished.
type progressTracker struct {
	value float64
	done  bool
}

// progressWarning reports a progress value that did not increase, or one sent
// after the request it belongs to completed. The spec is arithmetic here: "The
// progress value MUST increase with each notification, even if the total is
// unknown" and "Progress notifications MUST stop after completion."
//
// Only pairs mcpsnoop actually observed are compared, so a capture that starts
// mid-stream has nothing to measure against. A token is forgotten when its
// request completes rather than kept forever, because the spec requires a token
// to be unique only "across all active requests", so a client may legitimately
// spend the same token again on a later one.
func (sess *session) progressWarning(params json.RawMessage) string {
	var p struct {
		Token    json.RawMessage `json:"progressToken"`
		Progress *float64        `json:"progress"`
	}
	if json.Unmarshal(params, &p) != nil || len(p.Token) == 0 || p.Progress == nil {
		return ""
	}
	token := string(p.Token)
	previous, seen := sess.progress[token]
	if sess.progress == nil {
		sess.progress = make(map[string]progressTracker)
	}
	switch {
	case seen && previous.done:
		sess.progress[token] = progressTracker{value: *p.Progress, done: true}
		return "progress notification for token " + token +
			" arrived after its request completed, and 2026-07-28 requires them to stop"
	case seen && *p.Progress <= previous.value:
		sess.progress[token] = progressTracker{value: *p.Progress}
		return "progress for token " + token + " went from " +
			strconv.FormatFloat(previous.value, 'g', -1, 64) + " to " +
			strconv.FormatFloat(*p.Progress, 'g', -1, 64) +
			", and 2026-07-28 requires it to increase with each notification"
	}
	sess.progress[token] = progressTracker{value: *p.Progress}
	return ""
}

// noteProgressToken remembers the token a request carries, so a later
// notification has something to compare against and so completion can close it.
func (sess *session) noteProgressToken(params json.RawMessage) string {
	var p struct {
		Meta struct {
			ProgressToken json.RawMessage `json:"progressToken"`
		} `json:"_meta"`
	}
	if json.Unmarshal(params, &p) != nil || len(p.Meta.ProgressToken) == 0 {
		return ""
	}
	token := string(p.Meta.ProgressToken)
	if sess.progress == nil {
		sess.progress = make(map[string]progressTracker)
	}
	// A fresh request reopens the token, which is what makes reuse after
	// completion legal rather than a violation.
	delete(sess.progress, token)
	return token
}

// closeProgressToken marks a token's request finished, so a notification after
// it is reported rather than compared.
func (sess *session) closeProgressToken(token string) {
	if token == "" || sess.progress == nil {
		return
	}
	entry := sess.progress[token]
	entry.done = true
	sess.progress[token] = entry
}

// missingProtocolVersionWarning reports a request whose _meta omits the version
// field the revision makes mandatory. The base protocol marks
// io.modelcontextprotocol/protocolVersion Required and says "A request missing
// any required field is malformed; the server MUST reject it with JSON-RPC error
// code -32602 (Invalid params)."
//
// The caller has already established that this request is under the revision,
// which on stdio it can only do from the field itself. So this fires only where
// the MCP-Protocol-Version header supplied the revision the absent field should
// have carried, and a stdio request that declares nothing stays a legacy request
// rather than a malformed modern one.
func missingProtocolVersionWarning(params json.RawMessage) string {
	if metaProtocolVersion(params) != "" {
		return ""
	}
	return "request _meta is missing required io.modelcontextprotocol/protocolVersion"
}

// declaredClientCapabilities is the set the request itself declared, read from
// its own _meta rather than from anything the session accumulated, since a
// connection is not a session and two clients may interleave.
func declaredClientCapabilities(params json.RawMessage) (map[string]json.RawMessage, bool) {
	var p struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if json.Unmarshal(params, &p) != nil {
		return nil, false
	}
	raw, ok := p.Meta["io.modelcontextprotocol/clientCapabilities"]
	if !ok {
		return nil, false
	}
	var caps map[string]json.RawMessage
	if json.Unmarshal(raw, &caps) != nil {
		return nil, false
	}
	return caps, true
}

// undeclaredInputRequestWarnings reports an inputRequests the client never said
// it could fulfil. "Servers MUST NOT send an inputRequests that the client has
// not declared support for in its capabilities."
//
// Read from the matched request's own declaration, so an unmatched response and
// a request that declared nothing at all both stay silent.
func undeclaredInputRequestWarnings(state inputRequired, c *call) []string {
	if c == nil {
		return nil
	}
	caps, declared := declaredClientCapabilities(c.params)
	if !declared {
		return nil
	}
	needs := map[string]string{
		"elicitation/create":     "elicitation",
		"sampling/createMessage": "sampling",
		"roots/list":             "roots",
	}
	var warnings []string
	for _, method := range slices.Sorted(slices.Values(state.methods)) {
		capability, known := needs[method]
		if !known {
			continue
		}
		if _, ok := caps[capability]; !ok {
			warnings = append(warnings, "inputRequests asks for "+strconv.Quote(method)+
				" while the request declared no "+capability+" capability")
		}
	}
	return warnings
}
