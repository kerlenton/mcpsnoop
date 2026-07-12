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
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

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

func (f *redactKeysFlag) Type() string { return "strings" }

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

func (f *redactValuesFlag) Type() string { return "regexp" }

func redactConfig(commonSecrets bool, keys redactKeysFlag, values redactValuesFlag) proxy.RedactConfig {
	return proxy.RedactConfig{
		CommonSecrets: commonSecrets,
		Keys:          []string(keys),
		ValuePatterns: []string(values),
	}
}

func main() { os.Exit(execute(os.Args[1:])) }

// runShimFn and runHubFn are indirected so tests can check how the root command
// routes the wrapped command without spawning a server or launching the TUI.
var (
	runShimFn = runShim
	runHubFn  = runHub
)

// exitCode carries a command's process exit code out through cobra's error
// return so main can hand it to os.Exit unchanged.
type exitCode int

func (c exitCode) Error() string { return fmt.Sprintf("exit status %d", int(c)) }

func codeOf(code int) error {
	if code == 0 {
		return nil
	}
	return exitCode(code)
}

func execute(args []string) int {
	tui.Version = appVersion() // surfaced in the help overlay
	root := newRootCmd()
	root.SetArgs(args)
	root.SilenceErrors = true
	err := root.Execute()
	var code exitCode
	if errors.As(err, &code) {
		return int(code)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcpsnoop:", err)
		return 2
	}
	return 0
}

func newRootCmd() *cobra.Command {
	var (
		label, traceFile       string
		noTrace, redactSecrets bool
		redactKeys             redactKeysFlag
		redactValues           redactValuesFlag
	)
	cmd := &cobra.Command{
		Use:   "mcpsnoop [flags] -- <server command> [args...]",
		Short: "Wireshark for MCP, a transparent proxy and TUI for debugging MCP traffic",
		Long: `mcpsnoop is a transparent proxy debugger for MCP traffic.

Wrap your server with "mcpsnoop -- <server command>" and it forwards stdio byte
for byte while tracing every JSON-RPC frame. Run "mcpsnoop" with no arguments to
open the live TUI that collects traffic from every shim and past sessions.

Repeated shim flags can live in a .mcpsnoop.toml file in the current directory.`,
		Version:      appVersion(),
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return codeOf(runHubFn())
			}
			cfg, ok, err := loadConfig()
			if err != nil {
				fmt.Fprintln(os.Stderr, "mcpsnoop:", err)
				return exitCode(1)
			}
			applyConfig(cmd.Flags(), cfg, ok, &label, &traceFile, &noTrace, &redactSecrets, &redactKeys)
			return codeOf(runShimFn(args, label, traceFile, noTrace, redactConfig(redactSecrets, redactKeys, redactValues)))
		},
	}
	flags := cmd.Flags()
	flags.SortFlags = false
	flags.StringVar(&label, "label", "", "server label shown in the TUI, defaults to the command name")
	flags.StringVar(&traceFile, "trace-file", "", "override the JSONL trace path, defaults to the well-known session log")
	flags.BoolVar(&noTrace, "no-trace", false, "disable tracing, pure passthrough")
	flags.BoolVar(&redactSecrets, "redact-secrets", false, "scrub common secret JSON keys in trace payloads")
	flags.Var(&redactKeys, "redact-key", "JSON key name to scrub in saved trace payloads, repeat or comma-separated")
	flags.Var(&redactValues, "redact-value", "regular expression to scrub inside observed string values, stderr, and non-JSON text, repeatable")
	// Stop parsing at the first positional so the wrapped command keeps its flags.
	flags.SetInterspersed(false)

	cmd.SetVersionTemplate("mcpsnoop {{.Version}}\n")
	cmd.AddCommand(newHTTPCmd(), newExportCmd(), newCheckCmd(), newOpenCmd(), newRemoteCmd(), newDemoCmd(), newVersionCmd())
	return cmd
}

func newDemoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "demo",
		Short: "Play a scripted session in the TUI, no setup",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return codeOf(runDemo()) },
	}
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		Run:   func(cmd *cobra.Command, args []string) { fmt.Println("mcpsnoop", appVersion()) },
	}
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

// newExportCmd reads a persisted JSONL session and writes a portable export.
func newExportCmd() *cobra.Command {
	var formatFlag, outFlag string
	cmd := &cobra.Command{
		Use:   "export [session-id|log.jsonl]",
		Short: "Render a captured session to json, html, text, or otlp",
		Long:  "Render a captured session to a portable file. With no session, the newest session log is exported.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := exporter.ParseFormat(formatFlag)
			if err != nil {
				fmt.Fprintln(os.Stderr, "mcpsnoop export:", err)
				return exitCode(2)
			}
			var arg string
			if len(args) == 1 {
				arg = args[0]
			}
			inPath, err := exporter.ResolveSessionPath(arg)
			if err != nil {
				fmt.Fprintln(os.Stderr, "mcpsnoop export:", err)
				return exitCode(1)
			}

			out := os.Stdout
			if outFlag != "-" {
				if err := os.MkdirAll(filepath.Dir(outFlag), 0o700); err != nil {
					fmt.Fprintln(os.Stderr, "mcpsnoop export:", err)
					return exitCode(1)
				}
				f, err := os.OpenFile(outFlag, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
				if err != nil {
					fmt.Fprintln(os.Stderr, "mcpsnoop export:", err)
					return exitCode(1)
				}
				defer f.Close()
				out = f
			}
			if err := exporter.ExportFile(inPath, out, exporter.Options{Format: format}); err != nil {
				fmt.Fprintln(os.Stderr, "mcpsnoop export:", err)
				return exitCode(1)
			}
			return nil
		},
	}
	cmd.Flags().SortFlags = false
	cmd.Flags().StringVarP(&formatFlag, "format", "T", "json", "output format, one of json, html, text, otlp")
	cmd.Flags().StringVarP(&outFlag, "output", "o", "-", "output path, or - for stdout")
	return cmd
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

// newHTTPCmd runs the transparent HTTP proxy for a streamable-HTTP MCP server.
func newHTTPCmd() *cobra.Command {
	var (
		target, listen, label  string
		noTrace, redactSecrets bool
		redactKeys             redactKeysFlag
		redactValues           redactValuesFlag
	)
	cmd := &cobra.Command{
		Use:   "http --target <url> [--listen :7000]",
		Short: "Run as a transparent HTTP proxy for a streamable-HTTP MCP server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, ok, err := loadConfig()
			if err != nil {
				fmt.Fprintln(os.Stderr, "mcpsnoop http:", err)
				return exitCode(1)
			}
			applyConfig(cmd.Flags(), cfg, ok, &label, nil, &noTrace, &redactSecrets, &redactKeys)
			if target == "" {
				fmt.Fprintln(os.Stderr, "mcpsnoop http: --target is required")
				return exitCode(2)
			}
			lbl := label
			if lbl == "" {
				if u, err := url.Parse(target); err == nil && u.Host != "" {
					lbl = u.Host
				} else {
					lbl = "http"
				}
			}
			sessionID := fmt.Sprintf("%s-%d", lbl, os.Getpid())

			sink := traceSink(sessionID, "", noTrace, redactConfig(redactSecrets, redactKeys, redactValues))
			defer sink.Close()

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			fmt.Fprintf(os.Stderr, "mcpsnoop: proxying %s → %s (session %s)\n", listen, target, sessionID)
			if err := proxy.RunHTTP(ctx, proxy.HTTPConfig{
				Listen:    listen,
				Target:    target,
				Label:     lbl,
				SessionID: sessionID,
				Sink:      sink,
			}); err != nil {
				fmt.Fprintf(os.Stderr, "mcpsnoop: %v\n", err)
				return exitCode(1)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.SortFlags = false
	f.StringVar(&target, "target", "", "real MCP server endpoint, for example http://localhost:3000/mcp (required)")
	f.StringVar(&listen, "listen", ":7000", "address to listen on")
	f.StringVar(&label, "label", "", "server label shown in the TUI, defaults to the target host")
	f.BoolVar(&noTrace, "no-trace", false, "disable tracing, pure passthrough")
	f.BoolVar(&redactSecrets, "redact-secrets", false, "scrub common secret JSON keys in trace payloads")
	f.Var(&redactKeys, "redact-key", "JSON key name to scrub in saved trace payloads, repeat or comma-separated")
	f.Var(&redactValues, "redact-value", "regular expression to scrub inside observed string values, stderr, and non-JSON text, repeatable")
	return cmd
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

// newOpenCmd opens a persisted JSONL session directly in the TUI.
func newOpenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "open [session-id|session.jsonl|-]",
		Short: "Open a captured session in the TUI, or - to read from stdin",
		Long:  "Open a captured session in the TUI. With no session, the newest session log is opened. Use - to read from stdin.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var arg string
			if len(args) == 1 {
				arg = args[0]
			}
			return codeOf(runOpen(arg))
		},
	}
}

// runOpen loads a session (id, path, or - for stdin) and shows it in the TUI.
func runOpen(arg string) int {
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
