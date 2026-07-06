package proxy

import (
	"encoding/json"
	"testing"
)

func TestRedactingSinkScrubsMatchingKeysRecursively(t *testing.T) {
	sink := &captureSink{}
	redacted := NewRedactingSink(sink, RedactConfig{
		Keys: []string{"authorization", "token", "api_key", "password"},
	})

	redacted.Emit(Envelope{
		Raw: json.RawMessage(`{
			"jsonrpc":"2.0",
			"id":1,
			"method":"tools/call",
			"params":{
				"authorization":"Bearer secret",
				"arguments":{
					"token":"abc123",
					"nested":[{"api_key":"k-123","keep":"visible"}],
					"Password":{"inner":"secret"}
				}
			}
		}`),
		Text: "stderr token=secret",
	})

	got := sink.byDir("")[0]
	if got.Text != "stderr token=secret" {
		t.Fatalf("Text = %q, want unchanged stderr text", got.Text)
	}
	var obj map[string]any
	if err := json.Unmarshal(got.Raw, &obj); err != nil {
		t.Fatalf("redacted Raw is invalid JSON: %v", err)
	}
	params := obj["params"].(map[string]any)
	if params["authorization"] != redactedValue {
		t.Fatalf("authorization = %v, want redacted", params["authorization"])
	}
	args := params["arguments"].(map[string]any)
	if args["token"] != redactedValue {
		t.Fatalf("token = %v, want redacted", args["token"])
	}
	if args["Password"] != redactedValue {
		t.Fatalf("Password = %v, want redacted case-insensitively", args["Password"])
	}
	nested := args["nested"].([]any)[0].(map[string]any)
	if nested["api_key"] != redactedValue {
		t.Fatalf("api_key = %v, want redacted", nested["api_key"])
	}
	if nested["keep"] != "visible" {
		t.Fatalf("keep = %v, want visible", nested["keep"])
	}
}

func TestRedactingSinkLeavesPayloadUnchangedWithoutConfig(t *testing.T) {
	sink := &captureSink{}
	redacted := NewRedactingSink(sink, RedactConfig{})
	raw := json.RawMessage(`{"params":{"token":"abc123"}}`)

	redacted.Emit(Envelope{Raw: raw})

	got := sink.byDir("")[0]
	if string(got.Raw) != string(raw) {
		t.Fatalf("Raw = %s, want unchanged %s", got.Raw, raw)
	}
}

func TestRedactingSinkLeavesInvalidJSONUnchanged(t *testing.T) {
	sink := &captureSink{}
	redacted := NewRedactingSink(sink, RedactConfig{Keys: []string{"token"}})
	raw := json.RawMessage(`{"params":{"token":`)

	redacted.Emit(Envelope{Raw: raw})

	got := sink.byDir("")[0]
	if string(got.Raw) != string(raw) {
		t.Fatalf("Raw = %s, want unchanged %s", got.Raw, raw)
	}
}
