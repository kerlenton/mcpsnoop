package toolbaseline

import (
	"encoding/json"
	"os"
	"sync"
	"testing"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

func TestObserveCreatesBaselineThenDetectsDefinitionDrift(t *testing.T) {
	m := New(t.TempDir())
	baseline := []store.ToolDefinition{
		{Name: "search", Description: "Search docs", InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`)},
		{Name: "fetch", Description: "Fetch a page", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}

	report, created, err := m.Observe("docs", baseline)
	if err != nil {
		t.Fatal(err)
	}
	if !created || !report.Empty() {
		t.Fatalf("first observation = report %+v, created %v", report, created)
	}

	changed := []store.ToolDefinition{
		{Name: "search", Description: "Search every private document", InputSchema: json.RawMessage(`{"properties":{"query":{"minLength":1,"type":"string"}},"type":"object"}`)},
		{Name: "summarize", Description: "Summarize", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	report, created, err = m.Observe("docs", changed)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("existing baseline was recreated")
	}
	if !equalStrings(report.AddedTools, []string{"summarize"}) ||
		!equalStrings(report.RemovedTools, []string{"fetch"}) ||
		!equalStrings(report.ChangedDescriptions, []string{"search"}) ||
		!equalStrings(report.ChangedSchemas, []string{"search"}) {
		t.Fatalf("drift report = %+v", report)
	}
}

func TestConcurrentFirstObservationDoesNotOverwriteTrustedDefinition(t *testing.T) {
	dir := t.TempDir()
	managers := []*Manager{New(dir), New(dir)}
	definitions := [][]store.ToolDefinition{
		{{Name: "search", Description: "first", InputSchema: json.RawMessage(`{}`)}},
		{{Name: "search", Description: "second", InputSchema: json.RawMessage(`{}`)}},
	}
	type result struct {
		report  Report
		created bool
		err     error
	}
	results := make([]result, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range managers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i].report, results[i].created, results[i].err = managers[i].Observe("docs", definitions[i])
		}(i)
	}
	close(start)
	wg.Wait()

	created := 0
	drifted := 0
	for _, result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.created {
			created++
		}
		if len(result.report.ChangedDescriptions) == 1 {
			drifted++
		}
	}
	if created != 1 || drifted != 1 {
		t.Fatalf("results = %+v, want one creator and one drift comparison", results)
	}
}

func TestEquivalentSchemaKeyOrderDoesNotDrift(t *testing.T) {
	m := New(t.TempDir())
	before := []store.ToolDefinition{{Name: "search", InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)}}
	after := []store.ToolDefinition{{Name: "search", InputSchema: json.RawMessage(`{"properties":{"q":{"type":"string"}},"type":"object"}`)}}
	if _, _, err := m.Observe("docs", before); err != nil {
		t.Fatal(err)
	}
	report, _, err := m.Observe("docs", after)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Empty() {
		t.Fatalf("equivalent schema drifted: %+v", report)
	}
}

func TestObserveClassifiesDescriptionAndSchemaDriftSeparately(t *testing.T) {
	baseline := []store.ToolDefinition{{
		Name: "search", Description: "Search docs",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}}
	for _, test := range []struct {
		name    string
		current []store.ToolDefinition
		want    Report
	}{
		{
			name: "description only",
			current: []store.ToolDefinition{{
				Name: "search", Description: "Search private docs",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			}},
			want: Report{ChangedDescriptions: []string{"search"}},
		},
		{
			name: "schema only",
			current: []store.ToolDefinition{{
				Name: "search", Description: "Search docs",
				InputSchema: json.RawMessage(`{"type":"object","required":["query"]}`),
			}},
			want: Report{ChangedSchemas: []string{"search"}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := New(t.TempDir())
			if _, _, err := m.Observe("docs", baseline); err != nil {
				t.Fatal(err)
			}
			report, _, err := m.Observe("docs", test.current)
			if err != nil {
				t.Fatal(err)
			}
			if !equalStrings(report.ChangedDescriptions, test.want.ChangedDescriptions) ||
				!equalStrings(report.ChangedSchemas, test.want.ChangedSchemas) || report.Count() != test.want.Count() {
				t.Fatalf("report = %+v, want %+v", report, test.want)
			}
		})
	}
}

func TestAcceptAndResetBaseline(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)
	initial := []store.ToolDefinition{{Name: "search", Description: "old", InputSchema: json.RawMessage(`{}`)}}
	current := []store.ToolDefinition{{Name: "search", Description: "new", InputSchema: json.RawMessage(`{}`)}}
	if _, _, err := m.Observe("docs", initial); err != nil {
		t.Fatal(err)
	}
	if err := m.Accept("docs", current); err != nil {
		t.Fatal(err)
	}
	report, _, err := m.Observe("docs", current)
	if err != nil || !report.Empty() {
		t.Fatalf("accepted baseline = report %+v, err %v", report, err)
	}
	path := m.Path("docs")
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("baseline mode = %v, err %v", info, err)
	}
	if err := m.Reset("docs"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("baseline still exists: %v", err)
	}
}

func TestResetSessionNeedsOnlyTheServerLabel(t *testing.T) {
	m := New(t.TempDir())
	if err := m.Accept("docs", []store.ToolDefinition{{Name: "search"}}); err != nil {
		t.Fatal(err)
	}
	st := store.New()
	st.Ingest(proxy.Envelope{SessionID: "s1", ServerLabel: "docs"})

	server, err := ResetSession(m, st, "s1")
	if err != nil || server != "docs" {
		t.Fatalf("ResetSession() = server %q, err %v", server, err)
	}
	if _, err := os.Stat(m.Path("docs")); !os.IsNotExist(err) {
		t.Fatalf("baseline still exists: %v", err)
	}
}

func TestObserveSessionAttachesDriftToTheStore(t *testing.T) {
	m := New(t.TempDir())
	trusted := storeWithDefinitions("Search docs")
	if _, created, err := ObserveSession(m, trusted, "s1"); err != nil || !created {
		t.Fatalf("trusted observation = created %v, err %v", created, err)
	}

	changed := storeWithDefinitions("Search private docs")
	report, created, err := ObserveSession(m, changed, "s1")
	if err != nil || created || len(report.ChangedDescriptions) != 1 {
		t.Fatalf("changed observation = report %+v, created %v, err %v", report, created, err)
	}
	attached, ok := changed.ToolDrift("s1")
	if !ok || !equalStrings(attached.ChangedDescriptions, []string{"search"}) {
		t.Fatalf("attached drift = %+v, ok %v", attached, ok)
	}
}

func storeWithDefinitions(description string) *store.Store {
	st := store.New()
	st.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "docs", Seq: 1, Direction: proxy.ClientToServer,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
	})
	result, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"result": map[string]any{"tools": []any{map[string]any{
			"name": "search", "description": description, "inputSchema": map[string]any{"type": "object"},
		}}},
	})
	st.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "docs", Seq: 2, Direction: proxy.ServerToClient, Raw: result,
	})
	return st
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
