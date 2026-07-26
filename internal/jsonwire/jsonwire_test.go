package jsonwire

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestMarshalLeavesMarkupAlone is the whole point of the package: encoding/json
// rewrites <, > and & by default, including inside a json.RawMessage that is
// only being passed through, and a wire capture must not be rewritten.
func TestMarshalLeavesMarkupAlone(t *testing.T) {
	payload := json.RawMessage(`{"text":"<b>hi</b> & bye"}`)
	got, err := Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("Marshal rewrote a passed-through payload:\n got %s\nwant %s", got, payload)
	}
	// The comparison above is the real assertion; this one names the failure.
	for _, e := range []string{`\u003c`, `\u003e`, `\u0026`} {
		if strings.Contains(string(got), e) {
			t.Fatalf("Marshal emitted %s", e)
		}
	}
}

// TestMarshalDropsTheEncoderNewline. json.Encoder.Encode appends one and
// json.Marshal does not, and callers here are substituting for json.Marshal.
func TestMarshalDropsTheEncoderNewline(t *testing.T) {
	got, err := Marshal(map[string]string{"a": "b"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.HasSuffix(got, []byte("\n")) {
		t.Fatalf("Marshal should not append a newline, got %q", got)
	}
	want, _ := json.Marshal(map[string]string{"a": "b"})
	if !bytes.Equal(got, want) {
		t.Fatalf("Marshal  = %q, want %q", got, want)
	}
}

// TestNewEncoderStillProducesValidJSON. Turning escaping off must not produce
// something a decoder rejects, since the trace file is read back by the hub,
// the exporter and the replayer.
func TestNewEncoderStillProducesValidJSON(t *testing.T) {
	var buf bytes.Buffer
	in := map[string]string{"text": "a < b && c > d"}
	if err := NewEncoder(&buf).Encode(in); err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("the output must round-trip: %v (%s)", err, buf.String())
	}
	if out["text"] != in["text"] {
		t.Fatalf("round-trip changed the value: %q", out["text"])
	}
}
