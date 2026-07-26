package store

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// toolsListResult builds a tools/list result whose per-tool byte weights differ
// by an order of magnitude, which is the shape the feature exists to expose.
func toolsListResult(nextCursor string, tools ...string) string {
	cursor := ""
	if nextCursor != "" {
		cursor = fmt.Sprintf(`,"nextCursor":%q`, nextCursor)
	}
	body := ""
	for i, t := range tools {
		if i > 0 {
			body += ","
		}
		body += t
	}
	return `{"tools":[` + body + `]` + cursor + `}`
}

func listExchange(s *Store, seq uint64, cursor, result string) {
	now := time.Now()
	params := "{}"
	if cursor != "" {
		params = fmt.Sprintf(`{"cursor":%q}`, cursor)
	}
	s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: seq, TS: now, Direction: proxy.ClientToServer,
		Raw: json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/list","params":%s}`, seq, params)),
	})
	s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: seq + 1, TS: now, Direction: proxy.ServerToClient,
		Raw: json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, seq, result)),
	})
}

const (
	fatTool  = `{"name":"search","description":"A very long description that goes on at some length about what this tool does and when the model ought to reach for it.","inputSchema":{"type":"object","properties":{"q":{"type":"string"},"limit":{"type":"integer"},"filters":{"type":"object"}}}}`
	thinTool = `{"name":"ping","description":"Ping.","inputSchema":{"type":"object"}}`
)

// TestToolCostsBreaksDownAndSortsHeaviestFirst is the core claim: the total is
// the sum of the definitions as sent, and the order answers "which tools is
// this cost actually in" rather than "what are they called".
func TestToolCostsBreaksDownAndSortsHeaviestFirst(t *testing.T) {
	s := New()
	listExchange(s, 1, "", toolsListResult("", thinTool, fatTool))

	cost, ok := s.ToolCosts("s1")
	if !ok {
		t.Fatal("a tools/list response should produce a cost report")
	}
	if !cost.Complete {
		t.Fatal("a cursorless response with no nextCursor is a complete list")
	}
	if cost.Tools != 2 || len(cost.PerTool) != 2 {
		t.Fatalf("tools = %d, per-tool = %d, want 2 and 2", cost.Tools, len(cost.PerTool))
	}
	if cost.PerTool[0].Name != "search" {
		t.Fatalf("heaviest tool should lead, got %q", cost.PerTool[0].Name)
	}
	if want := len(fatTool) + len(thinTool); cost.Bytes != want {
		t.Fatalf("total = %d bytes, want %d, the definitions exactly as sent", cost.Bytes, want)
	}

	fat := cost.PerTool[0]
	if fat.Bytes <= cost.PerTool[1].Bytes {
		t.Fatalf("the fat tool should outweigh the thin one: %d vs %d", fat.Bytes, cost.PerTool[1].Bytes)
	}
	// Description and schema are separable, since the two are fixed by different
	// means and a caller has to be able to tell which one to go after.
	if fat.DescriptionBytes == 0 || fat.SchemaBytes == 0 {
		t.Fatalf("both components should be measured, got %+v", fat)
	}
	if fat.DescriptionBytes >= fat.Bytes || fat.SchemaBytes >= fat.Bytes {
		t.Fatalf("a component cannot exceed the whole definition: %+v", fat)
	}
}

// TestToolCostsAreWhitespaceInsensitive is the point of measuring the compacted
// definition: the same tool weighs the same whether a server pretty-prints its
// tools/list or sends it compact, so two captures stay comparable and the number
// is not inflated by formatting the client would strip before the model sees it.
func TestToolCostsAreWhitespaceInsensitive(t *testing.T) {
	pretty := New()
	// The same definition as thinTool, indented the way a server might send it.
	listExchange(pretty, 1, "", `{
		"tools": [
			{
				"name": "ping",
				"description": "Ping.",
				"inputSchema": { "type": "object" }
			}
		]
	}`)

	compact := New()
	listExchange(compact, 1, "", toolsListResult("", thinTool))

	pc, ok1 := pretty.ToolCosts("s1")
	cc, ok2 := compact.ToolCosts("s1")
	if !ok1 || !ok2 {
		t.Fatal("both sessions listed a tool")
	}
	if pc.Bytes != cc.Bytes || pc.Bytes != len(thinTool) {
		t.Fatalf("pretty %d and compact %d should both equal the compact definition %d", pc.Bytes, cc.Bytes, len(thinTool))
	}
	if pc.PerTool[0].SchemaBytes != cc.PerTool[0].SchemaBytes {
		t.Fatalf("schema bytes must be whitespace insensitive: %d vs %d", pc.PerTool[0].SchemaBytes, cc.PerTool[0].SchemaBytes)
	}
}

// TestToolCostsPaginatedListIsNotComplete locks the one way this number could
// mislead: a list still paginating has a floor, not a total.
func TestToolCostsPaginatedListIsNotComplete(t *testing.T) {
	s := New()
	listExchange(s, 1, "", toolsListResult("page2", fatTool))

	cost, ok := s.ToolCosts("s1")
	if !ok {
		t.Fatal("a first page is still an observed tools/list")
	}
	if cost.Complete {
		t.Fatal("a response carrying nextCursor is not a complete list")
	}
	partial := cost.Bytes
	if partial != len(fatTool) {
		t.Fatalf("first page = %d bytes, want %d", partial, len(fatTool))
	}

	// The continuation extends the set rather than replacing it, so the total
	// grows and only then becomes final.
	listExchange(s, 3, "page2", toolsListResult("", thinTool))
	cost, _ = s.ToolCosts("s1")
	if !cost.Complete {
		t.Fatal("the last page with no nextCursor completes the list")
	}
	if cost.Tools != 2 || cost.Bytes != len(fatTool)+len(thinTool) {
		t.Fatalf("after pagination: %d tools, %d bytes", cost.Tools, cost.Bytes)
	}
}

// TestToolCostsAbsentWithoutAToolsList separates "never listed" from "listed
// nothing". Reporting 0 bytes for a session that never asked would read as a
// free server rather than as an unmeasured one.
func TestToolCostsAbsentWithoutAToolsList(t *testing.T) {
	s := New()
	s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: time.Now(), Direction: proxy.ClientToServer,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search"}}`),
	})
	if _, ok := s.ToolCosts("s1"); ok {
		t.Fatal("a session that never listed tools has no definition cost to report")
	}

	// A server that advertises no tools did list, and reports an honest zero.
	empty := New()
	listExchange(empty, 1, "", toolsListResult(""))
	cost, ok := empty.ToolCosts("s1")
	if !ok || !cost.Complete || cost.Tools != 0 || cost.Bytes != 0 {
		t.Fatalf("an empty but complete list should report zero: ok=%v %+v", ok, cost)
	}
}

// TestToolCostsAbsentDescriptionIsZero locks the empty case: a tool with no
// description field weighs zero description bytes, not the two an empty JSON
// string occupies, so an export never implies a description that is not there.
// The rest of the definition is still measured.
func TestToolCostsAbsentDescriptionIsZero(t *testing.T) {
	s := New()
	listExchange(s, 1, "", `{"tools":[{"name":"ping","inputSchema":{"type":"object"}}]}`)

	cost, ok := s.ToolCosts("s1")
	if !ok || len(cost.PerTool) != 1 {
		t.Fatalf("one tool expected, got ok=%v %+v", ok, cost)
	}
	tool := cost.PerTool[0]
	if tool.DescriptionBytes != 0 {
		t.Fatalf("a tool with no description should weigh 0 description bytes, got %d", tool.DescriptionBytes)
	}
	if tool.Bytes == 0 || tool.SchemaBytes == 0 {
		t.Fatalf("the rest of the definition is still measured: %+v", tool)
	}
}

// TestApplyToolsListSkipsOnlyTheMalformedEntry locks the decoding change. The
// old typed decode discarded every tool when any element was not an object.
func TestApplyToolsListSkipsOnlyTheMalformedEntry(t *testing.T) {
	s := New()
	listExchange(s, 1, "", `{"tools":[`+thinTool+`,42,`+fatTool+`]}`)

	cost, ok := s.ToolCosts("s1")
	if !ok || cost.Tools != 2 {
		t.Fatalf("the two well-formed tools should survive a bad sibling: ok=%v %+v", ok, cost)
	}
}

// TestToolSummaryReportsResultBytes covers the per-call half: a total and the
// worst case, since one huge answer among many small ones vanishes into a mean.
func TestToolSummaryReportsResultBytes(t *testing.T) {
	s := New()
	now := time.Now()
	small := `{"content":[{"type":"text","text":"ok"}]}`
	large := `{"content":[{"type":"text","text":"` + repeat("x", 4096) + `"}]}`

	call := func(seq uint64, id int, result string) {
		s.Ingest(proxy.Envelope{
			SessionID: "s1", ServerLabel: "srv", Seq: seq, TS: now, Direction: proxy.ClientToServer,
			Raw: json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"search"}}`, id)),
		})
		s.Ingest(proxy.Envelope{
			SessionID: "s1", ServerLabel: "srv", Seq: seq + 1, TS: now.Add(time.Millisecond), Direction: proxy.ServerToClient,
			Raw: json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, id, result)),
		})
	}
	call(1, 1, small)
	call(3, 2, large)

	summary, ok := s.ToolSummary("s1")
	if !ok || len(summary.Tools) != 1 {
		t.Fatalf("expected one tool in the summary, got ok=%v %+v", ok, summary.Tools)
	}
	tool := summary.Tools[0]
	if want := int64(len(small) + len(large)); tool.ResultBytes != want {
		t.Fatalf("result bytes = %d, want %d", tool.ResultBytes, want)
	}
	if tool.MaxResultBytes != len(large) {
		t.Fatalf("worst case = %d, want the large result's %d", tool.MaxResultBytes, len(large))
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for range n {
		out = append(out, s...)
	}
	return string(out)
}
