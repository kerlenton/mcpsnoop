// Command mcpsnoop is a transparent proxy debugger for MCP traffic.
//
// Two modes in one binary.
//
//	mcpsnoop -- <server command>   run as a transparent stdio shim (the client
//	                              spawns this, and it proxies stdio to the real
//	                              server and traces every JSON-RPC frame).
//	mcpsnoop                       run the live TUI in your terminal, collecting
//	                              traffic from all shims and past sessions.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"

	"github.com/kerlenton/mcpsnoop/internal/exporter"
	"github.com/kerlenton/mcpsnoop/internal/paths"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/store"
	"github.com/kerlenton/mcpsnoop/internal/tui"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

// appVersion resolves the version to report. It uses the value baked in by
// -ldflags (release builds and `make build`), else the module version embedded
// by `go install ...@vX`, else "dev" for a plain local build.
func appVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return version
}

type redactKeysFlag []string

func (f *redactKeysFlag) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(*f, ",")
}

func (f *redactKeysFlag) Set(value string) error {
	for _, key := range strings.Split(value, ",") {
		key = strings.TrimSpace(key)
		if key != "" {
			*f = append(*f, key)
		}
	}
	return nil
}

type redactValuesFlag []string

func (f *redactValuesFlag) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(*f, ",")
}

func (f *redactValuesFlag) Set(value string) error {
	pattern := strings.TrimSpace(value)
	if pattern == "" {
		return nil
	}
	if _, err := regexp.Compile(pattern); err != nil {
		return fmt.Errorf("invalid redact value regex %q: %w", pattern, err)
	}
	*f = append(*f, pattern)
	return nil
}

func redactConfig(commonSecrets bool, keys redactKeysFlag, values redactValuesFlag) proxy.RedactConfig {
	return proxy.RedactConfig{
		CommonSecrets: commonSecrets,
		Keys:          []string(keys),
		ValuePatterns: []string(values),
	}
}

func main() {
	// `mcpsnoop http ...` is a separate subcommand with its own flags.
	if args := os.Args[1:]; len(args) > 0 && args[0] == "http" {
		os.Exit(runHTTP(args[1:]))
	}
	// `mcpsnoop export` renders a captured JSONL session to json/html/text/otlp.
	if args := os.Args[1:]; len(args) > 0 && args[0] == "export" {
		os.Exit(runExport(args[1:]))
	}
	// `mcpsnoop open` opens a session id or file directly in the TUI.
	if args := os.Args[1:]; len(args) > 0 && args[0] == "open" {
		os.Exit(runOpen(args[1:]))
	}
	// `mcpsnoop remote` prints the SSH reverse tunnel command for live remote view.
	if args := os.Args[1:]; len(args) > 0 && args[0] == "remote" {
		os.Exit(runRemote(args[1:]))
	}
	// `mcpsnoop version` mirrors the --version flag (what most CLIs expect).
	if args := os.Args[1:]; len(args) == 1 && (args[0] == "version" || args[0] == "-v") {
		fmt.Println("mcpsnoop", appVersion())
		return
	}
	// `mcpsnoop demo` plays a scripted session, no client or server to set up.
	if args := os.Args[1:]; len(args) == 1 && args[0] == "demo" {
		os.Exit(runDemo())
	}

	fs := flag.NewFlagSet("mcpsnoop", flag.ExitOnError)
	var redactKeys redactKeysFlag
	var redactValues redactValuesFlag
	var (
		label         = fs.String("label", "", "server label shown in the TUI (default: command name)")
		traceFile     = fs.String("trace-file", "", "override the JSONL trace path (default: well-known session log)")
		noTrace       = fs.Bool("no-trace", false, "disable tracing; pure passthrough")
		redactSecrets = fs.Bool("redact-secrets", false, "scrub common secret JSON keys in trace payloads")
		showVer       = fs.Bool("version", false, "print version and exit")
	)
	fs.Var(&redactKeys, "redact-key", "JSON key name to scrub in saved trace payloads (repeat or comma-separated)")
	fs.Var(&redactValues, "redact-value", "regular expression to scrub inside observed string values, stderr, and non-JSON text (repeatable)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "mcpsnoop %s · Wireshark for MCP\n\n", appVersion())
		fmt.Fprintf(os.Stderr, "Usage:\n")
		cmds := []struct{ use, desc string }{
			{"mcpsnoop [flags] -- <server command> [args...]", "run as a transparent stdio shim"},
			{"mcpsnoop http --target <url> [--listen :7000]", "run as a transparent HTTP proxy"},
			{"mcpsnoop export [-T json|html|text|otlp] [-o file|-] [session-id|log.jsonl]", ""},
			{"mcpsnoop open [session-id|log.jsonl|-]", "open a session in the TUI"},
			{"mcpsnoop remote [flags] <user@host>", "print the SSH tunnel command"},
			{"mcpsnoop", "run the live TUI (collector)"},
			{"mcpsnoop demo", "play a scripted session (no setup)"},
			{"mcpsnoop version", "print the version"},
			{"mcpsnoop help [command]", "show help for mcpsnoop or a command"},
		}
		col := 0
		for _, c := range cmds {
			if c.desc != "" && len(c.use) > col {
				col = len(c.use)
			}
		}
		for _, c := range cmds {
			if c.desc == "" {
				fmt.Fprintf(os.Stderr, "  %s\n", c.use)
				continue
			}
			fmt.Fprintf(os.Stderr, "  %-*s   %s\n", col, c.use, c.desc)
		}
		fmt.Fprintf(os.Stderr, "\nFlags:\n")
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nThe shim flags above can also be set in a .mcpsnoop.toml file in the\ncurrent directory. See the README for details.\n")
	}
	_ = fs.Parse(os.Args[1:])

	if *showVer {
		fmt.Println("mcpsnoop", appVersion())
		return
	}

	// `mcpsnoop help [command]` prints usage, so help is discoverable without -h
	// and "help" is never mistaken for a server command to run. A `--` means the
	// user is explicitly wrapping a command, so `mcpsnoop -- help` still runs it.
	if rest := fs.Args(); len(rest) > 0 && rest[0] == "help" && !slices.Contains(os.Args[1:], "--") {
		switch {
		case len(rest) < 2:
			fs.Usage()
		case rest[1] == "http":
			runHTTP([]string{"-h"})
		case rest[1] == "export":
			runExport([]string{"-h"})
		case rest[1] == "open":
			runOpen([]string{"-h"})
		case rest[1] == "remote":
			runRemote([]string{"-h"})
		default:
			fs.Usage()
		}
		return
	}

	if command := fs.Args(); len(command) > 0 {
		cfg, ok, err := loadConfig()
		if err != nil {
			fmt.Fprintln(os.Stderr, "mcpsnoop:", err)
			os.Exit(1)
		}

		applyConfig(
			fs,
			cfg,
			ok,
			label,
			traceFile,
			noTrace,
			redactSecrets,
			&redactKeys,
		)

		os.Exit(runShim(
			command,
			*label,
			*traceFile,
			*noTrace,
			redactConfig(*redactSecrets, redactKeys, redactValues),
		))
	}
	os.Exit(runHub())
}

// runnerNames are launchers we skip when guessing a session label, so wrapping
// `npx -y @scope/server-foo` shows "server-foo" rather than "npx".
var runnerNames = map[string]bool{
	"npx": true, "npm": true, "pnpm": true, "yarn": true, "bunx": true, "bun": true,
	"node": true, "deno": true, "python": true, "python3": true, "uv": true,
	"uvx": true, "pipx": true, "sh": true, "bash": true, "env": true, "go": true,
}

// labelFor derives a friendly session name from the wrapped command. It skips
// runners/flags and prefers a token that looks like a server (contains "server"
// or "mcp", an @scope/name, or a script file), falling back to the first real
// argument or the command itself.
func labelFor(command []string) string {
	var cands []string
	for i, a := range command {
		if strings.HasPrefix(a, "-") || a == "run" || a == "exec" || a == "-m" {
			continue
		}
		if runnerNames[filepath.Base(a)] && (i == 0 || len(cands) == 0) {
			continue
		}
		cands = append(cands, a)
	}
	pick := ""
	for _, c := range cands {
		lc := strings.ToLower(c)
		if strings.Contains(lc, "server") || strings.Contains(lc, "mcp") ||
			strings.HasPrefix(c, "@") || strings.HasSuffix(lc, ".js") ||
			strings.HasSuffix(lc, ".ts") || strings.HasSuffix(lc, ".py") {
			pick = c
			break
		}
	}
	if pick == "" && len(cands) > 0 {
		pick = cands[0]
	}
	if pick == "" {
		pick = command[0]
	}
	if i := strings.LastIndexAny(pick, "/\\"); i >= 0 {
		pick = pick[i+1:]
	}
	if pick == "" {
		return filepath.Base(command[0])
	}
	return pick
}

// runExport reads a persisted JSONL session and writes a portable export.
func runExport(args []string) int {
	fs := flag.NewFlagSet("mcpsnoop export", flag.ExitOnError)
	var (
		formatFlag = fs.String("T", "json", "output format: json, html, text, or otlp")
		outFlag    = fs.String("o", "-", "output path, or - for stdout")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: mcpsnoop export [-T json|html|text|otlp] [-o file|-] [session-id|log.jsonl]\n\n")
		fmt.Fprintf(os.Stderr, "If no session is provided, the newest session log is exported.\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if fs.NArg() > 1 {
		fmt.Fprintln(os.Stderr, "mcpsnoop export: expected at most one session id or log path")
		return 2
	}
	format, err := exporter.ParseFormat(*formatFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcpsnoop export:", err)
		return 2
	}
	var arg string
	if fs.NArg() == 1 {
		arg = fs.Arg(0)
	}
	inPath, err := exporter.ResolveSessionPath(arg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcpsnoop export:", err)
		return 1
	}

	var out *os.File
	if *outFlag == "-" {
		out = os.Stdout
	} else {
		if err := os.MkdirAll(filepath.Dir(*outFlag), 0o700); err != nil {
			fmt.Fprintln(os.Stderr, "mcpsnoop export:", err)
			return 1
		}
		f, err := os.OpenFile(*outFlag, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mcpsnoop export:", err)
			return 1
		}
		defer f.Close()
		out = f
	}
	if err := exporter.ExportFile(inPath, out, exporter.Options{Format: format}); err != nil {
		fmt.Fprintln(os.Stderr, "mcpsnoop export:", err)
		return 1
	}
	return 0
}

// runShim runs the transparent stdio proxy. It writes the durable session log
// AND streams live to the hub. Neither has to be running first.
func runShim(command []string, label, traceFile string, noTrace bool, redaction proxy.RedactConfig) int {
	if label == "" {
		label = labelFor(command)
	}
	sessionID := fmt.Sprintf("%s-%d", label, os.Getpid())

	sink := traceSink(sessionID, traceFile, noTrace, redaction)
	defer sink.Close()
	if !noTrace {
		fmt.Fprintf(os.Stderr, "mcpsnoop: tracing %q (session %s)\n", strings.Join(command, " "), sessionID)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	code, err := proxy.RunStdio(ctx, proxy.StdioConfig{
		Command:   command,
		Label:     label,
		SessionID: sessionID,
		Sink:      sink,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcpsnoop: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// traceSink builds the shared sink, a durable per-session JSONL log plus a
// best-effort live stream to the hub. Returns a no-op sink when disabled.
func traceSink(sessionID, traceFile string, noTrace bool, redaction proxy.RedactConfig) proxy.Sink {
	if noTrace {
		return proxy.NopSink()
	}
	if traceFile == "" {
		traceFile = paths.SessionLogPath(sessionID)
	}
	var sinks []proxy.Sink
	if f, err := os.OpenFile(traceFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "mcpsnoop: cannot open trace file %q: %v (continuing without file trace)\n", traceFile, err)
	} else {
		sinks = append(sinks, proxy.NewAsyncSink(f, 0))
	}
	sinks = append(sinks, proxy.NewSocketSink(paths.SocketPath(), 0))
	sink := proxy.Sink(proxy.NewMultiSink(sinks...))
	if redaction.Enabled() {
		sink = proxy.NewRedactingSink(sink, redaction)
	}
	return sink
}

// runHTTP runs the transparent HTTP proxy subcommand.
func runHTTP(args []string) int {
	fs := flag.NewFlagSet("mcpsnoop http", flag.ExitOnError)
	var redactKeys redactKeysFlag
	var redactValues redactValuesFlag
	var (
		target        = fs.String("target", "", "real MCP server endpoint, e.g. http://localhost:3000/mcp (required)")
		listen        = fs.String("listen", ":7000", "address to listen on")
		label         = fs.String("label", "", "server label shown in the TUI (default: target host)")
		noTrace       = fs.Bool("no-trace", false, "disable tracing; pure passthrough")
		redactSecrets = fs.Bool("redact-secrets", false, "scrub common secret JSON keys in trace payloads")
	)
	fs.Var(&redactKeys, "redact-key", "JSON key name to scrub in saved trace payloads (repeat or comma-separated)")
	fs.Var(&redactValues, "redact-value", "regular expression to scrub inside observed string values, stderr, and non-JSON text (repeatable)")
	_ = fs.Parse(args)
	cfg, ok, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcpsnoop http:", err)
		return 1
	}

	applyConfig(
		fs,
		cfg,
		ok,
		label,
		nil,
		noTrace,
		redactSecrets,
		&redactKeys,
	)
	if *target == "" {
		fmt.Fprintln(os.Stderr, "mcpsnoop http: --target is required")
		return 2
	}
	lbl := *label
	if lbl == "" {
		if u, err := url.Parse(*target); err == nil && u.Host != "" {
			lbl = u.Host
		} else {
			lbl = "http"
		}
	}
	sessionID := fmt.Sprintf("%s-%d", lbl, os.Getpid())

	sink := traceSink(sessionID, "", *noTrace, redactConfig(*redactSecrets, redactKeys, redactValues))
	defer sink.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(os.Stderr, "mcpsnoop: proxying %s → %s (session %s)\n", *listen, *target, sessionID)
	if err := proxy.RunHTTP(ctx, proxy.HTTPConfig{
		Listen:    *listen,
		Target:    *target,
		Label:     lbl,
		SessionID: sessionID,
		Sink:      sink,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "mcpsnoop: %v\n", err)
		return 1
	}
	return 0
}

// runHub runs the live TUI, collecting traffic from all shims and past sessions.
func runHub() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := tui.Run(ctx, paths.SocketPath(), paths.SessionsDir(), 0); err != nil {
		fmt.Fprintf(os.Stderr, "mcpsnoop: %v\n", err)
		return 1
	}
	return 0
}

// runOpen opens a persisted JSONL session directly in the TUI.
func runOpen(args []string) int {
	fs := flag.NewFlagSet("mcpsnoop open", flag.ExitOnError)

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: mcpsnoop open [session-id|session.jsonl|-]\n\n")
		fmt.Fprintf(os.Stderr, "If no session is provided, the newest session log is opened.\n")
		fmt.Fprintf(os.Stderr, "Use - to read from stdin.\n")
	}

	_ = fs.Parse(args)

	if fs.NArg() > 1 {
		fmt.Fprintln(os.Stderr, "mcpsnoop open: expected at most one session id or log path")
		return 2
	}

	var arg string
	if fs.NArg() == 1 {
		arg = fs.Arg(0)
	}
	inPath, usedStdin, err := resolveOpenSessionPath(arg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcpsnoop open:", err)
		return 1
	}

	var r io.Reader
	if usedStdin {
		r = os.Stdin
	} else {
		f, err := os.Open(inPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mcpsnoop open:", err)
			return 1
		}
		defer f.Close()
		r = f
	}

	st := store.New(0)
	if err := proxy.Decode(r, func(e proxy.Envelope) {
		st.Ingest(e)
	}); err != nil {
		fmt.Fprintln(os.Stderr, "mcpsnoop open:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if usedStdin {
		tty, err := openTTY()
		if err != nil {
			fmt.Fprintln(os.Stderr, "mcpsnoop open:", err)
			return 1
		}
		defer tty.Close()
		if err := tui.RunOpenWithInput(ctx, st, tty); err != nil {
			fmt.Fprintln(os.Stderr, "mcpsnoop open:", err)
			return 1
		}
	} else {
		if err := tui.RunOpen(ctx, st); err != nil {
			fmt.Fprintln(os.Stderr, "mcpsnoop open:", err)
			return 1
		}
	}

	return 0
}

func resolveOpenSessionPath(arg string) (string, bool, error) {
	if arg == "-" {
		return "", true, nil
	}
	path, err := exporter.ResolveSessionPath(arg)
	return path, false, err
}
