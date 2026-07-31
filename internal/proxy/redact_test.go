package proxy

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactorScrubsSecretsInServerArgv(t *testing.T) {
	r := NewRedactor(RedactConfig{CommonSecrets: true})
	meta, err := json.Marshal(SessionMeta{
		Command: []string{"npx", "some-server", "--api-key=sk-secret", "--token", "tok-secret", "--verbose"},
		CWD:     "/home/user/project",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := string(r.RedactEnvelope(Envelope{Direction: DirectionMeta, Raw: meta}).Raw)

	if strings.Contains(got, "sk-secret") || strings.Contains(got, "tok-secret") {
		t.Fatalf("a secret survived argv redaction: %s", got)
	}
	if !strings.Contains(got, "--api-key=[REDACTED]") {
		t.Fatalf("--api-key=value was not redacted in place: %s", got)
	}
	if !strings.Contains(got, "--token") || !strings.Contains(got, redactedValue) {
		t.Fatalf("--token followed by its value was not redacted: %s", got)
	}
	if !strings.Contains(got, "--verbose") {
		t.Fatalf("an unrelated argument must be left intact: %s", got)
	}
	if !strings.Contains(got, "/home/user/project") {
		t.Fatalf("cwd must be left intact: %s", got)
	}
}

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

func TestRedactingSinkScrubsMCPParamHeaders(t *testing.T) {
	sink := &captureSink{}
	redacted := NewRedactingSink(sink, RedactConfig{
		CommonSecrets: true,
		ValuePatterns: []string{`sk-[A-Za-z0-9]+`},
	})
	headers := []MCPParamHeader{
		{Name: "Mcp-Param-Token", Value: "=?base64?c2VjcmV0?="},
		{Name: "Mcp-Param-Region", Value: "route sk-abc123 here"},
		{Name: "Mcp-Param-Safe", Value: "visible"},
	}

	redacted.Emit(Envelope{MCPParamHeaders: headers})

	got := sink.byDir("")[0].MCPParamHeaders
	if got[0].Value != redactedValue {
		t.Fatalf("token header = %q, want redacted", got[0].Value)
	}
	if got[1].Value != "route [REDACTED] here" {
		t.Fatalf("pattern header = %q, want value-pattern redaction", got[1].Value)
	}
	if got[2].Value != "visible" {
		t.Fatalf("safe header = %q, want unchanged", got[2].Value)
	}
	if headers[0].Value == redactedValue || strings.Contains(headers[1].Value, redactedValue) {
		t.Fatal("redaction mutated the caller's envelope slice")
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

// TestRedactRawPreservesMarkupInUntouchedFields. Redaction re-encodes the whole
// payload to replace one value, so every other field is re-encoded with it. It
// used to come back HTML-escaped, which rewrote parts of the message that
// redaction was never asked to touch.
func TestRedactRawPreservesMarkupInUntouchedFields(t *testing.T) {
	sink := &captureSink{}
	redacted := NewRedactingSink(sink, RedactConfig{Keys: []string{"token"}})

	redacted.Emit(Envelope{
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"params":{"token":"abc123","html":"<b>hi</b> & bye"}}`),
	})

	got := string(sink.byDir("")[0].Raw)
	if strings.Contains(got, `\u003c`) {
		t.Fatalf("redaction must not rewrite the fields it leaves alone: %s", got)
	}
	if !strings.Contains(got, `<b>hi</b> & bye`) {
		t.Fatalf("the untouched value should survive verbatim: %s", got)
	}
	if !strings.Contains(got, redactedValue) {
		t.Fatalf("the secret should still be redacted: %s", got)
	}
}

// TestRedactPathAlsoScrubsTheMirroredParamHeader is the blocker this closes. A
// JSONPath addresses the body and has no expression for a header, so path
// redaction used to scrub the argument and leave its mirror in the Mcp-Param
// header verbatim. That leaked the secret into the log and made the store report
// the pair as disagreeing, which fails a default check run on correct traffic.
func TestRedactPathAlsoScrubsTheMirroredParamHeader(t *testing.T) {
	path, err := ParseRedactPath("$.params.arguments.authKey")
	if err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}
	NewRedactingSink(sink, RedactConfig{Paths: []RedactPath{path}}).Emit(Envelope{
		Direction: ClientToServer,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` +
			`{"name":"fetch","arguments":{"region":"us-west1","authKey":"sk-live-abcdef"}}}`),
		MCPParamHeaders: []MCPParamHeader{
			{Name: "Mcp-Param-Region", Value: "us-west1"},
			{Name: "Mcp-Param-Auth", Value: "sk-live-abcdef"},
		},
	})

	got := sink.byDir(ClientToServer)
	if len(got) != 1 {
		t.Fatalf("want one envelope, got %d", len(got))
	}
	if strings.Contains(string(got[0].Raw), "sk-live-abcdef") {
		t.Fatalf("the body should be scrubbed: %s", got[0].Raw)
	}
	for _, h := range got[0].MCPParamHeaders {
		if strings.Contains(h.Value, "sk-live-abcdef") {
			t.Fatalf("header %s still carries the secret the path rule removed: %q", h.Name, h.Value)
		}
		// A header the rule never named keeps its value, or redaction has turned
		// into a blanket wipe and the comparison it feeds becomes useless.
		if h.Name == "Mcp-Param-Region" && h.Value != "us-west1" {
			t.Fatalf("an unrelated header must survive, got %q", h.Value)
		}
	}
	if !got[0].Redacted {
		t.Fatal("a rewritten frame must be marked redacted, or the store cannot tell " +
			"mcpsnoop's placeholder from a client that sent those bytes")
	}
}

// TestRedactKeyAndSecretsAlsoScrubTheMirroredParamHeader. The link was recorded
// for path rules only, so --redact-key and --redact-secrets scrubbed the body and
// left the header holding the secret, which is the same leak in the two flags
// most people actually reach for.
func TestRedactKeyAndSecretsAlsoScrubTheMirroredParamHeader(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    RedactConfig
		args   string
		header string
	}{
		{"key rule", RedactConfig{Keys: []string{"authKey"}}, `{"authKey":"sk-live-abcdef"}`, "Mcp-Param-Auth"},
		{"secrets preset", RedactConfig{CommonSecrets: true}, `{"api_key":"sk-live-abcdef"}`, "Mcp-Param-Key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &captureSink{}
			NewRedactingSink(sink, tc.cfg).Emit(Envelope{
				Direction: ClientToServer,
				Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` +
					`{"name":"fetch","arguments":` + tc.args + `}}`),
				MCPParamHeaders: []MCPParamHeader{{Name: tc.header, Value: "sk-live-abcdef"}},
			})
			got := sink.byDir(ClientToServer)
			if len(got) != 1 {
				t.Fatalf("want one envelope, got %d", len(got))
			}
			if strings.Contains(string(got[0].Raw), "sk-live-abcdef") {
				t.Fatalf("the body should be scrubbed: %s", got[0].Raw)
			}
			if v := got[0].MCPParamHeaders[0].Value; strings.Contains(v, "sk-live-abcdef") {
				t.Fatalf("header %s still carries the secret the rule removed: %q", tc.header, v)
			}
		})
	}
}

// TestRedactScrubsTheEncodedMirror. A header value outside visible ASCII may only
// travel wrapped in the Base64 sentinel, while the body holds it plain, so a link
// that compares the two spellings verbatim never fires and the log keeps a header
// that decodes straight back to the secret.
func TestRedactScrubsTheEncodedMirror(t *testing.T) {
	const secret = "sk-live-абв"
	path, err := ParseRedactPath("$.params.arguments.authKey")
	if err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}
	NewRedactingSink(sink, RedactConfig{Paths: []RedactPath{path}}).Emit(Envelope{
		Direction: ClientToServer,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` +
			`{"name":"fetch","arguments":{"authKey":` + quoteJSONString(secret) + `}}}`),
		MCPParamHeaders: []MCPParamHeader{
			{Name: "Mcp-Param-Auth", Value: Base64SentinelPrefix +
				base64.StdEncoding.EncodeToString([]byte(secret)) + Base64SentinelSuffix},
		},
	})

	got := sink.byDir(ClientToServer)
	value := got[0].MCPParamHeaders[0].Value
	decoded, ok := DecodeHeaderValue(value)
	if !ok {
		t.Fatalf("header value stopped decoding: %q", value)
	}
	if strings.Contains(decoded, secret) {
		t.Fatalf("the encoded header still decodes back to the secret: %q", value)
	}
}

// TestRedactLeavesHeadersWhoseValueSurvivesElsewhere. Matching by value alone
// wiped every header sharing a spelling with a scrubbed argument, which is the
// normal case for booleans and small integers, and the store then skipped those
// bindings, so a genuine disagreement on them went unreported. When the spelling
// is still in the stored body in clear, removing it from the header protects
// nothing and only costs a check.
func TestRedactLeavesHeadersWhoseValueSurvivesElsewhere(t *testing.T) {
	for _, tc := range []struct {
		name, path, args string
		headers          []MCPParamHeader
		wantKept         string
	}{
		{
			name: "boolean", path: "$.params.arguments.debug",
			args:     `{"debug":true,"cacheEnabled":true}`,
			headers:  []MCPParamHeader{{Name: "Mcp-Param-Debug", Value: "true"}, {Name: "Mcp-Param-Cache", Value: "true"}},
			wantKept: "true",
		},
		{
			name: "small integer", path: "$.params.arguments.shard",
			args:     `{"shard":7,"retries":7}`,
			headers:  []MCPParamHeader{{Name: "Mcp-Param-Shard", Value: "7"}, {Name: "Mcp-Param-Retries", Value: "7"}},
			wantKept: "7",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, err := ParseRedactPath(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			sink := &captureSink{}
			NewRedactingSink(sink, RedactConfig{Paths: []RedactPath{path}}).Emit(Envelope{
				Direction: ClientToServer,
				Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` +
					`{"name":"fetch","arguments":` + tc.args + `}}`),
				MCPParamHeaders: tc.headers,
			})
			for _, h := range sink.byDir(ClientToServer)[0].MCPParamHeaders {
				if h.Value != tc.wantKept {
					t.Fatalf("header %s = %q, want %q: the same spelling is still in the body "+
						"in clear, so scrubbing the header buys nothing and costs a check",
						h.Name, h.Value, tc.wantKept)
				}
			}
		})
	}
}

// TestRedactScrubsANumericMirrorSpelledDifferently. The spec has these compared
// numerically, so a body of 4.2e1 and a header of 42 are one value, and matching
// wire spellings alone left the header showing a number the body no longer does.
func TestRedactScrubsANumericMirrorSpelledDifferently(t *testing.T) {
	path, err := ParseRedactPath("$.params.arguments.account")
	if err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}
	NewRedactingSink(sink, RedactConfig{Paths: []RedactPath{path}}).Emit(Envelope{
		Direction: ClientToServer,
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` +
			`{"name":"fetch","arguments":{"account":4.2e1}}}`),
		MCPParamHeaders: []MCPParamHeader{{Name: "Mcp-Param-Account", Value: "42"}},
	})
	if v := sink.byDir(ClientToServer)[0].MCPParamHeaders[0].Value; v != redactedValue {
		t.Fatalf("header = %q, want it scrubbed alongside the body it mirrors", v)
	}
}

func quoteJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
