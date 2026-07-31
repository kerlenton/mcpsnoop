package store

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

const cacheMeta = `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}`

// notif is req without an id, which is what makes a frame a notification.
func notif(seq uint64, ts time.Time, dir proxy.Direction, method, params string) proxy.Envelope {
	raw := `{"jsonrpc":"2.0","method":` + strconv.Quote(method) + `,"params":` + params + `}`
	return proxy.Envelope{SessionID: "s1", ServerLabel: "srv", Seq: seq, TS: ts, Direction: dir, Raw: json.RawMessage(raw)}
}

func TestParseCacheHints(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, result string
		wantTTL      int
		wantPresent  bool
		wantScope    string
		wantWarning  string
	}{
		{
			name:    "both fields",
			result:  `{"resultType":"complete","tools":[],"ttlMs":30000,"cacheScope":"private"}`,
			wantTTL: 30000, wantPresent: true, wantScope: "private",
		},
		{
			name:   "absent fields",
			result: `{"tools":[]}`,
		},
		// Zero is a value the spec gives a meaning to, immediately stale, so it has
		// to survive as a declared zero rather than read as nothing declared.
		{
			name:    "explicit zero is declared",
			result:  `{"ttlMs":0}`,
			wantTTL: 0, wantPresent: true,
		},
		{
			name:    "negative ttl is a violation and still reported",
			result:  `{"ttlMs":-5,"cacheScope":"private"}`,
			wantTTL: -5, wantPresent: true, wantScope: "private",
			wantWarning: "requires a value of zero or more",
		},
		// A legal JSON spelling of an integer. Decoding both fields behind one int
		// meant this failed the whole decode and took the cacheScope with it.
		{
			name:    "fractional spelling of an integer",
			result:  `{"ttlMs":60000.0,"cacheScope":"private"}`,
			wantTTL: 60000, wantPresent: true, wantScope: "private",
		},
		{
			name:    "exponent spelling of an integer",
			result:  `{"ttlMs":6e4,"cacheScope":"public"}`,
			wantTTL: 60000, wantPresent: true, wantScope: "public",
		},
		{
			name:        "ttl that is not an integer",
			result:      `{"ttlMs":"soon","cacheScope":"private"}`,
			wantScope:   "private",
			wantWarning: "not an integer",
		},
		{
			name:        "scope outside the two-value table",
			result:      `{"ttlMs":1000,"cacheScope":"semi-public"}`,
			wantTTL:     1000,
			wantPresent: true,
			wantScope:   "semi-public",
			wantWarning: "only public and private",
		},
		{
			name:        "empty scope",
			result:      `{"ttlMs":1000,"cacheScope":""}`,
			wantTTL:     1000,
			wantPresent: true,
			wantWarning: "only public and private",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hint, warnings := parseCacheHints(json.RawMessage(tc.result))
			if hint.TTLMs != tc.wantTTL || hint.TTLPresent != tc.wantPresent || hint.Scope != tc.wantScope {
				t.Fatalf("hint = %+v, want ttl=%d present=%v scope=%q",
					hint, tc.wantTTL, tc.wantPresent, tc.wantScope)
			}
			if tc.wantWarning == "" {
				if len(warnings) != 0 {
					t.Fatalf("warnings = %v, want none", warnings)
				}
				return
			}
			if len(warnings) != 1 || !strings.Contains(warnings[0], tc.wantWarning) {
				t.Fatalf("warnings = %v, want one containing %q", warnings, tc.wantWarning)
			}
		})
	}
}

func TestIngestSurfacesCacheHintsWithoutStaleRefetch(t *testing.T) {
	s := New()
	now := time.Now()
	s.Ingest(req(1, now, proxy.ClientToServer, "1", "tools/list", cacheMeta))
	ev := s.Ingest(resp(2, now, proxy.ServerToClient, "1", `"result":{"resultType":"complete","tools":[],"ttlMs":60000,"cacheScope":"private"}`))
	if ev.CacheHint.TTLMs != 60000 || ev.CacheHint.Scope != "private" {
		t.Fatalf("CacheHint = %+v", ev.CacheHint)
	}
	if ev.Warning != "" {
		t.Fatalf("a conforming cacheable result must be silent, got %q", ev.Warning)
	}
	if ev.CacheStaleRefetch != "" {
		t.Fatalf("response should not carry stale refetch observation: %q", ev.CacheStaleRefetch)
	}
}

// TestIngestWarnsOnAbsentCachingHints. The spec makes the hints mandatory on a
// complete result of a cacheable operation, which is the flattest violation on
// the page and the one a wire proxy sees for free.
func TestIngestWarnsOnAbsentCachingHints(t *testing.T) {
	for _, tc := range []struct {
		name, method, result string
		want                 bool
	}{
		{"complete with no ttlMs", "tools/list", `"result":{"resultType":"complete","tools":[]}`, true},
		{"complete with ttlMs", "tools/list", `"result":{"resultType":"complete","tools":[],"ttlMs":0}`, false},
		// An absent resultType is its own violation with its own warning on this
		// frame, so this one stays quiet rather than reporting the same omission
		// under a second name.
		{"no resultType at all", "tools/list", `"result":{"tools":[]}`, false},
		{"interim result carries no hints", "resources/read", `"result":{"resultType":"input_required","contents":[]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			now := time.Now()
			params := cacheMeta
			if tc.method == "resources/read" {
				params = `{"uri":"file:///a",` + cacheMeta[1:]
			}
			s.Ingest(req(1, now, proxy.ClientToServer, "1", tc.method, params))
			ev := s.Ingest(resp(2, now, proxy.ServerToClient, "1", tc.result))
			if got := strings.Contains(ev.Warning, "declares no ttlMs"); got != tc.want {
				t.Fatalf("warning = %q, want the missing-ttlMs report = %v", ev.Warning, tc.want)
			}
		})
	}
}

// TestIngestWarnsOnCacheScopeChangingMidRun is the one rule here a proxy is
// better placed to check than any client, because it sees every page of a run.
func TestIngestWarnsOnCacheScopeChangingMidRun(t *testing.T) {
	page := func(scope, cursor string) (string, string) {
		params := cacheMeta
		if cursor != "" {
			params = `{"cursor":` + strconv.Quote(cursor) + `,` + cacheMeta[1:]
		}
		return params, `"result":{"resultType":"complete","tools":[],"ttlMs":1000,"cacheScope":"` + scope + `"}`
	}

	t.Run("later page disagrees", func(t *testing.T) {
		s := New()
		now := time.Now()
		params, result := page("public", "")
		s.Ingest(req(1, now, proxy.ClientToServer, "1", "tools/list", params))
		s.Ingest(resp(2, now, proxy.ServerToClient, "1", result))
		params, result = page("private", "p2")
		s.Ingest(req(3, now, proxy.ClientToServer, "2", "tools/list", params))
		ev := s.Ingest(resp(4, now, proxy.ServerToClient, "2", result))
		if !strings.Contains(ev.Warning, "one scope per list request") {
			t.Fatalf("a scope change mid-run must be reported, got %q", ev.Warning)
		}
	})

	t.Run("later page agrees", func(t *testing.T) {
		s := New()
		now := time.Now()
		params, result := page("public", "")
		s.Ingest(req(1, now, proxy.ClientToServer, "1", "tools/list", params))
		s.Ingest(resp(2, now, proxy.ServerToClient, "1", result))
		params, result = page("public", "p2")
		s.Ingest(req(3, now, proxy.ClientToServer, "2", "tools/list", params))
		ev := s.Ingest(resp(4, now, proxy.ServerToClient, "2", result))
		if ev.Warning != "" {
			t.Fatalf("a run that keeps one scope must be silent, got %q", ev.Warning)
		}
	})

	// Two independent listings, each one page. The spec constrains one list
	// request, not the server forever, so a cursorless request has to start the
	// run over or this reports traffic the spec permits.
	t.Run("two independent listings may differ", func(t *testing.T) {
		s := New()
		now := time.Now()
		params, result := page("public", "")
		s.Ingest(req(1, now, proxy.ClientToServer, "1", "tools/list", params))
		s.Ingest(resp(2, now, proxy.ServerToClient, "1", result))
		params, result = page("private", "")
		s.Ingest(req(3, now, proxy.ClientToServer, "2", "tools/list", params))
		ev := s.Ingest(resp(4, now, proxy.ServerToClient, "2", result))
		if ev.Warning != "" {
			t.Fatalf("a fresh listing may declare its own scope, got %q", ev.Warning)
		}
	})
}

func TestIngestFlagsRefetchInsideTTL(t *testing.T) {
	s := New()
	t0 := time.Now()
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/list", cacheMeta))
	s.Ingest(resp(2, t0, proxy.ServerToClient, "1", `"result":{"resultType":"complete","tools":[],"ttlMs":60000,"cacheScope":"private"}`))

	t1 := t0.Add(10 * time.Second)
	ev := s.Ingest(req(3, t1, proxy.ClientToServer, "2", "tools/list", cacheMeta))
	if ev.CacheStaleRefetch == "" {
		t.Fatal("expected stale refetch observation")
	}
}

// TestIngestDoesNotFlagARefetchAfterListChanged. The spec says a relevant
// notification invalidates the cached response immediately, and the caching
// page's own diagram is a list_changed followed by exactly this re-fetch.
func TestIngestDoesNotFlagARefetchAfterListChanged(t *testing.T) {
	for _, tc := range []struct {
		name, notification, method, params string
		wantFlag                           bool
	}{
		{"tools", "notifications/tools/list_changed", "tools/list", cacheMeta, false},
		{"prompts", "notifications/prompts/list_changed", "prompts/list", cacheMeta, false},
		// A notification about something else leaves the entry fresh.
		{"unrelated", "notifications/prompts/list_changed", "tools/list", cacheMeta, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			t0 := time.Now()
			s.Ingest(req(1, t0, proxy.ClientToServer, "1", tc.method, tc.params))
			s.Ingest(resp(2, t0, proxy.ServerToClient, "1", `"result":{"resultType":"complete","ttlMs":60000,"cacheScope":"public"}`))
			s.Ingest(notif(3, t0.Add(time.Second), proxy.ServerToClient, tc.notification, `{}`))

			ev := s.Ingest(req(4, t0.Add(2*time.Second), proxy.ClientToServer, "2", tc.method, tc.params))
			if got := ev.CacheStaleRefetch != ""; got != tc.wantFlag {
				t.Fatalf("stale refetch = %q, want flagged = %v", ev.CacheStaleRefetch, tc.wantFlag)
			}
		})
	}
}

func TestIngestSilentAfterTTLExpires(t *testing.T) {
	s := New()
	t0 := time.Now()
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/list", cacheMeta))
	s.Ingest(resp(2, t0, proxy.ServerToClient, "1", `"result":{"resultType":"complete","tools":[],"ttlMs":1000}`))

	t1 := t0.Add(2 * time.Second)
	ev := s.Ingest(req(3, t1, proxy.ClientToServer, "2", "tools/list", cacheMeta))
	if ev.CacheStaleRefetch != "" {
		t.Fatalf("expected no stale refetch after ttl, got %q", ev.CacheStaleRefetch)
	}
}

func TestIngestCacheKeyIncludesCursor(t *testing.T) {
	s := New()
	t0 := time.Now()
	s.Ingest(req(1, t0, proxy.ClientToServer, "1", "tools/list", `{"cursor":"a",`+cacheMeta[1:]))
	s.Ingest(resp(2, t0, proxy.ServerToClient, "1", `"result":{"resultType":"complete","tools":[],"ttlMs":60000}`))

	t1 := t0.Add(time.Second)
	ev := s.Ingest(req(3, t1, proxy.ClientToServer, "2", "tools/list", `{"cursor":"b",`+cacheMeta[1:]))
	if ev.CacheStaleRefetch != "" {
		t.Fatalf("different cursor should be a separate cache key, got %q", ev.CacheStaleRefetch)
	}
}

// TestCacheChecksJudgeTheRequestsOwnRevision. Every check here is gated on the
// revision the request declared, never the session's, since a connection is not
// a session and a client may interleave revisions.
func TestIngestIgnoresCacheHintsBeforeRevision(t *testing.T) {
	s := New()
	now := time.Now()
	s.Ingest(req(1, now, proxy.ClientToServer, "1", "tools/list", `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-11-25"}}`))
	ev := s.Ingest(resp(2, now, proxy.ServerToClient, "1", `"result":{"tools":[],"ttlMs":-5,"cacheScope":"semi-public"}`))
	if !ev.CacheHint.Empty() {
		t.Fatalf("pre-2026-07-28 revision should not surface cache hints: %+v", ev.CacheHint)
	}
	if ev.Warning != "" {
		t.Fatalf("pre-2026-07-28 revision must not be judged by this revision's rules, got %q", ev.Warning)
	}
}
