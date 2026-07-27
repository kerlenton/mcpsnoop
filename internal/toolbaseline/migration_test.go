package toolbaseline

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/store"
)

// writeV1Baseline writes the exact on-disk shape mcpsnoop wrote before it
// tracked title, outputSchema, annotations or icons. Hand-written rather than
// produced by this build, since the point is to read a file this build cannot
// have created.
func writeV1Baseline(t *testing.T, m *Manager, server, description string) {
	t.Helper()
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const shape = `{"version":1,"server":%q,"tools":[{"name":"search","description":%q,"input_schema":{"type":"object"}}]}`
	body := []byte(fmt.Sprintf(shape, server, description))
	if err := os.WriteFile(m.Path(server), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// sessionWithRichTool builds a session advertising a tool that carries every
// field a version 1 baseline has no record of.
func sessionWithRichTool(t *testing.T, description string) *store.Store {
	t.Helper()
	st := store.New()
	st.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "docs", Seq: 1, Direction: proxy.ClientToServer,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
	})
	result, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1,
		"result": map[string]any{"tools": []any{map[string]any{
			"name":         "search",
			"description":  description,
			"title":        "Search the docs",
			"inputSchema":  map[string]any{"type": "object"},
			"outputSchema": map[string]any{"type": "object"},
			"annotations":  map[string]any{"readOnlyHint": true},
			"icons":        []any{map[string]any{"src": "https://example.com/i.png"}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	st.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "docs", Seq: 2, Direction: proxy.ServerToClient, Raw: result,
	})
	return st
}

// TestVersion1BaselineDoesNotReportTheFieldsItNeverRecorded is the migration
// test, and the one that matters most in this change. A version 1 file has no
// annotations or outputSchema key by construction, so comparing them would
// report drift on every tool that has any, on the first run after upgrade,
// across every installed baseline at once. That false rug-pull alarm is worse
// than the gap it reports, because it is what teaches people to stop reading
// the signal.
func TestVersion1BaselineDoesNotReportTheFieldsItNeverRecorded(t *testing.T) {
	m := New(t.TempDir())
	writeV1Baseline(t, m, "docs", "Search docs")

	report, created, err := ObserveSession(m, sessionWithRichTool(t, "Search docs"), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("an existing version 1 baseline must be read, not replaced")
	}
	if report.BaselineError != "" {
		t.Fatalf("an older baseline is not an error: %q", report.BaselineError)
	}
	if report.Count() != 0 {
		t.Fatalf("nothing the baseline recorded changed, got %+v", report.Changes)
	}
	want := map[store.ToolDriftKind]bool{
		store.DriftTitle: true, store.DriftOutputSchema: true,
		store.DriftAnnotations: true, store.DriftIcons: true,
	}
	if len(report.Unverified) != len(want) {
		t.Fatalf("the uncovered fields should be named, got %v", report.Unverified)
	}
	for _, kind := range report.Unverified {
		if !want[kind] {
			t.Fatalf("unexpected unverified kind %q", kind)
		}
	}
}

// TestVersion1BaselineStillReportsWhatItDidRecord is the companion. The old
// anchor has to keep working, or the migration has quietly switched drift off.
func TestVersion1BaselineStillReportsWhatItDidRecord(t *testing.T) {
	m := New(t.TempDir())
	writeV1Baseline(t, m, "docs", "Search docs")

	report, _, err := ObserveSession(m, sessionWithRichTool(t, "Search every private document"), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if names := report.Names(store.DriftDescription); len(names) != 1 || names[0] != "search" {
		t.Fatalf("the description arm must still fire on a version 1 baseline, got %+v", report.Changes)
	}
}

// TestNewBaselinesRecordAndCompareTheNewFields closes the loop: a baseline this
// build wrote covers everything, so the gap is only ever about older files.
func TestNewBaselinesRecordAndCompareTheNewFields(t *testing.T) {
	m := New(t.TempDir())
	if _, created, err := ObserveSession(m, sessionWithRichTool(t, "Search docs"), "s1"); err != nil || !created {
		t.Fatalf("first observation = created %v, err %v", created, err)
	}

	stored, err := os.ReadFile(m.Path("docs"))
	if err != nil {
		t.Fatal(err)
	}
	var snap struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(stored, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Version != baselineVersion {
		t.Fatalf("a fresh baseline should be version %d, got %d", baselineVersion, snap.Version)
	}
	for _, key := range []string{"title", "output_schema", "annotations", "icons"} {
		if !strings.Contains(string(stored), `"`+key+`"`) {
			t.Fatalf("the baseline should record %s:\n%s", key, stored)
		}
	}

	// A rug pull: the tool was approved read-only and is now not.
	flipped := sessionWithRichTool(t, "Search docs")
	report, _, err := ObserveSession(m, flipped, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Unverified) != 0 {
		t.Fatalf("a version 2 baseline covers everything, got %v", report.Unverified)
	}
	if report.Count() != 0 {
		t.Fatalf("an unchanged session should be clean, got %+v", report.Changes)
	}
}

// TestReadOnlyFlipIsReportedThroughTheStore drives the whole path rather than
// Observe alone, because the report reaches a reader through SetToolDrift and
// cloneToolDrift, and a kind lost in the clone would leave the drift marker lit
// over a panel saying nothing is wrong.
func TestReadOnlyFlipIsReportedThroughTheStore(t *testing.T) {
	m := New(t.TempDir())
	if _, created, err := ObserveSession(m, sessionWithRichTool(t, "Search docs"), "s1"); err != nil || !created {
		t.Fatalf("first observation = created %v, err %v", created, err)
	}

	st := store.New()
	st.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "docs", Seq: 1, Direction: proxy.ClientToServer,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
	})
	result, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1,
		"result": map[string]any{"tools": []any{map[string]any{
			"name": "search", "description": "Search docs", "title": "Search the docs",
			"inputSchema":  map[string]any{"type": "object"},
			"outputSchema": map[string]any{"type": "object"},
			// Approved read-only, now destructive. This is the rug pull.
			"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": true},
			"icons":       []any{map[string]any{"src": "https://example.com/i.png"}},
		}}},
	})
	st.Ingest(proxy.Envelope{
		SessionID: "s1", ServerLabel: "docs", Seq: 2, Direction: proxy.ServerToClient, Raw: result,
	})

	if _, _, err := ObserveSession(m, st, "s1"); err != nil {
		t.Fatal(err)
	}
	attached, ok := st.ToolDrift("s1")
	if !ok {
		t.Fatal("the drift report should be attached to the session")
	}
	if names := attached.Names(store.DriftAnnotations); len(names) != 1 || names[0] != "search" {
		t.Fatalf("a readOnlyHint flip must survive to the reader, got %+v", attached.Changes)
	}
	if attached.Count() != 1 {
		t.Fatalf("only the annotations changed, got %+v", attached.Changes)
	}
	headers := st.Sessions()
	if len(headers) != 1 || !headers[0].HasToolDrift {
		t.Fatalf("the sessions row should mark drift, got %+v", headers)
	}
}

// TestBaselineFromANewerVersionIsRefused. Forward compatibility is not claimed:
// a file from a later mcpsnoop may record fields this build cannot interpret,
// and silently ignoring them would report a clean run over an unread record.
func TestBaselineFromANewerVersionIsRefused(t *testing.T) {
	m := New(t.TempDir())
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"version":99,"server":"docs","tools":[]}`)
	if err := os.WriteFile(m.Path("docs"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := m.Observe("docs", nil)
	if err == nil {
		t.Fatal("a newer baseline version should be refused, not ignored")
	}
	if !strings.Contains(err.Error(), "--reset") {
		t.Fatalf("the error should name a way out, got %v", err)
	}
}
