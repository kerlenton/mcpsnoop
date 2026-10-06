package store

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// TestTypeViolationsWarnOnTheListingThatCarriedThem. Each value below costs a
// client built on an official SDK the whole listing, so the frame that carried it
// is where the reader has to be told, naming the value and what it should have
// been. mcpsnoop itself keeps every tool it can read, which is the gap the
// warning closes.
func TestTypeViolationsWarnOnTheListingThatCarriedThem(t *testing.T) {
	s := New()
	ingestFrames(t, s, [][3]any{
		{proxy.ClientToServer, 0, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`},
		{proxy.ServerToClient, 5, `{"jsonrpc":"2.0","id":1,"result":{"tools":[` +
			`{"name":"ok","inputSchema":{"type":"object"}},` +
			`{"name":"labelled","title":42,"description":null,"inputSchema":{"type":"object"}},` +
			`{"name":"odd","inputSchema":{"type":"object"},"annotations":{"title":7,"readOnlyHint":"true","openWorldHint":null}},` +
			`{"name":"flat","inputSchema":{"type":"object"},"annotations":"read-only"},` +
			`"stray",` +
			`{"name":7,"inputSchema":{"type":"object"}},` +
			`{"inputSchema":{"type":"object"}}` +
			`],"nextCursor":3}}`},
	})
	warning := s.Timeline("demo")[1].Warning
	for _, want := range []string{
		"the result sends nextCursor as a number instead of a string",
		`tool "labelled" sends title as a number instead of a string`,
		`tool "labelled" sends description as null instead of a string`,
		`tool "odd" sends annotations.title as a number instead of a string`,
		`tool "odd" sends annotations.readOnlyHint as a string instead of a boolean`,
		`tool "odd" sends annotations.openWorldHint as null instead of a boolean`,
		`tool "flat" sends annotations as a string instead of an object`,
		"tools[4] is a string instead of a tool object",
		"tools[5] sends name as a number instead of a string",
		"tools[6] is missing the name every tool requires",
	} {
		if !strings.Contains(warning, want) {
			t.Errorf("warning does not say %q:\n%s", want, warning)
		}
	}
	if strings.Contains(warning, `tool "ok"`) {
		t.Errorf("a well-typed tool was reported:\n%s", warning)
	}

	// Every tool with a readable name is still listed, so the capture shows what
	// the client never got.
	report, ok := s.AnnotationFindings("demo")
	if !ok {
		t.Fatal("a listing with readable tools has an annotation report")
	}
	if got := report.Names(AnnotationsMissing); !slices.Equal(got, []string{"ok", "labelled"}) {
		t.Errorf("unannotated = %v, want [ok labelled]", got)
	}
	if got := report.Names(AnnotationsImplicit); !slices.Equal(got, []string{"odd"}) {
		t.Errorf("implicitHints = %v, want [odd]", got)
	}
	// The violation already failed the default gate as a warning, so counting it
	// again as an observation would report one value twice under two signals.
	if got := report.Names(AnnotationsMalformed); got != nil {
		t.Errorf("the observational report carries the violation: %v", got)
	}
	// A cursor nobody can follow leaves the listing open, so nothing is compared
	// against a baseline as if it were complete.
	if _, complete := s.ToolDefinitions("demo"); complete {
		t.Error("a listing whose cursor is not a string was taken as complete")
	}
}

// TestTypeViolationsHoldEachFieldFromItsOwnRevision. A session known to speak an
// older revision is not held to a field that revision never had. 2024-11-05 has
// no annotations and no tool title, 2025-03-26 added annotations, and 2025-06-18
// the title. description is in all of them.
func TestTypeViolationsHoldEachFieldFromItsOwnRevision(t *testing.T) {
	const listing = `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"t","title":1,"description":2,"inputSchema":{"type":"object"},"annotations":{"readOnlyHint":"yes"}}]}}`
	for _, tc := range []struct {
		revision string
		want     []bool // title, description, annotations
	}{
		{"2024-11-05", []bool{false, true, false}},
		{"2025-03-26", []bool{false, true, true}},
		{"2025-06-18", []bool{true, true, true}},
		{"2026-07-28", []bool{true, true, true}},
		// A capture that starts after the handshake gives no revision to excuse
		// anything with, and the types are the same in every revision that has them.
		{"", []bool{true, true, true}},
	} {
		t.Run("revision "+tc.revision, func(t *testing.T) {
			s := New()
			var frames [][3]any
			if tc.revision != "" {
				frames = append(frames,
					[3]any{proxy.ClientToServer, 0, `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"` + tc.revision + `","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`},
					[3]any{proxy.ServerToClient, 1, `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"` + tc.revision + `","capabilities":{"tools":{}},"serverInfo":{"name":"s","version":"1"}}}`})
			}
			frames = append(frames,
				[3]any{proxy.ClientToServer, 2, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`},
				[3]any{proxy.ServerToClient, 3, listing})
			ingestFrames(t, s, frames)
			events := s.Timeline("demo")
			warning := events[len(events)-1].Warning
			for i, field := range []string{"sends title as", "sends description as", "sends annotations.readOnlyHint as"} {
				if got := strings.Contains(warning, field); got != tc.want[i] {
					t.Errorf("%q reported = %v, want %v:\n%s", field, got, tc.want[i], warning)
				}
			}
		})
	}
}

// TestTypeViolationsLeaveMcpsnoopsOwnRedactionAlone. The placeholder is mcpsnoop's
// rewrite, so reporting it would turn the user's own redaction settings into an
// accusation against a server that sent booleans.
func TestTypeViolationsLeaveMcpsnoopsOwnRedactionAlone(t *testing.T) {
	s := New()
	for i, raw := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":1,"result":{"tools":[` +
			`{"name":"a","inputSchema":{"type":"object"},"annotations":"[REDACTED]"},` +
			`{"name":"b","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":"[REDACTED]","title":"[REDACTED]"}}` +
			`]}}`,
	} {
		dir := proxy.ClientToServer
		if i == 1 {
			dir = proxy.ServerToClient
		}
		s.Ingest(proxy.Envelope{SessionID: "demo", ServerLabel: "srv", Seq: uint64(i + 1), Direction: dir,
			Transport: proxy.TransportStdio, Raw: json.RawMessage(raw), Redacted: i == 1})
	}
	if warning := s.Timeline("demo")[1].Warning; warning != "" {
		t.Fatalf("a value mcpsnoop redacted was reported as the server's: %s", warning)
	}
}

func TestLastToolsPage(t *testing.T) {
	for raw, want := range map[string]bool{
		``: true, `null`: true, `""`: true, `"p2"`: false,
		// Not a cursor anyone can follow, so not proof that nothing follows.
		`3`: false, `{}`: false,
	} {
		if got := lastToolsPage(json.RawMessage(raw)); got != want {
			t.Errorf("lastToolsPage(%s) = %v, want %v", raw, got, want)
		}
	}
}

// TestTypeViolationsBelongToTheListingThatCarriedThem. The verdicts are parked
// while the listing is read and drained onto its response, so a later listing
// that is clean must not inherit them, and one whose tools are not an array at
// all must still say so, since no tool on it can be read.
func TestTypeViolationsBelongToTheListingThatCarriedThem(t *testing.T) {
	s := New()
	ingestFrames(t, s, [][3]any{
		{proxy.ClientToServer, 0, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`},
		{proxy.ServerToClient, 5, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"odd","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":"true"}}]}}`},
		{proxy.ClientToServer, 10, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`},
		{proxy.ServerToClient, 15, `{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"odd","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":true}}]}}`},
		{proxy.ClientToServer, 20, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`},
		{proxy.ServerToClient, 25, `{"jsonrpc":"2.0","id":3,"result":{"tools":{"odd":{}}}}`},
	})
	events := s.Timeline("demo")
	if !strings.Contains(events[1].Warning, "annotations.readOnlyHint as a string") {
		t.Fatalf("first listing = %q, want its own violation", events[1].Warning)
	}
	if events[3].Warning != "" {
		t.Fatalf("a clean listing inherited an earlier one's verdict: %q", events[3].Warning)
	}
	if !strings.Contains(events[5].Warning, "the result sends tools as an object instead of an array") {
		t.Fatalf("third listing = %q, want the tools array named", events[5].Warning)
	}
}
