package store

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// cacheRequiredFrom is the revision that added ttlMs and cacheScope (SEP-2549).
const cacheRequiredFrom = "2026-07-28"

type cacheEntry struct {
	receivedAt time.Time
	ttlMs      int
}

// CacheHint surfaces ttlMs and cacheScope from a cacheable MCP result.
//
// TTLPresent separates a server that declared 0, which the spec gives a meaning
// of its own (immediately stale), from one that declared nothing, which on this
// revision is a violation. Collapsing the two into a zero TTLMs made the
// omission unreportable.
type CacheHint struct {
	TTLMs      int
	TTLPresent bool
	Scope      string
}

// Empty reports whether a result carried no caching hint at all.
func (h CacheHint) Empty() bool { return !h.TTLPresent && h.Scope == "" }

func cacheableMethod(method string) bool {
	switch method {
	case "server/discover", "tools/list", "prompts/list",
		"resources/list", "resources/templates/list", "resources/read":
		return true
	default:
		return false
	}
}

// paginatedListMethod reports whether a cacheable method returns pages, which is
// the set the one-scope-per-run rule applies to.
func paginatedListMethod(method string) bool {
	switch method {
	case "tools/list", "prompts/list", "resources/list", "resources/templates/list":
		return true
	default:
		return false
	}
}

func cacheKey(method string, params json.RawMessage) string {
	switch method {
	case "tools/list", "prompts/list", "resources/list", "resources/templates/list":
		var p struct {
			Cursor string `json:"cursor"`
		}
		_ = json.Unmarshal(params, &p)
		return method + "\x00" + p.Cursor
	case "resources/read":
		var p struct {
			URI string `json:"uri"`
		}
		_ = json.Unmarshal(params, &p)
		return method + "\x00" + p.URI
	case "server/discover":
		return method
	default:
		return ""
	}
}

// parseCacheHints reads the two hint fields and reports what the values
// themselves violate.
//
// The fields are decoded one at a time, through the number grammar rather than
// straight into an int. Decoding both behind one struct meant a ttlMs that Go
// would not put in an int, such as the legal 60000.0 a server emits from a
// duration in seconds, failed the whole decode and silently took the cacheScope
// beside it away with every check that depended on it.
func parseCacheHints(result json.RawMessage) (CacheHint, []string) {
	var fields struct {
		TTLMs json.RawMessage `json:"ttlMs"`
		Scope json.RawMessage `json:"cacheScope"`
	}
	if len(result) == 0 || json.Unmarshal(result, &fields) != nil {
		return CacheHint{}, nil
	}
	var (
		hint     CacheHint
		warnings []string
	)
	if declared(fields.TTLMs) {
		switch ttl, ok := cacheInteger(fields.TTLMs); {
		case !ok:
			warnings = append(warnings, "ttlMs is "+string(fields.TTLMs)+", which is not an integer")
		case ttl < 0:
			hint.TTLMs, hint.TTLPresent = ttl, true
			warnings = append(warnings, "ttlMs is "+strconv.Itoa(ttl)+
				", and 2026-07-28 requires a value of zero or more")
		default:
			hint.TTLMs, hint.TTLPresent = ttl, true
		}
	}
	if declared(fields.Scope) {
		var scope string
		switch {
		case json.Unmarshal(fields.Scope, &scope) != nil:
			warnings = append(warnings, "cacheScope is "+string(fields.Scope)+", which is not a string")
		case scope == "":
			warnings = append(warnings, "cacheScope is empty, and 2026-07-28 defines only public and private")
		default:
			hint.Scope = scope
			if scope != "public" && scope != "private" {
				warnings = append(warnings, "cacheScope is "+strconv.Quote(scope)+
					", and 2026-07-28 defines only public and private")
			}
		}
	}
	return hint, warnings
}

func declared(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

// cacheInteger reads a JSON number that denotes an integer, accepting the
// spellings JSON permits for one. 60000.0 and 6e4 are the same value as 60000
// and satisfy the released schema, and a server generating them from a duration
// is conforming.
func cacheInteger(raw json.RawMessage) (int, bool) {
	parsed := parseMCPInteger(string(raw))
	if !parsed.integer || !parsed.safe {
		return 0, false
	}
	value := int(parsed.value)
	if int64(value) != parsed.value {
		return 0, false
	}
	return value, true
}

// resultTypeIs reports whether a result declares exactly the given resultType.
// Used rather than isCompleteCacheableResult where the distinction between a
// declared "complete" and an absent resultType matters, because an absent one
// already has a warning of its own on the same frame and one omission must not
// be reported twice.
func resultTypeIs(result json.RawMessage, want string) bool {
	var fields struct {
		ResultType string `json:"resultType"`
	}
	return json.Unmarshal(result, &fields) == nil && fields.ResultType == want
}

func isCompleteCacheableResult(result json.RawMessage) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(result, &fields) != nil {
		return false
	}
	raw, ok := fields["resultType"]
	if !ok {
		return true
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	return value == "complete"
}

func (sess *session) checkCacheStale(method string, params json.RawMessage, ts time.Time, protocolVersion string) string {
	if !atLeastRevision(protocolVersion, cacheRequiredFrom) {
		return ""
	}
	key := cacheKey(method, params)
	if key == "" {
		return ""
	}
	entry, ok := sess.cacheFresh[key]
	if !ok || entry.ttlMs <= 0 {
		return ""
	}
	deadline := entry.receivedAt.Add(time.Duration(entry.ttlMs) * time.Millisecond)
	if !ts.Before(deadline) {
		return ""
	}
	return "re-fetched inside declared ttlMs freshness window"
}

// invalidateCache drops the freshness entries a change notification makes stale.
// The spec says a relevant notification invalidates a cached response
// immediately, and its own diagram is a list_changed followed by a re-fetch, so
// without this the note fires on the flow the page draws as correct.
func (sess *session) invalidateCache(method string, params json.RawMessage) {
	var prefixes []string
	switch method {
	case "notifications/tools/list_changed":
		prefixes = []string{"tools/list\x00"}
	case "notifications/prompts/list_changed":
		prefixes = []string{"prompts/list\x00"}
	case "notifications/resources/list_changed":
		prefixes = []string{"resources/list\x00", "resources/templates/list\x00"}
	case "notifications/resources/updated":
		var p struct {
			URI string `json:"uri"`
		}
		if json.Unmarshal(params, &p) != nil || p.URI == "" {
			return
		}
		delete(sess.cacheFresh, "resources/read\x00"+p.URI)
		return
	default:
		return
	}
	// Every page of a run goes stale together, since the notification invalidates
	// the listing rather than one cursor of it.
	for key := range sess.cacheFresh {
		for _, prefix := range prefixes {
			if strings.HasPrefix(key, prefix) {
				delete(sess.cacheFresh, key)
			}
		}
	}
}

// checkCacheScopeAcrossPages enforces the one rule on this page that a wire
// proxy is better placed to check than any single client, since the proxy sees
// every page of a run. A cursorless request starts a new run and sets the scope
// the rest of it must repeat; keying on the method alone without that reset
// would report two independent single-page listings whose scope legitimately
// differs, which is traffic the spec permits.
func (sess *session) checkCacheScopeAcrossPages(c *call, scope string) string {
	if !paginatedListMethod(c.method) {
		return ""
	}
	if !hasListCursor(c.params) {
		if sess.cacheListScope == nil {
			sess.cacheListScope = make(map[string]string)
		}
		sess.cacheListScope[c.method] = scope
		return ""
	}
	first, ok := sess.cacheListScope[c.method]
	if !ok || first == "" || scope == "" || first == scope {
		return ""
	}
	return "cacheScope " + strconv.Quote(scope) + " on a later page of " + c.method +
		" disagrees with " + strconv.Quote(first) + " on the first, and 2026-07-28 requires one scope per list request"
}

func (sess *session) recordCacheFromResponse(c *call, result json.RawMessage, ts time.Time) (CacheHint, []string) {
	if c == nil || !cacheableMethod(c.method) {
		return CacheHint{}, nil
	}
	if !atLeastRevision(c.protocolVersion, cacheRequiredFrom) {
		return CacheHint{}, nil
	}
	if !isCompleteCacheableResult(result) {
		return CacheHint{}, nil
	}
	hint, warnings := parseCacheHints(result)
	// The spec makes the hints mandatory on a complete result of a cacheable
	// operation, so their absence is the flattest violation on the page. Reported
	// only when the result says "complete" outright: an absent resultType is its
	// own violation with its own warning on this very frame, and saying it twice
	// sends the reader after two problems where there is one.
	if !hint.TTLPresent && resultTypeIs(result, "complete") {
		warnings = append(warnings, "cacheable "+c.method+
			" result declares no ttlMs, which 2026-07-28 requires")
	}
	if note := sess.checkCacheScopeAcrossPages(c, hint.Scope); note != "" {
		warnings = append(warnings, note)
	}
	if hint.TTLMs > 0 {
		if sess.cacheFresh == nil {
			sess.cacheFresh = make(map[string]cacheEntry)
		}
		sess.cacheFresh[cacheKey(c.method, c.params)] = cacheEntry{receivedAt: ts, ttlMs: hint.TTLMs}
	}
	return hint, warnings
}
