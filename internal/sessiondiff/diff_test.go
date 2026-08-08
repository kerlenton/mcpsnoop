package sessiondiff

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/exporter"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

func TestCompareReportsToolCallAndDurationChanges(t *testing.T) {
	beforeDuration := 100.0
	afterDuration := 350.0
	before := exporter.SessionExport{
		Session: exporter.SessionSummary{ID: "before"},
		Calls: []exporter.CallExport{
			listCall(`{"tools":[{"name":"search","description":"Search docs","inputSchema":{"type":"object","properties":{"query":{"type":"string"}}}},{"name":"old","inputSchema":{}}]}`),
			toolCall("search", `{"name":"search","arguments":{"limit":5,"query":"ruff"}}`, "ok", &beforeDuration),
		},
	}
	after := exporter.SessionExport{
		Session: exporter.SessionSummary{ID: "after"},
		Calls: []exporter.CallExport{
			listCall(`{"tools":[{"name":"search","description":"Search private docs","inputSchema":{"properties":{"query":{"minLength":1,"type":"string"}},"type":"object"}},{"name":"summarize","inputSchema":{}}]}`),
			toolCall("search", `{"arguments":{"query":"ruff","limit":5},"name":"search"}`, "error", &afterDuration),
		},
	}

	report := Compare(before, after, Options{
		DurationThreshold: 100 * time.Millisecond,
		DurationRatio:     2,
	})

	if !slices.Equal(report.Tools.Names(store.DriftToolAdded), []string{"summarize"}) {
		t.Fatalf("added tools = %v", report.Tools.Names(store.DriftToolAdded))
	}
	if !slices.Equal(report.Tools.Names(store.DriftToolRemoved), []string{"old"}) {
		t.Fatalf("removed tools = %v", report.Tools.Names(store.DriftToolRemoved))
	}
	if !slices.Equal(report.Tools.Names(store.DriftDescription), []string{"search"}) {
		t.Fatalf("changed descriptions = %v", report.Tools.Names(store.DriftDescription))
	}
	if !slices.Equal(report.Tools.Names(store.DriftInputSchema), []string{"search"}) {
		t.Fatalf("changed schemas = %v", report.Tools.Names(store.DriftInputSchema))
	}
	if len(report.CallChanges) != 1 {
		t.Fatalf("call changes = %+v", report.CallChanges)
	}
	if got := report.CallChanges[0]; got.ToolName != "search" || got.Arguments != `{"limit":5,"query":"ruff"}` || got.Before != "ok" || got.After != "error" {
		t.Fatalf("call change = %+v", got)
	}
	if len(report.DurationChanges) != 1 {
		t.Fatalf("duration changes = %+v", report.DurationChanges)
	}
	if got := report.DurationChanges[0]; got.Before != 100*time.Millisecond || got.After != 350*time.Millisecond {
		t.Fatalf("duration change = %+v", got)
	}
}

func TestCompareToolDefinitionsDetectsDescriptionOnlyChanges(t *testing.T) {
	before := []ToolDefinition{{
		Name: "search", Description: "Search docs",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
	}}
	after := []ToolDefinition{{
		Name: "search", Description: "Search private docs",
		InputSchema: json.RawMessage(`{"properties":{"q":{"type":"string"}},"type":"object"}`),
	}}

	changes := CompareToolDefinitions(before, after)
	if !slices.Equal(changes.Names(store.DriftDescription), []string{"search"}) || len(changes.Names(store.DriftInputSchema)) != 0 {
		t.Fatalf("tool changes = %+v", changes)
	}
}

func TestCompareUsesLatestCompleteToolListing(t *testing.T) {
	before := exporter.SessionExport{
		Session: exporter.SessionSummary{ID: "before"},
		Calls: []exporter.CallExport{
			listCall(`{"tools":[{"name":"withdrawn","inputSchema":{}}]}`),
			listCall(`{"tools":[{"name":"page-one","inputSchema":{}}]}`),
			{
				Method: "tools/list",
				Params: json.RawMessage(`{"cursor":"next"}`),
				Result: json.RawMessage(`{"tools":[{"name":"page-two","inputSchema":{}}]}`),
			},
		},
	}
	after := exporter.SessionExport{
		Session: exporter.SessionSummary{ID: "after"},
		Calls: []exporter.CallExport{
			listCall(`{"tools":[{"name":"page-one","inputSchema":{}},{"name":"page-two","inputSchema":{}}]}`),
		},
	}

	report := Compare(before, after, Options{})
	if len(report.Tools.Names(store.DriftToolAdded)) != 0 || len(report.Tools.Names(store.DriftToolRemoved)) != 0 || len(report.Tools.Names(store.DriftInputSchema)) != 0 {
		t.Fatalf("tool changes = added %v, removed %v, schemas %v", report.Tools.Names(store.DriftToolAdded), report.Tools.Names(store.DriftToolRemoved), report.Tools.Names(store.DriftInputSchema))
	}
}

func TestWriteTextReportsNoDifferences(t *testing.T) {
	report := Compare(
		exporter.SessionExport{Session: exporter.SessionSummary{ID: "a"}},
		exporter.SessionExport{Session: exporter.SessionSummary{ID: "b"}},
		Options{},
	)
	var out strings.Builder
	if err := WriteText(&out, report); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "mcpsnoop diff a -> b\nno differences found\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestHasRegression(t *testing.T) {
	for _, c := range []struct {
		name   string
		report Report
		want   bool
	}{
		{"empty", Report{}, false},
		{"added tool only", Report{Tools: driftOf(store.DriftToolAdded, "x")}, false},
		{"removed tool", Report{Tools: driftOf(store.DriftToolRemoved, "x")}, true},
		{"description changed", Report{Tools: driftOf(store.DriftDescription, "x")}, true},
		{"schema changed", Report{Tools: driftOf(store.DriftInputSchema, "x")}, true},
		{"title changed", Report{Tools: driftOf(store.DriftTitle, "x")}, true},
		{"output schema changed", Report{Tools: driftOf(store.DriftOutputSchema, "x")}, true},
		{"annotations changed", Report{Tools: driftOf(store.DriftAnnotations, "x")}, true},
		// Icons change how a tool looks, not what it does or promises, so they are
		// drift but not a regression.
		{"icons changed", Report{Tools: driftOf(store.DriftIcons, "x")}, false},
		{"status worse", Report{CallChanges: []CallChange{{Before: "ok", After: "error"}}}, true},
		{"status better", Report{CallChanges: []CallChange{{Before: "error", After: "ok"}}}, false},
		{"slower", Report{DurationChanges: []DurationChange{{Before: time.Second, After: 2 * time.Second}}}, true},
		{"faster", Report{DurationChanges: []DurationChange{{Before: 2 * time.Second, After: time.Second}}}, false},
	} {
		if got := c.report.HasRegression(); got != c.want {
			t.Errorf("%s: HasRegression() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCanonicalJSONPreservesLargeIntegers(t *testing.T) {
	left := canonicalJSON(json.RawMessage(`{"id":9007199254740992}`))
	right := canonicalJSON(json.RawMessage(`{"id":9007199254740993}`))
	if left == right {
		t.Fatalf("distinct integers canonicalized to the same value: %s", left)
	}
}

func TestNotableDurationChangeRequiresAChange(t *testing.T) {
	if notableDurationChange(100*time.Millisecond, 100*time.Millisecond, Options{DurationRatio: 1}) {
		t.Fatal("identical durations reported as a change")
	}
	if !notableDurationChange(100*time.Millisecond, 101*time.Millisecond, Options{DurationRatio: 1}) {
		t.Fatal("non-zero duration change was not reported with open thresholds")
	}
}

func listCall(result string) exporter.CallExport {
	return exporter.CallExport{Method: "tools/list", Result: json.RawMessage(result)}
}

func toolCall(name, params, status string, durationMS *float64) exporter.CallExport {
	return exporter.CallExport{
		Method:     "tools/call",
		Status:     status,
		IsTool:     true,
		ToolName:   name,
		Params:     json.RawMessage(params),
		DurationMS: durationMS,
	}
}

// driftOf builds a one-entry drift report for a table case.
func driftOf(kind store.ToolDriftKind, name string) store.ToolDrift {
	var d store.ToolDrift
	d.Add(kind, name)
	return d
}

// TestStatusRankCoversCancellationAndLateResults. A run that goes from ok to a
// call the client gave up on, or to a result that arrived after it did, is a
// regression. Ranking the two new statuses with ok made diff print the change and
// still exit 0, so the line a human reads and the code a CI job reads disagreed.
func TestStatusRankCoversCancellationAndLateResults(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   int
	}{
		{"ok", 0},
		{"pending", 1},
		{"call_cancelled", 1},
		{"late_result", 1},
		{"error", 2},
	} {
		if got := statusRank(tc.status); got != tc.want {
			t.Errorf("statusRank(%q) = %d, want %d", tc.status, got, tc.want)
		}
	}
	if statusRank("call_cancelled") <= statusRank("ok") {
		t.Fatal("a cancelled call must rank worse than ok, or the exit code stays 0")
	}
}
