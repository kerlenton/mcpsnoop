package store

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

func TestAnalyzeAnnotations(t *testing.T) {
	for _, tc := range []struct {
		name     string
		raw      string
		redacted bool
		want     []AnnotationFindingKind
	}{
		{"absent", ``, false, []AnnotationFindingKind{AnnotationsMissing}},
		{"null", `null`, false, []AnnotationFindingKind{AnnotationsMissing}},
		{"empty object", `{}`, false, []AnnotationFindingKind{AnnotationsMissing}},
		// title names the tool, it says nothing about what the tool does.
		{"title only", `{"title":"Search"}`, false, []AnnotationFindingKind{AnnotationsMissing}},
		{"read-only, the rest left to the defaults", `{"readOnlyHint":true}`, false, []AnnotationFindingKind{AnnotationsImplicit}},
		{"idempotent only", `{"idempotentHint":true}`, false, []AnnotationFindingKind{AnnotationsImplicit}},
		// destructiveHint false on a read-only tool is exactly what ChatGPT's review
		// asks for, so it must not read as a problem.
		{"the three ChatGPT asks for, read-only", `{"readOnlyHint":true,"destructiveHint":false,"openWorldHint":false}`, false, nil},
		{"all four, a write", `{"readOnlyHint":false,"destructiveHint":true,"idempotentHint":false,"openWorldHint":true}`, false, nil},
		{"read-only and destructive", `{"readOnlyHint":true,"destructiveHint":true,"openWorldHint":false}`, false, []AnnotationFindingKind{AnnotationsContradictory}},
		{"contradictory and implicit together", `{"readOnlyHint":true,"destructiveHint":true}`, false, []AnnotationFindingKind{AnnotationsImplicit, AnnotationsContradictory}},
		{"a string where a boolean belongs", `{"readOnlyHint":"true","destructiveHint":false,"openWorldHint":false}`, false, []AnnotationFindingKind{AnnotationsMalformed}},
		// The violation leads, and the rest is still read.
		{"a malformed hint that also leaves others out", `{"readOnlyHint":"true"}`, false, []AnnotationFindingKind{AnnotationsMalformed, AnnotationsImplicit}},
		{"a title that is not a string, and nothing else", `{"title":42}`, false, []AnnotationFindingKind{AnnotationsMalformed, AnnotationsMissing}},
		// json.Unmarshal takes null into a bool without complaint, which is the trap.
		{"null where a boolean belongs", `{"readOnlyHint":null,"destructiveHint":false,"openWorldHint":false}`, false, []AnnotationFindingKind{AnnotationsMalformed}},
		{"annotations as a string", `"read-only"`, false, []AnnotationFindingKind{AnnotationsMalformed}},
		{"annotations as an array", `[]`, false, []AnnotationFindingKind{AnnotationsMalformed}},
		{"mcpsnoop's own placeholder in a hint", `{"readOnlyHint":"[REDACTED]","destructiveHint":false,"openWorldHint":false}`, true, nil},
		{"the same placeholder on a frame that was not redacted", `{"readOnlyHint":"[REDACTED]","destructiveHint":false,"openWorldHint":false}`, false, []AnnotationFindingKind{AnnotationsMalformed}},
		{"mcpsnoop's own placeholder for the whole block", `"[REDACTED]"`, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := analyzeAnnotations(json.RawMessage(tc.raw), tc.redacted); !slices.Equal(got, tc.want) {
				t.Fatalf("analyzeAnnotations(%s) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestCompareToolHints(t *testing.T) {
	type change = HintChange
	for _, tc := range []struct {
		name            string
		before, after   string
		loose, tightOut []change
	}{
		{
			name: "a tool that declared nothing now claims read-only", before: ``, after: `{"readOnlyHint":true}`,
			loose: []change{{HintReadOnly, "false (default)", "true"}},
		},
		{
			name: "a read-only tool goes back to the defaults", before: `{"readOnlyHint":true}`, after: `{}`,
			tightOut: []change{{HintReadOnly, "true", "false (default)"}},
		},
		{
			name: "a write stops calling itself destructive", before: `{"readOnlyHint":false,"destructiveHint":true}`, after: `{"readOnlyHint":false,"destructiveHint":false}`,
			loose: []change{{HintDestructive, "true", "false"}},
		},
		{
			// The edit a server makes to pass ChatGPT's review. No client does anything
			// different with it, so calling it loosening would be a false alarm.
			name: "a read-only tool spells out destructiveHint false", before: `{"readOnlyHint":true}`, after: `{"readOnlyHint":true,"destructiveHint":false,"openWorldHint":true}`,
		},
		{
			name: "a read-only tool drops an explicit destructiveHint true", before: `{"readOnlyHint":true,"destructiveHint":true}`, after: `{"readOnlyHint":true}`,
			loose: []change{{HintDestructive, "true", "true (default)"}},
		},
		{
			name: "a read-only tool gains an explicit destructiveHint true", before: `{"readOnlyHint":true}`, after: `{"readOnlyHint":true,"destructiveHint":true}`,
			tightOut: []change{{HintDestructive, "true (default)", "true"}},
		},
		{
			name: "a tool narrows itself to a closed world", before: `{}`, after: `{"openWorldHint":false}`,
			loose: []change{{HintOpenWorld, "true (default)", "false"}},
		},
		{
			name: "a closed-world tool opens up", before: `{"openWorldHint":false}`, after: `{}`,
			tightOut: []change{{HintOpenWorld, "false", "true (default)"}},
		},
		{
			name: "a write becomes safe to retry", before: `{"readOnlyHint":false,"idempotentHint":false}`, after: `{"readOnlyHint":false,"idempotentHint":true}`,
			loose: []change{{HintIdempotent, "false", "true"}},
		},
		{
			name: "idempotentHint on a read-only tool is inert", before: `{"readOnlyHint":true,"idempotentHint":false}`, after: `{"readOnlyHint":true,"idempotentHint":true}`,
		},
		{
			// The flip is the change. The destructiveHint it wakes up belongs to it.
			name: "a read-only tool starts writing", before: `{"readOnlyHint":true,"destructiveHint":false}`, after: `{"readOnlyHint":false,"destructiveHint":false}`,
			tightOut: []change{{HintReadOnly, "true", "false"}},
		},
		{
			name: "one hint loosens while another tightens", before: `{"readOnlyHint":false,"destructiveHint":true,"openWorldHint":false}`, after: `{"readOnlyHint":false,"destructiveHint":false,"openWorldHint":true}`,
			loose:    []change{{HintDestructive, "true", "false"}},
			tightOut: []change{{HintOpenWorld, "false", "true"}},
		},
		{
			name: "a hint turns into a string", before: `{"readOnlyHint":false}`, after: `{"readOnlyHint":"true"}`,
			loose: []change{{HintReadOnly, "false", `malformed "true"`}},
		},
		{
			name: "the whole block turns into a string", before: `{"readOnlyHint":true}`, after: `"read-only"`,
			loose: []change{{"annotations", "an object", `malformed "read-only"`}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CompareToolHints(json.RawMessage(tc.before), json.RawMessage(tc.after))
			if !slices.Equal(got.Loosened, tc.loose) {
				t.Errorf("loosened = %v, want %v", got.Loosened, tc.loose)
			}
			if !slices.Equal(got.Tightened, tc.tightOut) {
				t.Errorf("tightened = %v, want %v", got.Tightened, tc.tightOut)
			}
		})
	}
}

func TestAnnotationFindingsReportsTheCompleteToolList(t *testing.T) {
	s := New()
	if _, ok := s.AnnotationFindings("demo"); ok {
		t.Fatal("a session that has not listed its tools has no annotation report")
	}
	ingestFrames(t, s, [][3]any{
		{proxy.ClientToServer, 0, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`},
		{proxy.ServerToClient, 5, `{"jsonrpc":"2.0","id":1,"result":{"tools":[` +
			`{"name":"bare","inputSchema":{"type":"object"}},` +
			`{"name":"search","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":true,"destructiveHint":false,"openWorldHint":true}},` +
			`{"name":"lookup","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":true}},` +
			`{"name":"wipe","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":true,"destructiveHint":true,"openWorldHint":false}},` +
			`{"name":"odd","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":"true","destructiveHint":false,"openWorldHint":false}}` +
			`]}}`},
	})
	report, ok := s.AnnotationFindings("demo")
	if !ok {
		t.Fatal("a listed session has an annotation report")
	}
	want := map[AnnotationFindingKind][]string{
		AnnotationsMissing:       {"bare"},
		AnnotationsImplicit:      {"lookup"},
		AnnotationsContradictory: {"wipe"},
	}
	for _, kind := range AnnotationFindingKinds {
		if got := report.Names(kind); !slices.Equal(got, want[kind]) {
			t.Errorf("%s = %v, want %v", kind, got, want[kind])
		}
	}
	if report.Count() != 3 || report.Empty() {
		t.Fatalf("count = %d, empty = %v", report.Count(), report.Empty())
	}
	// odd's violation is kept on its definition, where a renderer can show it,
	// and left out of the report the annotations signal counts.
	definitions, _ := s.ToolDefinitions("demo")
	if got := definitions[4].AnnotationFindings; !slices.Equal(got, []AnnotationFindingKind{AnnotationsMalformed}) {
		t.Fatalf("odd's findings = %v, want the violation alone", got)
	}
}

// TestRedactedAnnotationIsNotTheServersFault drives the placeholder through a
// real frame, since the redaction flag rides on the envelope rather than on the
// annotations themselves.
func TestRedactedAnnotationIsNotTheServersFault(t *testing.T) {
	s := New()
	for i, raw := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"t","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":"[REDACTED]","destructiveHint":false,"openWorldHint":false}}]}}`,
	} {
		dir := proxy.ClientToServer
		if i == 1 {
			dir = proxy.ServerToClient
		}
		s.Ingest(proxy.Envelope{SessionID: "demo", ServerLabel: "srv", Seq: uint64(i + 1), Direction: dir,
			Transport: proxy.TransportStdio, Raw: json.RawMessage(raw), Redacted: i == 1})
	}
	report, _ := s.AnnotationFindings("demo")
	if !report.Empty() {
		t.Fatalf("a hint mcpsnoop redacted itself was reported as the server's: %v", report.ByKind)
	}
}

func TestToolDriftShiftsSurviveTheStoreCopy(t *testing.T) {
	s := New()
	ingestFrames(t, s, [][3]any{{proxy.ClientToServer, 0, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`}})
	var drift ToolDrift
	drift.Add(DriftAnnotations, "t")
	drift.SetShift("t", AnnotationShift{Loosened: []HintChange{{HintReadOnly, "false", "true"}}})
	drift.SetShift("quiet", AnnotationShift{})
	// An edit no client acts on, a destructiveHint flip on a read-only tool say,
	// is drift with nothing to say about direction, and records no shift at all.
	if _, recorded := drift.Shifts["quiet"]; recorded {
		t.Fatal("a shift that moved nothing was recorded")
	}
	s.SetToolDrift("demo", drift)

	got, _ := s.ToolDrift("demo")
	if names := got.LoosenedNames(); !slices.Equal(names, []string{"t"}) {
		t.Fatalf("loosened = %v, want [t] and nothing for a shift that moved nothing", names)
	}
	got.Shifts["t"].Loosened[0].To = "tampered"
	again, _ := s.ToolDrift("demo")
	if again.Shifts["t"].Loosened[0].To != "true" {
		t.Fatal("a caller's copy of the drift reached into the store's own")
	}
}

// TestToolReadersCannotReachTheStoresFindings. ToolDefinitions and ToolCosts hand
// out copies, and a copy that shared its finding slices with the store let a
// reader rewrite what every later reader would see.
func TestToolReadersCannotReachTheStoresFindings(t *testing.T) {
	s := New()
	ingestFrames(t, s, [][3]any{
		{proxy.ClientToServer, 0, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`},
		{proxy.ServerToClient, 5, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"bare","inputSchema":{"type":"object","properties":{"q":{"oneOf":[{"type":"string"}]}}}}]}}`},
	})
	definitions, _ := s.ToolDefinitions("demo")
	definitions[0].AnnotationFindings[0] = "tampered"
	definitions[0].Cost.AnnotationKinds[0] = "tampered"
	definitions[0].Cost.FindingKinds[0] = "tampered"
	costs, _ := s.ToolCosts("demo")
	costs.PerTool[0].AnnotationKinds[0] = "tampered"
	costs.PerTool[0].FindingKinds[0] = "tampered"

	again, _ := s.ToolDefinitions("demo")
	if again[0].AnnotationFindings[0] != AnnotationsMissing || again[0].Cost.AnnotationKinds[0] != AnnotationsMissing ||
		again[0].Cost.FindingKinds[0] != FindingOneOf {
		t.Fatalf("a reader's copy reached the store: %+v", again[0])
	}
}
