package store

import (
	"mime"
	"strings"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// The two content types the Streamable HTTP transport is written in terms of.
const (
	contentTypeJSON = "application/json"
	contentTypeSSE  = "text/event-stream"
)

// acceptWarning reports a client POST whose Accept header does not offer both
// types the transport requires.
//
// "The client MUST include an `Accept` header listing both `application/json`
// and `text/event-stream` as supported content types." The same sentence is in
// 2025-11-25, so unlike the extension rules this one needs no revision gate: a
// client has owed both types for as long as the transport has existed.
//
// A server is free to answer with either, so offering only one is not a
// preference, it is a client that will reject half the legal answers. The usual
// symptom is a stream that never opens and no error anywhere.
func acceptWarning(dir proxy.Direction, headers *proxy.TransportHeaders) string {
	if dir != proxy.ClientToServer || headers == nil {
		// nil means the log predates mcpsnoop recording these, not that the client
		// sent nothing. Reading an old capture must not report every frame in it.
		return ""
	}
	if headers.Accept == "" {
		return "the request sent no Accept header, and the transport requires one listing " +
			contentTypeJSON + " and " + contentTypeSSE
	}
	var missing []string
	for _, want := range []string{contentTypeJSON, contentTypeSSE} {
		if !acceptOffers(headers.Accept, want) {
			missing = append(missing, want)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return "the request's Accept header does not offer " + strings.Join(missing, " or ") +
		", which the transport requires a client to list"
}

// acceptOffers reports whether an Accept header admits a media type, honouring
// the wildcards HTTP defines. A client sending */* has offered everything, and
// reporting it would be mcpsnoop inventing a stricter rule than the one written.
func acceptOffers(accept, want string) bool {
	wantType, _, _ := strings.Cut(want, "/")
	for _, part := range strings.Split(accept, ",") {
		media, _, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			// q-values and unknown parameters are handled by ParseMediaType; anything
			// it cannot read is compared whole rather than dropped, since discarding
			// it would be the same as the client not having sent it.
			media = strings.ToLower(strings.TrimSpace(strings.Split(part, ";")[0]))
		}
		if media == "*/*" || media == want || media == wantType+"/*" {
			return true
		}
	}
	return false
}

// responseContentTypeWarning reports a server answering a JSON-RPC request with
// a body that is neither of the two types the transport allows.
//
// "If the body is a JSON-RPC request, the server MUST return either
// `Content-Type: application/json` (a single JSON object) or
// `Content-Type: text/event-stream` (an SSE response stream)." Also unchanged
// since 2025-11-25.
//
// Only answers to a request are judged, which is why this is called from the
// response branch alone. A 202 acknowledging a notification carries no JSON-RPC
// body and never reaches it, and neither does a bare transport failure.
func responseContentTypeWarning(dir proxy.Direction, headers *proxy.TransportHeaders) string {
	if dir != proxy.ServerToClient || headers == nil {
		return ""
	}
	if headers.ContentType == "" {
		return "the response to a request carried no Content-Type, and the transport requires " +
			contentTypeJSON + " or " + contentTypeSSE
	}
	media, _, err := mime.ParseMediaType(headers.ContentType)
	if err != nil {
		media = strings.ToLower(strings.TrimSpace(strings.Split(headers.ContentType, ";")[0]))
	}
	if media == contentTypeJSON || media == contentTypeSSE {
		return ""
	}
	return "the response to a request is " + media + ", and the transport allows only " +
		contentTypeJSON + " or " + contentTypeSSE
}
