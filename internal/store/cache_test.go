package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

const cacheMeta = `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}`

func TestParseCacheHints(t *testing.T) {
	t.Parallel()
	hint, ok := parseCacheHints(json.RawMessage(`{"resultType":"complete","tools":[],"ttlMs":30000,"cacheScope":"private"}`))
	if !ok {
		t.Fatal("expected cache hints")
	}
	if hint.TTLMs != 30000 || hint.Scope != "private" {
		t.Fatalf("hint = %+v", hint)
	}
	if _, ok := parseCacheHints(json.RawMessage(`{"tools":[]}`)); ok {
		t.Fatal("absent fields should not report hints")
	}
	if _, ok := parseCacheHints(json.RawMessage(`{"ttlMs":0,"cacheScope":""}`)); ok {
		t.Fatal("zero ttl and empty scope should not report hints")
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
	if ev.CacheStaleRefetch != "" {
		t.Fatalf("response should not carry stale refetch observation: %q", ev.CacheStaleRefetch)
	}
}

func TestIngestSurfacesCacheHintsOnToolsListResponse(t *testing.T) {
	s := New()
	now := time.Now()
	s.Ingest(req(1, now, proxy.ClientToServer, "1", "tools/list", cacheMeta))
	ev := s.Ingest(resp(2, now, proxy.ServerToClient, "1", `"result":{"resultType":"complete","tools":[],"ttlMs":60000,"cacheScope":"private"}`))
	if ev.CacheHint.TTLMs != 60000 || ev.CacheHint.Scope != "private" {
		t.Fatalf("CacheHint = %+v", ev.CacheHint)
	}
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

func TestIngestWarnsPublicScopeOnUserSpecificRead(t *testing.T) {
	s := New()
	now := time.Now()
	s.Ingest(req(1, now, proxy.ClientToServer, "1", "resources/read", `{"uri":"file:///user/profile",`+cacheMeta[1:]))
	ev := s.Ingest(resp(2, now, proxy.ServerToClient, "1", `"result":{"resultType":"complete","contents":[{"uri":"file:///user/profile","text":"{\"userId\":\"123\"}"}],"cacheScope":"public"}`))
	if !strings.Contains(ev.Warning, "user-specific") {
		t.Fatalf("expected public scope warning, got %q", ev.Warning)
	}
}

func TestIngestIgnoresCacheHintsBeforeRevision(t *testing.T) {
	s := New()
	now := time.Now()
	s.Ingest(req(1, now, proxy.ClientToServer, "1", "tools/list", `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-11-25"}}`))
	ev := s.Ingest(resp(2, now, proxy.ServerToClient, "1", `"result":{"tools":[],"ttlMs":60000,"cacheScope":"private"}`))
	if ev.CacheHint.TTLMs != 0 || ev.CacheHint.Scope != "" {
		t.Fatalf("pre-2026-07-28 revision should not surface cache hints: %+v", ev.CacheHint)
	}
}
