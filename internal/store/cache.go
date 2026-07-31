package store

import (
	"encoding/json"
	"strings"
	"time"
)

// cacheRequiredFrom is the revision that added ttlMs and cacheScope (SEP-2549).
const cacheRequiredFrom = "2026-07-28"

type cacheEntry struct {
	receivedAt time.Time
	ttlMs      int
	scope      string
}

// CacheHint surfaces ttlMs and cacheScope from a cacheable MCP result.
type CacheHint struct {
	TTLMs int
	Scope string
}

func cacheableMethod(method string) bool {
	switch method {
	case "server/discover", "tools/list", "prompts/list",
		"resources/list", "resources/templates/list", "resources/read":
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

func parseCacheHints(result json.RawMessage) (CacheHint, bool) {
	if len(result) == 0 {
		return CacheHint{}, false
	}
	var fields struct {
		TTLMs *int    `json:"ttlMs"`
		Scope *string `json:"cacheScope"`
	}
	if json.Unmarshal(result, &fields) != nil {
		return CacheHint{}, false
	}
	hint := CacheHint{}
	found := false
	if fields.TTLMs != nil && *fields.TTLMs > 0 {
		hint.TTLMs = *fields.TTLMs
		found = true
	}
	if fields.Scope != nil && *fields.Scope != "" {
		hint.Scope = *fields.Scope
		found = true
	}
	return hint, found
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

func publicScopeMislabelWarning(method string, params, result json.RawMessage, scope string) string {
	if scope != "public" {
		return ""
	}
	if method != "resources/read" {
		return ""
	}
	var p struct {
		URI string `json:"uri"`
	}
	_ = json.Unmarshal(params, &p)
	uri := strings.ToLower(p.URI)
	for _, marker := range []string{"user", "profile", "account", "me/", "/me", "private", "session"} {
		if strings.Contains(uri, marker) {
			return "cacheScope is public on a resource URI that looks user-specific"
		}
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(result, &body) != nil {
		return ""
	}
	if contents, ok := body["contents"]; ok {
		var items []map[string]json.RawMessage
		if json.Unmarshal(contents, &items) == nil {
			for _, item := range items {
				if text, ok := item["text"]; ok {
					if looksUserSpecificJSON(text) {
						return "cacheScope is public on a resource that looks user-specific"
					}
				}
			}
		}
	}
	return ""
}

func looksUserSpecificJSON(raw json.RawMessage) bool {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return false
	}
	for key := range obj {
		lower := strings.ToLower(key)
		switch lower {
		case "userid", "user_id", "email", "accountid", "account_id",
			"sessionid", "session_id", "accesstoken", "access_token",
			"apitoken", "api_token", "password", "ssn":
			return true
		}
	}
	return false
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

func (sess *session) recordCacheFromResponse(c *call, result json.RawMessage, ts time.Time) (CacheHint, string) {
	if c == nil || !cacheableMethod(c.method) {
		return CacheHint{}, ""
	}
	if !atLeastRevision(c.protocolVersion, cacheRequiredFrom) {
		return CacheHint{}, ""
	}
	if !isCompleteCacheableResult(result) {
		return CacheHint{}, ""
	}
	hint, found := parseCacheHints(result)
	if !found {
		return CacheHint{}, ""
	}
	if hint.TTLMs > 0 {
		if sess.cacheFresh == nil {
			sess.cacheFresh = make(map[string]cacheEntry)
		}
		sess.cacheFresh[cacheKey(c.method, c.params)] = cacheEntry{
			receivedAt: ts,
			ttlMs:      hint.TTLMs,
			scope:      hint.Scope,
		}
	}
	warning := publicScopeMislabelWarning(c.method, c.params, result, hint.Scope)
	if hint.TTLMs > 0 || hint.Scope != "" {
		return hint, warning
	}
	return CacheHint{}, warning
}
