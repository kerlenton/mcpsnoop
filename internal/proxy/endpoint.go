package proxy

import (
	"net/url"
	"strings"
)

// strippedValue marks a part of an endpoint mcpsnoop removed before writing it
// down. It is deliberately not redactedValue: that placeholder means a
// --redact-secrets, --redact-key, --redact-value or --redact-path rule matched,
// and a reader who is running with none of those and sees it would be right to
// call it a bug. This removal happens either way.
const strippedValue = "[stripped]"

// EndpointForLog renders an MCP endpoint URL for the session log with the parts
// that could carry a credential removed by construction rather than by pattern.
//
// The Streamable HTTP transport says a server "MUST provide a single HTTP
// endpoint path (hereafter referred to as the MCP endpoint)", so that URL
// identifies an HTTP server the way argv identifies a stdio one, and an HTTP
// capture that does not record it cannot say what it captured. The label is not
// a substitute, because it defaults to the target host alone and two proxies
// pointed at two paths of one host produce the same one.
//
// Writing the URL down verbatim is what needs care. Redaction is off unless
// asked for, so a target carrying ?api_key=... would reach the log of every user
// who never turned it on, and this is not a payload they chose to send but a
// flag they had to pass to run the proxy at all. Three parts go and the rest
// stays, since a reader still has to tell two endpoints apart.
//
//   - Userinfo goes whole and leaves a marker behind. A password is never
//     anything else, and Basic credentials routinely put the key in the user
//     half, so keeping either half keeps the secret.
//   - Query values go and their keys stay. The key is what distinguishes two
//     endpoints of one host, and the value is where a token hides. Telling an
//     api_key from a region needs a denylist, and a denylist fails open.
//   - The fragment goes because it never reaches the server. RFC 3986 makes it
//     client-side and Go builds the forwarded request line from path and query
//     alone, so dropping it records what was actually on the wire.
//
// An empty string means there was nothing safe to record, which is also what a
// target with no host gives, matching the condition the http command already
// uses to decide a target is usable at all.
func EndpointForLog(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		// Unparseable is not a reason to fall back to the original bytes. Whatever
		// is in there is precisely what could not be inspected.
		return ""
	}
	var b strings.Builder
	if u.Scheme != "" {
		b.WriteString(u.Scheme)
		b.WriteString("://")
	}
	if u.User != nil {
		b.WriteString(strippedValue)
		b.WriteByte('@')
	}
	b.WriteString(u.Host)
	b.WriteString(u.EscapedPath())
	if q := stripQueryValues(u.RawQuery); q != "" {
		b.WriteByte('?')
		b.WriteString(q)
	}
	return b.String()
}

// stripQueryValues rewrites each value of a raw query in place, walking the
// bytes rather than round-tripping through url.Values. Encode sorts its keys and
// re-escapes what it emits, so a round trip would reorder and respell a query
// the user typed, and this only has to change the halves after the equals signs.
func stripQueryValues(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	parts := strings.Split(rawQuery, "&")
	for i, part := range parts {
		key, value, ok := strings.Cut(part, "=")
		if !ok || value == "" {
			continue // a bare key carries no value to remove
		}
		parts[i] = key + "=" + strippedValue
	}
	return strings.Join(parts, "&")
}
