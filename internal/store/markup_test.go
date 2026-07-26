package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// TestToolCostMeasuresTheDescriptionAsTheServerSpelledIt. Bytes is the compacted
// definition, so DescriptionBytes has to be the same bytes or the two are not
// comparable and the documented same-basis contract is false. Re-encoding a
// decoded Go string cannot do that in either direction: it escapes a literal <
// the server sent plain, and flattens one the server sent escaped. The markup
// case is also where the arithmetic goes visibly wrong, since a part measured
// six bytes per character can exceed the whole.
func TestToolCostMeasuresTheDescriptionAsTheServerSpelledIt(t *testing.T) {
	for _, tc := range []struct {
		name        string
		description string // exactly as it appears in the tools/list bytes
		want        int
	}{
		{"literal markup", `"Search & filter <query>"`, 25},
		{"all markup", `"<<<<<<<<<<"`, 12},
		// A server that escapes its own output really does spend those bytes, so
		// the measurement follows the wire rather than the decoded text.
		{"server-escaped markup", `"\u003c"`, 8},
		{"plain", `"Search docs"`, 13},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			raw := `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"x","description":` +
				tc.description + `,"inputSchema":{"type":"object"}}]}}`
			s.Ingest(proxy.Envelope{
				SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: time.Now(),
				Direction: proxy.ClientToServer, Transport: proxy.TransportStdio,
				Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
			})
			s.Ingest(proxy.Envelope{
				SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: time.Now(),
				Direction: proxy.ServerToClient, Transport: proxy.TransportStdio,
				Raw: json.RawMessage(raw),
			})

			defs, ok := s.ToolDefinitions("s1")
			if !ok || len(defs) != 1 {
				t.Fatalf("expected one tool definition, got %v %v", defs, ok)
			}
			cost := defs[0].Cost
			if cost.DescriptionBytes != tc.want {
				t.Fatalf("DescriptionBytes = %d, want %d (the %d bytes it occupies in the definition)",
					cost.DescriptionBytes, tc.want, tc.want)
			}
			if cost.DescriptionBytes >= cost.Bytes {
				t.Fatalf("a component cannot exceed the whole definition: description %d of %d",
					cost.DescriptionBytes, cost.Bytes)
			}
		})
	}
}

// TestToolCostLeavesAnAbsentDescriptionAtZero keeps the rule the measurement has
// always had: no description is not the two bytes of an empty one.
func TestToolCostLeavesAnAbsentDescriptionAtZero(t *testing.T) {
	s := New()
	s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: time.Now(),
		Direction: proxy.ClientToServer, Transport: proxy.TransportStdio,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
	})
	s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: time.Now(),
		Direction: proxy.ServerToClient, Transport: proxy.TransportStdio,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"x","inputSchema":{}}]}}`),
	})
	defs, _ := s.ToolDefinitions("s1")
	if len(defs) != 1 || defs[0].Cost.DescriptionBytes != 0 {
		t.Fatalf("an absent description should measure 0, got %+v", defs)
	}
}

// TestToolWithAMalformedDescriptionStaysInTheInventory pins a behaviour change.
// Reading the description as raw bytes means a non-string one no longer fails the
// whole tool-level decode, where it used to drop the tool from the list along
// with its name and schema. A tool that is advertised should be visible even when
// one of its fields is junk, since it can still be called.
func TestToolWithAMalformedDescriptionStaysInTheInventory(t *testing.T) {
	s := New()
	s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 1, TS: time.Now(),
		Direction: proxy.ClientToServer, Transport: proxy.TransportStdio,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
	})
	s.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "srv", Seq: 2, TS: time.Now(),
		Direction: proxy.ServerToClient, Transport: proxy.TransportStdio,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"tools":[` +
			`{"name":"broken","description":{"oops":1},"inputSchema":{"type":"object"}},` +
			`{"name":"fine","description":"ok","inputSchema":{"type":"object"}}]}}`),
	})

	defs, ok := s.ToolDefinitions("s1")
	if !ok || len(defs) != 2 {
		t.Fatalf("both tools should be listed, got %d: %+v", len(defs), defs)
	}
	var broken ToolDefinition
	for _, d := range defs {
		if d.Name == "broken" {
			broken = d
		}
	}
	if broken.Name == "" {
		t.Fatalf("the tool with the malformed description went missing: %+v", defs)
	}
	if broken.Description != "" {
		t.Fatalf("a non-string description should read as absent, got %q", broken.Description)
	}
	// The bytes were still spent on the wire, so the cost still reports them.
	if broken.Cost.DescriptionBytes != len(`{"oops":1}`) {
		t.Fatalf("DescriptionBytes = %d, want %d", broken.Cost.DescriptionBytes, len(`{"oops":1}`))
	}
}
