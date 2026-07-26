package toolbaseline

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kerlenton/mcpsnoop/internal/store"
)

// TestBaselineFileKeepsMarkupInTheStoredDefinition. The baseline is the durable
// record of what --accept trusted, so it has to say what the server said. The
// schema is the half a bare encoder swap misses: normalize canonicalises it into
// a RawMessage before the outer encoder runs, and turning escaping off does not
// unescape what is already escaped.
func TestBaselineFileKeepsMarkupInTheStoredDefinition(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)
	defs := []store.ToolDefinition{{
		Name:        "search",
		Description: "Find a <tag> & keep it",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"description":"pattern, e.g. <name>"}}}`),
	}}
	if err := m.Accept("docs", defs); err != nil {
		t.Fatal(err)
	}

	stored, err := os.ReadFile(m.Path("docs"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), `\u003c`) {
		t.Fatalf("the baseline must record what the server sent:\n%s", stored)
	}
	for _, want := range []string{"Find a <tag> & keep it", "pattern, e.g. <name>"} {
		if !strings.Contains(string(stored), want) {
			t.Fatalf("expected %q in the baseline:\n%s", want, stored)
		}
	}
}

// TestBaselineWrittenBeforeTheEncodingChangeStillMatches. load re-canonicalises
// what it read, so a baseline recorded with the old escaping must not read as
// drift against the same tools captured verbatim today. Without that, upgrading
// would turn every stored definition containing markup into a CI failure.
func TestBaselineWrittenBeforeTheEncodingChangeStillMatches(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)
	defs := []store.ToolDefinition{{
		Name:        "search",
		Description: "Find a <tag>",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"description":"<name>"}}}`),
	}}
	if err := m.Accept("docs", defs); err != nil {
		t.Fatal(err)
	}

	// Rewrite the file the way the old encoder would have left it.
	stored, err := os.ReadFile(m.Path("docs"))
	if err != nil {
		t.Fatal(err)
	}
	old := strings.NewReplacer("<", `\u003c`, ">", `\u003e`, "&", `\u0026`).Replace(string(stored))
	if old == string(stored) {
		t.Fatal("the fixture should differ from the current encoding, or it proves nothing")
	}
	if err := os.WriteFile(m.Path("docs"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	report, created, err := m.Observe("docs", defs)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("the existing baseline should have been read, not recreated")
	}
	if !report.Empty() {
		t.Fatalf("an old-encoding baseline must not read as drift: %+v", report)
	}
}
