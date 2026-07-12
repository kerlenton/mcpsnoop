package proxy

import (
	"encoding/json"
	"strings"
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

func TestRedactingSinkScrubsCommonSecretPresetAndExplicitKeys(t *testing.T) {
	sink := &captureSink{}
	redacted := NewRedactingSink(sink, RedactConfig{
		CommonSecrets: true,
		Keys:          []string{"custom_secret"},
	})

	redacted.Emit(Envelope{
		Raw: json.RawMessage(`{
			"params":{
				"Authorization":"Bearer secret",
				"apiKey":"key-123",
				"client_secret":"client-123",
				"custom_secret":"custom-123",
				"keep":"visible"
			}
		}`),
	})

	got := sink.byDir("")[0]
	var obj map[string]any
	if err := json.Unmarshal(got.Raw, &obj); err != nil {
		t.Fatalf("redacted Raw is invalid JSON: %v", err)
	}
	params := obj["params"].(map[string]any)
	for _, key := range []string{"Authorization", "apiKey", "client_secret", "custom_secret"} {
		if params[key] != redactedValue {
			t.Fatalf("%s = %v, want redacted", key, params[key])
		}
	}
	if params["keep"] != "visible" {
		t.Fatalf("keep = %v, want visible", params["keep"])
	}
}

func TestRedactingSinkScrubsValuePatternMatches(t *testing.T) {
	sink := &captureSink{}
	redacted := NewRedactingSink(sink, RedactConfig{
		ValuePatterns: []string{`sk-[A-Za-z0-9]+`, `Bearer\s+\S+`},
	})

	redacted.Emit(Envelope{
		Raw: json.RawMessage(`{
			"params":{
				"message":"use sk-abc123 in this text",
				"headers":["Bearer token-123", "keep visible"],
				"count":42
			}
		}`),
		Text: "stderr leaked sk-stderr123",
	})

	got := sink.byDir("")[0]
	if got.Text != "stderr leaked [REDACTED]" {
		t.Fatalf("Text = %q, want stderr leaked [REDACTED]", got.Text)
	}
	var obj map[string]any
	if err := json.Unmarshal(got.Raw, &obj); err != nil {
		t.Fatalf("redacted Raw is invalid JSON: %v", err)
	}
	params := obj["params"].(map[string]any)
	if params["message"] != "use [REDACTED] in this text" {
		t.Fatalf("message = %v, want value pattern redacted", params["message"])
	}
	headers := params["headers"].([]any)
	if headers[0] != redactedValue {
		t.Fatalf("headers[0] = %v, want redacted", headers[0])
	}
	if headers[1] != "keep visible" {
		t.Fatalf("headers[1] = %v, want visible", headers[1])
	}
	if params["count"] != float64(42) {
		t.Fatalf("count = %v, want unchanged number", params["count"])
	}
}

func TestRedactingSinkScrubsOnlyMatchingJSONPath(t *testing.T) {
	path, err := ParseRedactPath("$.params.arguments.password")
	if err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}
	redacted := NewRedactingSink(sink, RedactConfig{Paths: []RedactPath{path}})
	raw := json.RawMessage(`{
		"params":{
			"password":"keep-param",
			"arguments":{"password":"secret","nested":{"password":"keep-nested"}}
		},
		"password":"keep-root"
	}`)

	redacted.Emit(Envelope{Raw: raw})

	var obj map[string]any
	if err := json.Unmarshal(sink.byDir("")[0].Raw, &obj); err != nil {
		t.Fatal(err)
	}
	params := obj["params"].(map[string]any)
	args := params["arguments"].(map[string]any)
	if args["password"] != redactedValue {
		t.Fatalf("arguments.password = %v, want redacted", args["password"])
	}
	if params["password"] != "keep-param" || obj["password"] != "keep-root" {
		t.Fatalf("same-named fields outside the path were changed: %v", obj)
	}
	if got := args["nested"].(map[string]any)["password"]; got != "keep-nested" {
		t.Fatalf("nested password = %v, want unchanged", got)
	}
}

func TestRedactPathPreservesUntargetedNumbers(t *testing.T) {
	path, err := ParseRedactPath("$.secret")
	if err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}
	redacted := NewRedactingSink(sink, RedactConfig{Paths: []RedactPath{path}})
	// Redacting one field re-marshals the whole payload, so untargeted big
	// integers and exponents must round-trip verbatim, not through float64.
	redacted.Emit(Envelope{Raw: json.RawMessage(
		`{"secret":"x","id":10000000000000000001,"big":123456789012345678,"exp":1.5e10}`)})

	out := string(sink.byDir("")[0].Raw)
	if !strings.Contains(out, `"secret":"[REDACTED]"`) {
		t.Fatalf("secret not redacted: %s", out)
	}
	for _, want := range []string{`"id":10000000000000000001`, `"big":123456789012345678`, `"exp":1.5e10`} {
		if !strings.Contains(out, want) {
			t.Fatalf("number was reformatted, missing %q in: %s", want, out)
		}
	}
}

func TestRedactPathComposesWithKeyRedaction(t *testing.T) {
	path, err := ParseRedactPath("$.params.arguments.password")
	if err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}
	redacted := NewRedactingSink(sink, RedactConfig{
		Keys:  []string{"token"},
		Paths: []RedactPath{path},
	})
	// A path rule and a key rule apply to the same payload in one pass.
	redacted.Emit(Envelope{Raw: json.RawMessage(
		`{"params":{"token":"t","arguments":{"password":"p","keep":"k"}}}`)})

	var obj map[string]any
	if err := json.Unmarshal(sink.byDir("")[0].Raw, &obj); err != nil {
		t.Fatal(err)
	}
	params := obj["params"].(map[string]any)
	if params["token"] != redactedValue {
		t.Fatalf("token (key rule) = %v, want redacted", params["token"])
	}
	args := params["arguments"].(map[string]any)
	if args["password"] != redactedValue {
		t.Fatalf("password (path rule) = %v, want redacted", args["password"])
	}
	if args["keep"] != "k" {
		t.Fatalf("keep = %v, want unchanged", args["keep"])
	}
}

func TestRedactingSinkScrubsEveryJSONPathWildcardMatch(t *testing.T) {
	path, err := ParseRedactPath("$.params.arguments.accounts[*].password")
	if err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}
	redacted := NewRedactingSink(sink, RedactConfig{Paths: []RedactPath{path}})

	redacted.Emit(Envelope{Raw: json.RawMessage(`{
		"params":{"arguments":{"accounts":[
			{"password":"first","name":"one"},
			{"password":"second","name":"two"}
		]}}
	}`)})

	var obj map[string]any
	if err := json.Unmarshal(sink.byDir("")[0].Raw, &obj); err != nil {
		t.Fatal(err)
	}
	accounts := obj["params"].(map[string]any)["arguments"].(map[string]any)["accounts"].([]any)
	for i, account := range accounts {
		got := account.(map[string]any)
		if got["password"] != redactedValue {
			t.Fatalf("accounts[%d].password = %v, want redacted", i, got["password"])
		}
	}
}

func TestRedactingSinkLeavesRawBytesUnchangedWhenJSONPathDoesNotMatch(t *testing.T) {
	path, err := ParseRedactPath("$.params.arguments.password")
	if err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}
	redacted := NewRedactingSink(sink, RedactConfig{Paths: []RedactPath{path}})
	raw := json.RawMessage(`{ "params": { "arguments": { "token": "visible" } } }`)

	redacted.Emit(Envelope{Raw: raw})

	if got := sink.byDir("")[0].Raw; string(got) != string(raw) {
		t.Fatalf("Raw = %s, want byte-for-byte unchanged %s", got, raw)
	}
}

func TestParseRedactPathRejectsInvalidOrNonModifiableExpressions(t *testing.T) {
	for _, path := range []string{"", "$.[", "$.."} {
		t.Run(path, func(t *testing.T) {
			if _, err := ParseRedactPath(path); err == nil {
				t.Fatalf("ParseRedactPath(%q) returned nil error", path)
			}
		})
	}
}

func TestRedactConfigEnabledByJSONPath(t *testing.T) {
	path, err := ParseRedactPath("$")
	if err != nil {
		t.Fatal(err)
	}
	if !(RedactConfig{Paths: []RedactPath{path}}).Enabled() {
		t.Fatal("RedactConfig.Enabled() = false, want true")
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
