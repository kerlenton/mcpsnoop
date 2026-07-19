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

	if len(cfg.RedactPaths) != 0 {
		t.Fatal("expected no redact paths")
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

func TestParseConfigRedactPaths(t *testing.T) {
	cfg, err := parseConfig(strings.NewReader(`
redact-path = "$.params.arguments.password"
redact-path = "$.result.token"
`))
	if err != nil {
		t.Fatal(err)
	}

	if got, want := cfg.RedactPaths.String(), "$.params.arguments.password,$.result.token"; got != want {
		t.Fatalf("RedactPaths = %q, want %q", got, want)
	}
}

func TestParseConfigRejectsInvalidRedactPath(t *testing.T) {
	_, err := parseConfig(strings.NewReader(`redact-path = "$.["`))
	if err == nil {
		t.Fatal("parseConfig returned nil error")
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
	var redactValues redactValuesFlag
	var redactPaths redactPathsFlag
	fs.Var(&redactKeys, "redact-key", "")
	fs.Var(&redactValues, "redact-value", "")
	fs.Var(&redactPaths, "redact-path", "")

	cfg := config{
		Label:         "filesystem",
		TraceFile:     "trace.jsonl",
		NoTrace:       true,
		RedactSecrets: true,
		RedactKeys:    []string{"token", "api_key"},
	}
	if err := cfg.RedactPaths.Set("$.params.arguments.password"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.RedactValues.Set("sk-[0-9]+"); err != nil {
		t.Fatal(err)
	}

	applyConfig(fs, cfg, true, label, traceFile, noTrace, redactSecrets, &redactKeys, &redactValues, &redactPaths)

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

	if got, want := redactPaths.String(), "$.params.arguments.password"; got != want {
		t.Fatalf("redact paths = %q, want %q", got, want)
	}

	if got, want := redactValues.String(), "sk-[0-9]+"; got != want {
		t.Fatalf("redact values = %q, want %q", got, want)
	}
}

func TestParseConfigAccumulatesRedactKeysAndValues(t *testing.T) {
	cfg, err := parseConfig(strings.NewReader(
		"redact-key = \"token\"\nredact-key = \"api_key\"\nredact-value = \"sk-[0-9]+\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	// Both redact-key lines take effect rather than the last silently winning.
	if len(cfg.RedactKeys) != 2 || cfg.RedactKeys[0] != "token" || cfg.RedactKeys[1] != "api_key" {
		t.Fatalf("redact-key lines did not accumulate: %v", cfg.RedactKeys)
	}
	if len(cfg.RedactValues) != 1 || cfg.RedactValues[0] != "sk-[0-9]+" {
		t.Fatalf("redact-value not parsed: %v", cfg.RedactValues)
	}
}

func TestParseConfigRejectsInvalidRedactValue(t *testing.T) {
	if _, err := parseConfig(strings.NewReader("redact-value = \"([\"\n")); err == nil {
		t.Fatal("an invalid redact-value regex should be rejected, the same as the flag")
	}
}

func TestApplyConfigExplicitFlagOverridesConfig(t *testing.T) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)

	label := fs.String("label", "", "")
	traceFile := fs.String("trace-file", "", "")
	noTrace := fs.Bool("no-trace", false, "")
	redactSecrets := fs.Bool("redact-secrets", false, "")
	var redactKeys redactKeysFlag
	var redactValues redactValuesFlag
	var redactPaths redactPathsFlag
	fs.Var(&redactKeys, "redact-key", "")
	fs.Var(&redactValues, "redact-value", "")
	fs.Var(&redactPaths, "redact-path", "")

	if err := fs.Parse([]string{"--label=cli"}); err != nil {
		t.Fatal(err)
	}

	cfg := config{
		Label: "from-config",
	}

	applyConfig(fs, cfg, true, label, traceFile, noTrace, redactSecrets, &redactKeys, &redactValues, &redactPaths)

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
	var redactValues redactValuesFlag
	var redactPaths redactPathsFlag
	fs.Var(&redactKeys, "redact-key", "")
	fs.Var(&redactValues, "redact-value", "")
	fs.Var(&redactPaths, "redact-path", "")

	cfg := config{
		Label:         "filesystem",
		TraceFile:     "trace.jsonl",
		NoTrace:       true,
		RedactSecrets: true,
		RedactKeys:    []string{"token"},
	}

	applyConfig(fs, cfg, false, label, traceFile, noTrace, redactSecrets, &redactKeys, &redactValues, &redactPaths)

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
	var redactValues redactValuesFlag
	var redactPaths redactPathsFlag
	fs.Var(&redactKeys, "redact-key", "")
	fs.Var(&redactValues, "redact-value", "")
	fs.Var(&redactPaths, "redact-path", "")

	if err := fs.Parse([]string{"--no-trace=false"}); err != nil {
		t.Fatal(err)
	}

	cfg := config{
		NoTrace: true,
	}

	applyConfig(fs, cfg, true, label, traceFile, noTrace, redactSecrets, &redactKeys, &redactValues, &redactPaths)

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
	var redactValues redactValuesFlag
	var redactPaths redactPathsFlag
	fs.Var(&redactKeys, "redact-key", "")
	fs.Var(&redactValues, "redact-value", "")
	fs.Var(&redactPaths, "redact-path", "")

	if err := fs.Parse([]string{"--redact-key=cli-key"}); err != nil {
		t.Fatal(err)
	}
	cfg := config{RedactKeys: []string{"config-key"}}

	applyConfig(fs, cfg, true, label, traceFile, noTrace, redactSecrets, &redactKeys, &redactValues, &redactPaths)

	if len(redactKeys) != 1 || redactKeys[0] != "cli-key" {
		t.Fatalf("expected explicit redact-key to win, got %v", redactKeys)
	}
}

func TestApplyConfigExplicitRedactPathOverridesConfig(t *testing.T) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	label := fs.String("label", "", "")
	traceFile := fs.String("trace-file", "", "")
	noTrace := fs.Bool("no-trace", false, "")
	redactSecrets := fs.Bool("redact-secrets", false, "")
	var redactKeys redactKeysFlag
	var redactValues redactValuesFlag
	var redactPaths redactPathsFlag
	fs.Var(&redactKeys, "redact-key", "")
	fs.Var(&redactValues, "redact-value", "")
	fs.Var(&redactPaths, "redact-path", "")

	if err := fs.Parse([]string{"--redact-path=$.cli.password"}); err != nil {
		t.Fatal(err)
	}
	var configPaths redactPathsFlag
	if err := configPaths.Set("$.config.password"); err != nil {
		t.Fatal(err)
	}
	cfg := config{RedactPaths: configPaths}

	applyConfig(fs, cfg, true, label, traceFile, noTrace, redactSecrets, &redactKeys, &redactValues, &redactPaths)

	if got, want := redactPaths.String(), "$.cli.password"; got != want {
		t.Fatalf("redact paths = %q, want %q", got, want)
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
