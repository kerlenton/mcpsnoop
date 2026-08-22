package replay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// HTTPTarget is where a replayed request goes and what it carries besides the
// body.
//
// URL comes from whoever is replaying, never from the capture. What a capture
// records is the endpoint with its userinfo and every query value removed, on
// purpose, so it identifies the server without being an address to dial. A
// replay reaches a live server, which may be the production one, so the person
// asking for it says where it goes.
type HTTPTarget struct {
	URL string
	// Headers are extra request headers as "Name: value". They are applied last,
	// so a caller can override anything below, and they are how a credential
	// reaches the request. mcpsnoop never records one and never replays one.
	Headers []string
}

// Routing is the request metadata a captured frame carried, re-sent verbatim.
//
// The transport makes these mandatory: Mcp-Method on every request, Mcp-Name on
// tools/call, resources/read and prompts/get, and one Mcp-Param-{Name} per
// parameter a tool annotated with x-mcp-header. A server rejects a mismatch with
// 400 and -32020, so a POST of the bare captured body gets an error rather than
// a result.
//
// Verbatim matters. A value outside the safe ASCII set travels Base64-wrapped in
// the =?base64?…?= sentinel, and the capture holds whatever the client sent, so
// re-sending it needs no encoder and cannot disagree with what the body says.
type Routing struct {
	// Name is the Mcp-Name the capture carried. It is a fallback: the header is
	// derived from the body actually being sent, because the transport says it
	// "MUST match" that body and an edit can change what the body names. The
	// captured value is used only when the body names nothing, which is what a
	// capture from a client that predates this revision looks like.
	Name         string
	ParamHeaders []proxy.MCPParamHeader
	// Edited says the person replaying rewrote the params. The captured
	// Mcp-Param-* headers mirror the captured arguments, so against a body
	// somebody rewrote they assert something mcpsnoop cannot know is still true.
	Edited bool
}

// methodsNeedingName are the requests the transport marks Mcp-Name REQUIRED for.
var methodsNeedingName = map[string]struct{}{
	"tools/call":     {},
	"resources/read": {},
	"prompts/get":    {},
}

// ReplayHTTP re-issues one captured request as an HTTP POST against target.
//
// It sends exactly one request. The transport makes every message its own POST
// and this revision has no handshake to perform, so there is nothing to
// negotiate first: the request describes itself in its own _meta, which
// withClientMeta builds, and in the headers that mirror it.
func ReplayHTTP(ctx context.Context, target HTTPTarget, method string, params json.RawMessage, routing Routing, timeout time.Duration) (Result, error) {
	if strings.TrimSpace(target.URL) == "" {
		return Result{}, errors.New("replay: no target to send to, pass --replay-target")
	}
	if _, err := url.Parse(target.URL); err != nil {
		return Result{}, fmt.Errorf("replay: invalid target %q: %w", target.URL, err)
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	// A redacted header value is mcpsnoop's placeholder, not the client's data.
	// Sending it would put that placeholder on a live server as though a user had
	// typed it, and omitting it would produce a header mismatch the server reports
	// as the client's fault. Refusing says which it is.
	for _, h := range routing.ParamHeaders {
		if h.Redacted {
			return Result{}, fmt.Errorf("replay: %s was scrubbed by redaction in this capture, so there is nothing to re-send; replay from an unredacted capture", h.Name)
		}
	}

	body := withClientMeta(params)
	frame := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": body}
	encoded, err := json.Marshal(frame)
	if err != nil {
		return Result{}, fmt.Errorf("replay: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, bytes.NewReader(encoded))
	if err != nil {
		return Result{}, fmt.Errorf("replay: %w", err)
	}
	applyReplayHeaders(req, method, body, routing, target.Headers)

	start := time.Now()
	resp, err := replayClient.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("replay: %w", err)
	}
	defer resp.Body.Close()

	out := Result{Method: method, Params: body}
	raw, err := readReplayResponse(resp)
	out.Duration = time.Since(start)
	if err != nil {
		return out, err
	}
	out.Response = raw

	var msg struct {
		Result json.RawMessage `json:"result"`
		Error  *proxy.RPCError `json:"error"`
	}
	// Both halves. Valid JSON that carries neither a result nor an error is not a
	// response to anything, and reporting it as ok let a proxy's own success page
	// read as the server having answered.
	if json.Unmarshal(raw, &msg) != nil || (len(msg.Result) == 0 && msg.Error == nil) {
		return out, fmt.Errorf("replay: the server answered something that is not a JSON-RPC response: %s", clip(raw))
	}
	out.RPCResult, out.Err = msg.Result, msg.Error
	out.ToolErr = isToolError(msg.Result)
	return out, nil
}

// applyReplayHeaders sets what the transport requires of every POST, then
// whatever the caller asked for, so a caller can override any of it.
func applyReplayHeaders(req *http.Request, method string, body json.RawMessage, routing Routing, extra []string) {
	req.Header.Set("Content-Type", "application/json")
	// Both types, because the client "MUST include an Accept header listing both
	// application/json and text/event-stream", and a server may answer either.
	req.Header.Set("Accept", "application/json, text/event-stream")
	// Derived from what withClientMeta put in the body, never copied from the
	// capture. The header value "MUST match the io.modelcontextprotocol/protocolVersion
	// field carried in the request body's _meta", and the body is rebuilt here, so
	// echoing the captured revision would disagree with it by construction.
	req.Header.Set("MCP-Protocol-Version", statelessProtocolVersion)
	req.Header.Set("Mcp-Method", method)
	if _, needs := methodsNeedingName[method]; needs {
		if name := mcpName(body, routing.Name); name != "" {
			req.Header.Set("Mcp-Name", name)
		}
	}
	if !routing.Edited {
		for _, h := range routing.ParamHeaders {
			// Only the family the annotations produce. A capture is a file people hand
			// around, and setting whatever name it carries let a doctored one overwrite
			// the mandatory headers above, or add an Authorization the operator never
			// passed.
			if !strings.HasPrefix(textproto.CanonicalMIMEHeaderKey(h.Name), mcpParamPrefix) {
				continue
			}
			req.Header.Set(h.Name, h.Value)
		}
	}
	for _, h := range extra {
		name, value, ok := strings.Cut(h, ":")
		if !ok {
			continue
		}
		req.Header.Set(strings.TrimSpace(name), strings.TrimSpace(value))
	}
}

// mcpParamPrefix is the header family a tool's x-mcp-header annotations produce,
// in the spelling net/http canonicalises to.
const mcpParamPrefix = "Mcp-Param-"

// mcpName is the operation the request names, taken from the body being sent.
//
// The transport sources this header from "params.name or params.uri", and
// requires a server to reject a header that disagrees with the body. So it is
// derived rather than copied: an edit that renames the tool would otherwise send
// the captured name against a body naming another, which is exactly the mismatch
// the server is told to refuse. The captured value is the fallback for a body
// that names nothing.
func mcpName(body json.RawMessage, captured string) string {
	var params struct {
		Name string `json:"name"`
		URI  string `json:"uri"`
	}
	if json.Unmarshal(body, &params) == nil {
		if params.Name != "" {
			return proxy.EncodeHeaderValue(params.Name)
		}
		if params.URI != "" {
			return proxy.EncodeHeaderValue(params.URI)
		}
	}
	return captured
}

// readReplayResponse reads whichever of the two shapes the server chose.
//
// A request is answered with "either Content-Type: application/json (a single
// JSON object) or Content-Type: text/event-stream (an SSE response stream)", and
// on a stream "the final JSON-RPC response SHOULD terminate the stream", with
// request-scoped notifications before it. A replay wants the response, so the
// notifications are read past.
func readReplayResponse(resp *http.Response) (json.RawMessage, error) {
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp)
	}
	// Compared case-insensitively. A media type is case-insensitive in HTTP and Go
	// canonicalises header names but never values, so a server spelling it
	// Text/Event-Stream sent a perfectly legal answer that was then parsed as one
	// JSON object and reported as garbage.
	if isEventStream(resp.Header.Get("Content-Type")) {
		return readSSEResponse(resp.Body)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxReplayBody+1))
	if err != nil {
		return nil, fmt.Errorf("replay: %w", err)
	}
	if len(raw) > maxReplayBody {
		// Said rather than handed over cut in half. A truncated body is not a server
		// that answered nonsense, and blaming it for one would send somebody looking
		// in the wrong place.
		return nil, fmt.Errorf("replay: the answer is larger than the %d MiB a replay will hold, so it was not read", maxReplayBody>>20)
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, fmt.Errorf("replay: the server answered %d with an empty body", resp.StatusCode)
	}
	return json.RawMessage(raw), nil
}

// isEventStream reports whether a Content-Type names the streamed shape,
// ignoring case and any parameters after the type itself.
func isEventStream(contentType string) bool {
	media, _, _ := strings.Cut(contentType, ";")
	return strings.EqualFold(strings.TrimSpace(media), "text/event-stream")
}

// maxReplayBody bounds what one replayed answer may hand back, since the overlay
// that renders it holds it in memory and the server on the other end is not
// mcpsnoop's.
const maxReplayBody = 8 << 20

// replayClient refuses to follow a redirect.
//
// The address a replay posts to is the one the person replaying named and
// answered for, and following a redirect hands that choice back to the far end.
// Go's default client would resend the body and, on a hop that only changes the
// port, the Authorization header too, so a staging endpoint answering 307 could
// deliver a mutating tools/call and a credential to production while the overlay
// reported success and never named the host that answered. A 301, 302 or 303
// would additionally rewrite the POST into a bodiless GET.
//
// So the hop is reported instead, with the address it wanted, and the person
// decides whether to name that one.
var replayClient = &http.Client{
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// statusError turns a non-200 into a message that says what to do about it.
//
// The transport distinguishes these, so the reader is told which one happened
// rather than being handed a number. A 400 is where a modern server reports a
// header or version problem, which is exactly what a replay gets wrong, and a
// 404 with a JSON-RPC body is an unknown method rather than a wrong address.
func statusError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxReplayBody))
	raw = bytes.TrimSpace(raw)
	var msg struct {
		Error *proxy.RPCError `json:"error"`
	}
	modern := json.Unmarshal(raw, &msg) == nil && msg.Error != nil

	switch {
	case isRedirect(resp.StatusCode):
		to := resp.Header.Get("Location")
		if to == "" {
			to = "somewhere it did not name"
		}
		return fmt.Errorf("replay: %s answered %d redirecting to %s, and a replay goes only where you named; pass --replay-target %s if that is where you meant to send it",
			resp.Request.URL.Redacted(), resp.StatusCode, to, to)
	case resp.StatusCode == http.StatusUnauthorized:
		challenge := strings.Join(resp.Header.Values("WWW-Authenticate"), ", ")
		if challenge == "" {
			return errors.New("replay: the server answered 401 and named no scheme; pass a credential with --replay-header")
		}
		return fmt.Errorf("replay: the server answered 401 demanding %s; pass a credential with --replay-header", challenge)
	case modern && msg.Error.Code == headerMismatchCode:
		return fmt.Errorf("replay: the server rejected the request metadata: %s", msg.Error.Message)
	case modern:
		return fmt.Errorf("replay: the server answered %d with %s", resp.StatusCode, msg.Error.Message)
	case resp.StatusCode == http.StatusBadRequest, resp.StatusCode == http.StatusNotFound,
		resp.StatusCode == http.StatusMethodNotAllowed:
		// The era test the transport prescribes, run the other way round from the
		// stdio one. A modern server answers these with a JSON-RPC error, so a body
		// that is not one means the address is not a modern MCP endpoint.
		return fmt.Errorf("replay: %s answered %d with no JSON-RPC error, so it is not a Streamable HTTP MCP endpoint of this revision",
			resp.Request.URL.Redacted(), resp.StatusCode)
	case len(raw) == 0:
		return fmt.Errorf("replay: the server answered %d with an empty body", resp.StatusCode)
	default:
		return fmt.Errorf("replay: the server answered %d: %s", resp.StatusCode, clip(raw))
	}
}

// headerMismatchCode is what a server returns when the mirrored headers and the
// body disagree, which is the failure a replay is most likely to cause.
const headerMismatchCode = -32020

// isRedirect covers the codes a client would otherwise follow.
func isRedirect(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// readSSEResponse reads an event stream until the JSON-RPC response arrives.
func readSSEResponse(r io.Reader) (json.RawMessage, error) {
	scanner := bufio.NewScanner(io.LimitReader(r, maxReplayBody))
	scanner.Buffer(make([]byte, 0, 64<<10), maxReplayBody)
	var data []byte
	for scanner.Scan() {
		line := scanner.Bytes()
		switch {
		case len(bytes.TrimSpace(line)) == 0:
			// End of one event. A response terminates the stream; anything else is a
			// request-scoped notification and is read past.
			if len(data) > 0 {
				if isRPCResponse(data) {
					return json.RawMessage(append([]byte(nil), data...)), nil
				}
				data = data[:0]
			}
		case bytes.HasPrefix(line, []byte(":")):
			// A comment, which servers send as a keep-alive. Ignored, as the spec says.
		case bytes.HasPrefix(line, []byte("data:")):
			chunk := bytes.TrimPrefix(line, []byte("data:"))
			chunk = bytes.TrimPrefix(chunk, []byte(" "))
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, chunk...)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("replay: reading the response stream: %w", err)
	}
	if len(data) > 0 && isRPCResponse(data) {
		return json.RawMessage(data), nil
	}
	return nil, errors.New("replay: the response stream ended without a JSON-RPC response")
}

// isRPCResponse reports whether a stream event is the answer rather than one of
// the notifications that precede it.
func isRPCResponse(data []byte) bool {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &msg) != nil {
		return false
	}
	return msg.Method == "" && len(msg.ID) > 0 && (len(msg.Result) > 0 || len(msg.Error) > 0)
}

// clip bounds a server's answer when it goes into an error message, since it is
// a stranger's bytes and the message is read in a terminal.
func clip(raw []byte) string {
	const limit = 200
	if len(raw) <= limit {
		return string(raw)
	}
	return string(raw[:limit]) + "…"
}
