package sessiondiff

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kerlenton/mcpsnoop/internal/exporter"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

func tool(name string, mutate func(*ToolDefinition)) ToolDefinition {
	d := ToolDefinition{
		Name:        name,
		Description: "Search docs",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
	if mutate != nil {
		mutate(&d)
	}
	return d
}

// driftKinds is the set of kinds a comparison reported, for assertions that care
// about which arm fired rather than about tool names.
func driftKinds(d store.ToolDrift) []store.ToolDriftKind {
	var kinds []store.ToolDriftKind
	for _, kind := range store.ToolDriftKinds {
		if len(d.Names(kind)) > 0 {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

// TestAnnotationDefaultsAreNotDrift. Every hint has a documented default, so a
// server that starts spelling one out has said nothing new. Comparing the raw
// JSON would report all three of these, and a routine server refactor would
// light up every tool at once.
func TestAnnotationDefaultsAreNotDrift(t *testing.T) {
	const allDefaults = `{"readOnlyHint":false,"destructiveHint":true,"idempotentHint":false,"openWorldHint":true}`
	for _, tc := range []struct{ name, before, after string }{
		{"absent against empty object", ``, `{}`},
		{"empty object against null", `{}`, `null`},
		{"absent against null", ``, `null`},
		{"absent against the spelled-out defaults", ``, allDefaults},
		{"title only against title plus defaults", `{"title":"Search"}`,
			`{"title":"Search","readOnlyHint":false,"destructiveHint":true,"idempotentHint":false,"openWorldHint":true}`},
		{"reordered keys", `{"readOnlyHint":true,"title":"S"}`, `{"title":"S","readOnlyHint":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := []ToolDefinition{tool("search", func(d *ToolDefinition) { d.Annotations = json.RawMessage(tc.before) })}
			after := []ToolDefinition{tool("search", func(d *ToolDefinition) { d.Annotations = json.RawMessage(tc.after) })}
			if got := CompareToolDefinitions(before, after); got.Count() != 0 {
				t.Fatalf("semantically identical annotations reported drift: %v", driftKinds(got))
			}
		})
	}
}

// TestAnnotationChangesAreDrift covers the rug-pull this feature exists for and
// the cases a defaults-aware comparison could wrongly swallow.
func TestAnnotationChangesAreDrift(t *testing.T) {
	for _, tc := range []struct{ name, before, after string }{
		// The headline case: a tool approved as read-only becomes destructive.
		{"readOnlyHint true to false", `{"readOnlyHint":true}`, `{"readOnlyHint":false}`},
		{"readOnlyHint true to absent", `{"readOnlyHint":true}`, `{}`},
		{"destructiveHint false to absent", `{"destructiveHint":false}`, `{}`},
		{"idempotentHint dropped", `{"idempotentHint":true}`, `{}`},
		{"openWorldHint narrowed", `{"openWorldHint":true}`, `{"openWorldHint":false}`},
		{"annotations title changed", `{"title":"Search"}`, `{"title":"Delete"}`},
		{"unknown key added", `{}`, `{"x-vendor":"whatever"}`},
		// A non-boolean must never resolve onto the default. A lenient client
		// renders "true" as read-only, so a baseline with the hint absent (default
		// false) matching this would hide the change entirely.
		{"invalid type against absent", `{}`, `{"readOnlyHint":"true"}`},
		{"invalid type against the same boolean", `{"readOnlyHint":false}`, `{"readOnlyHint":"false"}`},
		// A non-object value is not an absent one.
		{"object against string", `{}`, `"read-only"`},
		{"object against array", `{}`, `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := []ToolDefinition{tool("search", func(d *ToolDefinition) { d.Annotations = json.RawMessage(tc.before) })}
			after := []ToolDefinition{tool("search", func(d *ToolDefinition) { d.Annotations = json.RawMessage(tc.after) })}
			got := CompareToolDefinitions(before, after)
			if names := got.Names(store.DriftAnnotations); len(names) != 1 || names[0] != "search" {
				t.Fatalf("expected annotation drift on search, got %v", driftKinds(got))
			}
		})
	}
}

// TestRedundantSchemaDialectIsNotDrift. 2020-12 is the default for both schema
// fields, so declaring it explicitly changes nothing about how the schema is read.
func TestRedundantSchemaDialectIsNotDrift(t *testing.T) {
	const plain = `{"type":"object","properties":{"q":{"type":"string"}}}`
	for _, tc := range []struct {
		name      string
		after     string
		wantDrift bool
	}{
		{"dialect added", `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"q":{"type":"string"}}}`, false},
		{"dialect added with empty fragment", `{"$schema":"https://json-schema.org/draft/2020-12/schema#","type":"object","properties":{"q":{"type":"string"}}}`, false},
		// A different dialect really does change how the schema is interpreted.
		{"other dialect", `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"q":{"type":"string"}}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, field := range []struct {
				name string
				kind store.ToolDriftKind
				set  func(*ToolDefinition, string)
			}{
				{"inputSchema", store.DriftInputSchema, func(d *ToolDefinition, s string) { d.InputSchema = json.RawMessage(s) }},
				{"outputSchema", store.DriftOutputSchema, func(d *ToolDefinition, s string) { d.OutputSchema = json.RawMessage(s) }},
			} {
				t.Run(field.name, func(t *testing.T) {
					before := []ToolDefinition{tool("search", func(d *ToolDefinition) { field.set(d, plain) })}
					after := []ToolDefinition{tool("search", func(d *ToolDefinition) { field.set(d, tc.after) })}
					got := len(CompareToolDefinitions(before, after).Names(field.kind)) > 0
					if got != tc.wantDrift {
						t.Fatalf("drift = %v, want %v", got, tc.wantDrift)
					}
				})
			}
		})
	}
}

// TestOutputSchemaAbsentIsNotAnEmptySchema. Absent means the tool promises
// nothing about structuredContent; {} means it promises conforming output the
// client should validate. Collapsing them erases a real contract change.
func TestOutputSchemaAbsentIsNotAnEmptySchema(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after string
		wantDrift     bool
	}{
		{"absent against null", ``, `null`, false},
		{"absent against empty schema", ``, `{}`, true},
		{"schema removed", `{"type":"object"}`, ``, true},
		{"schema changed", `{"type":"object"}`, `{"type":"array"}`, true},
		{"key order only", `{"type":"object","title":"t"}`, `{"title":"t","type":"object"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := []ToolDefinition{tool("search", func(d *ToolDefinition) { d.OutputSchema = json.RawMessage(tc.before) })}
			after := []ToolDefinition{tool("search", func(d *ToolDefinition) { d.OutputSchema = json.RawMessage(tc.after) })}
			got := len(CompareToolDefinitions(before, after).Names(store.DriftOutputSchema)) > 0
			if got != tc.wantDrift {
				t.Fatalf("drift = %v, want %v", got, tc.wantDrift)
			}
		})
	}
}

// TestIconOrderIsDrift pins the deliberate answer. A consumer that takes the
// first usable icon fetches a different URL when two are swapped, and src is
// the field the spec warns can point off-domain or at a data: URI, so this must
// not be quietly normalised away by sorting.
func TestIconOrderIsDrift(t *testing.T) {
	const a = `[{"src":"https://a.example/i.png"},{"src":"https://b.example/i.png"}]`
	const swapped = `[{"src":"https://b.example/i.png"},{"src":"https://a.example/i.png"}]`
	before := []ToolDefinition{tool("search", func(d *ToolDefinition) { d.Icons = json.RawMessage(a) })}
	after := []ToolDefinition{tool("search", func(d *ToolDefinition) { d.Icons = json.RawMessage(swapped) })}
	if got := CompareToolDefinitions(before, after); len(got.Names(store.DriftIcons)) != 1 {
		t.Fatalf("swapping two icons should report drift, got %v", driftKinds(got))
	}
}

// TestTitleIsComparedOnItsOwn. The spec ranks title above annotations.title,
// so tracking annotations while ignoring title would leave the easier spoof
// open: add a title after approval and the displayed name changes with
// byte-identical annotations.
func TestTitleIsComparedOnItsOwn(t *testing.T) {
	before := []ToolDefinition{tool("search", func(d *ToolDefinition) {
		d.Annotations = json.RawMessage(`{"title":"Search docs"}`)
	})}
	after := []ToolDefinition{tool("search", func(d *ToolDefinition) {
		d.Title = "Delete everything"
		d.Annotations = json.RawMessage(`{"title":"Search docs"}`)
	})}
	got := CompareToolDefinitions(before, after)
	if len(got.Names(store.DriftTitle)) != 1 {
		t.Fatalf("a title appearing after approval is drift, got %v", driftKinds(got))
	}
	if len(got.Names(store.DriftAnnotations)) != 0 {
		t.Fatalf("annotations were untouched, got %v", driftKinds(got))
	}
}

// TestSkippedKindsAreNotCompared covers the migration gate at the comparison
// layer: a kind the trusted side never recorded is not evidence of anything.
func TestSkippedKindsAreNotCompared(t *testing.T) {
	before := []ToolDefinition{tool("search", nil)}
	after := []ToolDefinition{tool("search", func(d *ToolDefinition) {
		d.Title = "New"
		d.Annotations = json.RawMessage(`{"readOnlyHint":false}`)
		d.OutputSchema = json.RawMessage(`{"type":"object"}`)
		d.Icons = json.RawMessage(`[{"src":"https://a.example/i.png"}]`)
	})}
	skip := []store.ToolDriftKind{store.DriftTitle, store.DriftAnnotations, store.DriftOutputSchema, store.DriftIcons}
	got := CompareToolDefinitions(before, after, skip...)
	if got.Count() != 0 {
		t.Fatalf("skipped kinds must not report, got %v", driftKinds(got))
	}
	if len(got.Unverified) != len(skip) {
		t.Fatalf("skipped kinds should be reported as unverified, got %v", got.Unverified)
	}
	// The arms that were not skipped still work.
	changed := []ToolDefinition{tool("search", func(d *ToolDefinition) { d.Description = "Something else" })}
	if names := CompareToolDefinitions(before, changed, skip...).Names(store.DriftDescription); len(names) != 1 {
		t.Fatal("skipping the new kinds must not switch off the old ones")
	}
}

// TestListedToolsSurvivesAMalformedNeighbour. The page used to be decoded into a
// typed slice, so one tool with a non-string field discarded every well-formed
// tool beside it, and a one-sided failure inverted the whole report into
// removed-and-added. A tool that is advertised should be compared even when one
// of its fields is junk.
func TestListedToolsSurvivesAMalformedNeighbour(t *testing.T) {
	const page = `{"tools":[
		{"name":"first","description":"ok","inputSchema":{"type":"object"}},
		{"name":"broken","description":{"oops":1},"title":42,"inputSchema":{"type":"object"}},
		{"name":"third","description":"ok","inputSchema":{"type":"object"}}]}`

	got := listedTools(exporter.SessionExport{
		Session: exporter.SessionSummary{ID: "s"},
		Calls:   []exporter.CallExport{listCall(page)},
	})
	names := make([]string, 0, len(got))
	for _, d := range got {
		names = append(names, d.Name)
	}
	if len(names) != 3 {
		t.Fatalf("every advertised tool should survive a malformed neighbour, got %v", names)
	}
	for _, d := range got {
		if d.Name == "broken" && (d.Description != "" || d.Title != "") {
			t.Fatalf("a non-string field should read as absent, got %+v", d)
		}
	}
}

// TestDiffReportsAnnotationDriftLikeTheBaselineDoes. mcpsnoop diff and mcpsnoop
// baseline reach the comparison through different decode sites, so this is the
// test that catches one of them being left behind, which is the failure shape
// where baseline flags a rug pull that diff calls clean on the same capture.
func TestDiffReportsAnnotationDriftLikeTheBaselineDoes(t *testing.T) {
	list := func(annotations string) exporter.SessionExport {
		return exporter.SessionExport{
			Session: exporter.SessionSummary{ID: "s"},
			Calls: []exporter.CallExport{listCall(
				`{"tools":[{"name":"search","inputSchema":{"type":"object"},"annotations":` + annotations + `}]}`)},
		}
	}
	report := Compare(list(`{"readOnlyHint":true}`), list(`{"readOnlyHint":false}`), Options{})
	if names := report.Tools.Names(store.DriftAnnotations); len(names) != 1 || names[0] != "search" {
		t.Fatalf("diff should report the annotation flip, got %+v", report.Tools.Changes)
	}
	if !report.HasRegression() {
		t.Fatal("a tool becoming destructive after the fact is a regression")
	}
}

// TestWriteTextPrintsEveryDriftKind is the renderer guard. Count and the clone
// are complete by construction now, so this hand-written enumeration is the only
// one left that can silently omit a kind.
func TestWriteTextPrintsEveryDriftKind(t *testing.T) {
	var drift store.ToolDrift
	for _, kind := range store.ToolDriftKinds {
		drift.Add(kind, "tool-"+string(kind))
	}
	var buf bytes.Buffer
	if err := WriteText(&buf, Report{BeforeSession: "a", AfterSession: "b", Tools: drift}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range store.ToolDriftKinds {
		if !strings.Contains(buf.String(), "tool-"+string(kind)) {
			t.Fatalf("kind %q was counted but never printed:\n%s", kind, buf.String())
		}
	}
}
