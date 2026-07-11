package main

import (
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

func TestParseConfigEmpty(t *testing.T) {
	cfg, err := parseConfig(strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Label != "" {
		t.Fatal("expected empty label")
	}

	if cfg.TraceFile != "" {
		t.Fatal("expected empty trace file")
	}

	if cfg.NoTrace {
		t.Fatal("expected no-trace=false")
	}

	if cfg.RedactSecrets {
		t.Fatal("expected redact-secrets=false")
	}

	if len(cfg.RedactKeys) != 0 {
		t.Fatal("expected no redact keys")
	}
}

func TestParseConfigLabel(t *testing.T) {
	cfg, err := parseConfig(strings.NewReader(`label = "filesystem"`))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Label != "filesystem" {
		t.Fatalf("expected label %q, got %q", "filesystem", cfg.Label)
	}
}

func TestParseConfigBoolValues(t *testing.T) {
	cfg, err := parseConfig(strings.NewReader(`
no-trace = true
redact-secrets = true
`))
	if err != nil {
		t.Fatal(err)
	}

	if !cfg.NoTrace {
		t.Fatal("expected no-trace=true")
	}

	if !cfg.RedactSecrets {
		t.Fatal("expected redact-secrets=true")
	}
}

func TestParseConfigRedactKeys(t *testing.T) {
	cfg, err := parseConfig(strings.NewReader(`
redact-key = "token,api_key"
`))
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"token", "api_key"}

	if len(cfg.RedactKeys) != len(want) {
		t.Fatalf("expected %d keys, got %d", len(want), len(cfg.RedactKeys))
	}

	for i := range want {
		if cfg.RedactKeys[i] != want[i] {
			t.Fatalf("expected %q at index %d, got %q", want[i], i, cfg.RedactKeys[i])
		}
	}
}

func TestParseConfigUnknownKey(t *testing.T) {
	_, err := parseConfig(strings.NewReader(`
foo = "bar"
`))

	if err == nil {
		t.Fatal("expected error")
	}
}

func TestParseConfigInvalidBool(t *testing.T) {
	_, err := parseConfig(strings.NewReader(`
no-trace = maybe
`))

	if err == nil {
		t.Fatal("expected error")
	}
}

func TestParseConfigMissingEquals(t *testing.T) {
	_, err := parseConfig(strings.NewReader("just some text"))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestApplyConfigUsesConfigWhenFlagNotSet(t *testing.T) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)

	label := fs.String("label", "", "")
	traceFile := fs.String("trace-file", "", "")
	noTrace := fs.Bool("no-trace", false, "")
	redactSecrets := fs.Bool("redact-secrets", false, "")
	var redactKeys redactKeysFlag
	fs.Var(&redactKeys, "redact-key", "")

	cfg := config{
		Label:         "filesystem",
		TraceFile:     "trace.jsonl",
		NoTrace:       true,
		RedactSecrets: true,
		RedactKeys:    []string{"token", "api_key"},
	}

	applyConfig(fs, cfg, true, label, traceFile, noTrace, redactSecrets, &redactKeys)

	if *label != "filesystem" {
		t.Fatalf("expected label %q, got %q", "filesystem", *label)
	}

	if *traceFile != "trace.jsonl" {
		t.Fatalf("expected trace-file %q, got %q", "trace.jsonl", *traceFile)
	}

	if !*noTrace {
		t.Fatal("expected no-trace=true")
	}

	if !*redactSecrets {
		t.Fatal("expected redact-secrets=true")
	}

	want := []string{"token", "api_key"}

	if len(redactKeys) != len(want) {
		t.Fatalf("expected %d redact keys, got %d", len(want), len(redactKeys))
	}

	for i := range want {
		if redactKeys[i] != want[i] {
			t.Fatalf("expected %q at index %d, got %q", want[i], i, redactKeys[i])
		}
	}
}

func TestApplyConfigExplicitFlagOverridesConfig(t *testing.T) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)

	label := fs.String("label", "", "")
	traceFile := fs.String("trace-file", "", "")
	noTrace := fs.Bool("no-trace", false, "")
	redactSecrets := fs.Bool("redact-secrets", false, "")
	var redactKeys redactKeysFlag
	fs.Var(&redactKeys, "redact-key", "")

	if err := fs.Parse([]string{"--label=cli"}); err != nil {
		t.Fatal(err)
	}

	cfg := config{
		Label: "from-config",
	}

	applyConfig(fs, cfg, true, label, traceFile, noTrace, redactSecrets, &redactKeys)

	if *label != "cli" {
		t.Fatalf("expected CLI value %q, got %q", "cli", *label)
	}
}

func TestApplyConfigNoConfigFile(t *testing.T) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)

	label := fs.String("label", "", "")
	traceFile := fs.String("trace-file", "", "")
	noTrace := fs.Bool("no-trace", false, "")
	redactSecrets := fs.Bool("redact-secrets", false, "")
	var redactKeys redactKeysFlag
	fs.Var(&redactKeys, "redact-key", "")

	cfg := config{
		Label:         "filesystem",
		TraceFile:     "trace.jsonl",
		NoTrace:       true,
		RedactSecrets: true,
		RedactKeys:    []string{"token"},
	}

	applyConfig(fs, cfg, false, label, traceFile, noTrace, redactSecrets, &redactKeys)

	if *label != "" {
		t.Fatal("expected label to remain unchanged")
	}

	if *traceFile != "" {
		t.Fatal("expected trace-file to remain unchanged")
	}

	if *noTrace {
		t.Fatal("expected no-trace=false")
	}

	if *redactSecrets {
		t.Fatal("expected redact-secrets=false")
	}

	if len(redactKeys) != 0 {
		t.Fatal("expected no redact keys")
	}
}

func TestApplyConfigExplicitFalseBoolOverridesConfig(t *testing.T) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)

	label := fs.String("label", "", "")
	traceFile := fs.String("trace-file", "", "")
	noTrace := fs.Bool("no-trace", false, "")
	redactSecrets := fs.Bool("redact-secrets", false, "")
	var redactKeys redactKeysFlag
	fs.Var(&redactKeys, "redact-key", "")

	if err := fs.Parse([]string{"--no-trace=false"}); err != nil {
		t.Fatal(err)
	}

	cfg := config{
		NoTrace: true,
	}

	applyConfig(fs, cfg, true, label, traceFile, noTrace, redactSecrets, &redactKeys)

	if *noTrace {
		t.Fatal("expected explicit --no-trace=false to override config")
	}
}

func TestApplyConfigExplicitRedactKeyOverridesConfig(t *testing.T) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	label := fs.String("label", "", "")
	traceFile := fs.String("trace-file", "", "")
	noTrace := fs.Bool("no-trace", false, "")
	redactSecrets := fs.Bool("redact-secrets", false, "")
	var redactKeys redactKeysFlag
	fs.Var(&redactKeys, "redact-key", "")

	if err := fs.Parse([]string{"--redact-key=cli-key"}); err != nil {
		t.Fatal(err)
	}
	cfg := config{RedactKeys: []string{"config-key"}}

	applyConfig(fs, cfg, true, label, traceFile, noTrace, redactSecrets, &redactKeys)

	if len(redactKeys) != 1 || redactKeys[0] != "cli-key" {
		t.Fatalf("expected explicit redact-key to win, got %v", redactKeys)
	}
}

func TestParseConfigRejectsArraySyntax(t *testing.T) {
	_, err := parseConfig(strings.NewReader(`
redact-key = ["token", "authorization"]
`))

	if err == nil {
		t.Fatal("expected error")
	}
}

func TestParseConfigRejectsTrailingContentAfterQuote(t *testing.T) {
	_, err := parseConfig(strings.NewReader(`
label = "fs" # my server
`))

	if err == nil {
		t.Fatal("expected error")
	}
}

func TestParseConfigRejectsUnterminatedQuotedValue(t *testing.T) {
	_, err := parseConfig(strings.NewReader(`
label = "fs
`))

	if err == nil {
		t.Fatal("expected error")
	}
}

func TestParseConfigRejectsTrailingCommentOnUnquotedValue(t *testing.T) {
	_, err := parseConfig(strings.NewReader(`
redact-key = token,api_key # note
`))

	if err == nil {
		t.Fatal("expected error")
	}
}
