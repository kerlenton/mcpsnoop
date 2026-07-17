package sessiondiff

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/exporter"
)

func TestCompareReportsToolCallAndDurationChanges(t *testing.T) {
	beforeDuration := 100.0
	afterDuration := 350.0
	before := exporter.SessionExport{
		Session: exporter.SessionSummary{ID: "before"},
		Calls: []exporter.CallExport{
			listCall(`{"tools":[{"name":"search","inputSchema":{"type":"object","properties":{"query":{"type":"string"}}}},{"name":"old","inputSchema":{}}]}`),
			toolCall("search", `{"name":"search","arguments":{"limit":5,"query":"ruff"}}`, "ok", &beforeDuration),
		},
	}
	after := exporter.SessionExport{
		Session: exporter.SessionSummary{ID: "after"},
		Calls: []exporter.CallExport{
			listCall(`{"tools":[{"name":"search","inputSchema":{"properties":{"query":{"minLength":1,"type":"string"}},"type":"object"}},{"name":"summarize","inputSchema":{}}]}`),
			toolCall("search", `{"arguments":{"query":"ruff","limit":5},"name":"search"}`, "error", &afterDuration),
		},
	}

	report := Compare(before, after, Options{
		DurationThreshold: 100 * time.Millisecond,
		DurationRatio:     2,
	})

	if !slices.Equal(report.AddedTools, []string{"summarize"}) {
		t.Fatalf("added tools = %v", report.AddedTools)
	}
	if !slices.Equal(report.RemovedTools, []string{"old"}) {
		t.Fatalf("removed tools = %v", report.RemovedTools)
	}
	if !slices.Equal(report.ChangedSchemas, []string{"search"}) {
		t.Fatalf("changed schemas = %v", report.ChangedSchemas)
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
	if len(report.AddedTools) != 0 || len(report.RemovedTools) != 0 || len(report.ChangedSchemas) != 0 {
		t.Fatalf("tool changes = added %v, removed %v, schemas %v", report.AddedTools, report.RemovedTools, report.ChangedSchemas)
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
		{"added tool only", Report{AddedTools: []string{"x"}}, false},
		{"removed tool", Report{RemovedTools: []string{"x"}}, true},
		{"schema changed", Report{ChangedSchemas: []string{"x"}}, true},
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
