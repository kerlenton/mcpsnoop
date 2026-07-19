package exporter

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/kerlenton/mcpsnoop/internal/proxy"
)

// harSession builds a session with the three call outcomes that shape the HAR:
// one that succeeded, one that failed, and one that never got a response.
func harSession() SessionExport {
	start := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	ok := 12.5
	failed := 30.0
	return SessionExport{
		Session: SessionSummary{ID: "s1-123", Label: "filesystem"},
		Calls: []CallExport{
			{
				Index: 0, ID: "1", Method: "tools/call", Status: "ok",
				IsTool: true, ToolName: "search",
				StartedAt: start, DurationMS: &ok,
				Params: json.RawMessage(`{"name":"search"}`),
				Result: json.RawMessage(`{"content":[]}`),
			},
			{
				Index: 1, ID: "2", Method: "tools/call", Status: "error",
				IsTool: true, ToolName: "write", IsError: true,
				StartedAt: start.Add(time.Second), DurationMS: &failed,
				Params: json.RawMessage(`{"name":"write"}`),
				Error:  &proxy.RPCError{Code: -32602, Message: "invalid params"},
			},
			{
				Index: 2, ID: "3", Method: "tools/list", Status: "pending",
				StartedAt: start.Add(2 * time.Second),
			},
		},
	}
}

func writeHARForTest(t *testing.T, data SessionExport) harRoot {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteHAR(&buf, data); err != nil {
		t.Fatalf("WriteHAR: %v", err)
	}
	var got harRoot
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	return got
}

// The spec makes cache and timings mandatory on every entry, and this is what a
// strict validator rejects a HAR for, so assert on the raw JSON rather than the
// decoded struct, which would happily supply zero values for missing keys.
func TestHARAlwaysCarriesCacheAndTimings(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteHAR(&buf, harSession()); err != nil {
		t.Fatalf("WriteHAR: %v", err)
	}
	var raw struct {
		Log struct {
			Entries []map[string]json.RawMessage `json:"entries"`
		} `json:"log"`
	}
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if len(raw.Log.Entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(raw.Log.Entries))
	}
	for i, entry := range raw.Log.Entries {
		for _, key := range []string{"startedDateTime", "time", "request", "response", "cache", "timings"} {
			if _, ok := entry[key]; !ok {
				t.Errorf("entry %d is missing the required %q field", i, key)
			}
		}
	}
}

// time must equal the sum of the timings, and send, wait and receive must be
// non-negative, so -1 is not a legal stand-in for the ones mcpsnoop cannot know.
func TestHARTimingsSumToTime(t *testing.T) {
	got := writeHARForTest(t, harSession())
	for i, entry := range got.Log.Entries {
		tm := entry.Timings
		if tm.Send < 0 || tm.Wait < 0 || tm.Receive < 0 {
			t.Errorf("entry %d has a negative timing %+v, send/wait/receive must be non-negative", i, tm)
		}
		if sum := tm.Send + tm.Wait + tm.Receive; sum != entry.Time {
			t.Errorf("entry %d time = %v, want the timing sum %v", i, entry.Time, sum)
		}
	}
}

func TestHARMapsCallOutcomeToStatus(t *testing.T) {
	got := writeHARForTest(t, harSession())
	want := []int{200, 500, 0}
	for i, code := range want {
		if got.Log.Entries[i].Response.Status != code {
			t.Errorf("entry %d status = %d, want %d", i, got.Log.Entries[i].Response.Status, code)
		}
	}
	// A failed call still needs a readable body, and the error object is all there is.
	if body := got.Log.Entries[1].Response.Content.Text; body == "" {
		t.Error("a failed call should carry its JSON-RPC error as the response body")
	}
	// An unanswered call must not look like an empty success.
	if body := got.Log.Entries[2].Response.Content.Text; body != "" {
		t.Errorf("a pending call should have no response body, got %q", body)
	}
}

// Every tools/call would otherwise share one URL, which is the column a HAR
// viewer is scanned by.
func TestHARURLCarriesLabelAndTool(t *testing.T) {
	got := writeHARForTest(t, harSession())
	if url := got.Log.Entries[0].Request.URL; url != "mcp://filesystem/tools/call/search" {
		t.Errorf("url = %q, want the label, method and tool", url)
	}
	if url := got.Log.Entries[2].Request.URL; url != "mcp://filesystem/tools/list" {
		t.Errorf("url = %q, want no trailing tool for a non-tool call", url)
	}
}

func TestHARFallsBackToSessionIDWhenUnlabelled(t *testing.T) {
	data := harSession()
	data.Session.Label = ""
	got := writeHARForTest(t, data)
	if url := got.Log.Entries[0].Request.URL; url != "mcp://s1-123/tools/call/search" {
		t.Errorf("url = %q, want the session id when there is no label", url)
	}
}

func TestHARLogHeader(t *testing.T) {
	got := writeHARForTest(t, harSession())
	if got.Log.Version != "1.2" {
		t.Errorf("log.version = %q, want 1.2", got.Log.Version)
	}
	if got.Log.Creator.Name != "mcpsnoop" || got.Log.Creator.Version == "" {
		t.Errorf("creator = %+v, want a name and a non-empty version", got.Log.Creator)
	}
}

// An empty session must still produce a valid document with an empty array
// rather than a null, which some readers reject.
func TestHAREmptySessionStaysValid(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteHAR(&buf, SessionExport{Session: SessionSummary{ID: "s1"}}); err != nil {
		t.Fatalf("WriteHAR: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"entries": []`)) {
		t.Errorf("an empty session should emit an empty entries array\n%s", buf.String())
	}
}
