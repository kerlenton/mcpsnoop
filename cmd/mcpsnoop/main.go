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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/kerlenton/mcpsnoop/internal/exporter"
	"github.com/kerlenton/mcpsnoop/internal/hub"
	"github.com/kerlenton/mcpsnoop/internal/otlpsink"
	"github.com/kerlenton/mcpsnoop/internal/paths"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/replay"
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

type redactPathsFlag []proxy.RedactPath

func (f *redactPathsFlag) String() string {
	if f == nil {
		return ""
	}
	paths := make([]string, len(*f))
	for i, path := range *f {
		paths[i] = path.String()
	}
	return strings.Join(paths, ",")
}

func (f *redactPathsFlag) Set(value string) error {
	path, err := proxy.ParseRedactPath(value)
	if err != nil {
		return fmt.Errorf("invalid redact JSONPath %q: %w", value, err)
	}
	*f = append(*f, path)
	return nil
}

func (f *redactPathsFlag) Type() string { return "jsonpath" }

type otlpHeadersFlag http.Header

func (f *otlpHeadersFlag) String() string {
	if f == nil {
		return ""
	}
	var headers []string
	for name, values := range http.Header(*f) {
		for _, value := range values {
			headers = append(headers, name+"="+value)
		}
	}
	sort.Strings(headers)
	return strings.Join(headers, ",")
}

func (f *otlpHeadersFlag) Set(value string) error {
	name, headerValue, ok := strings.Cut(value, "=")
	name = strings.TrimSpace(name)
	headerValue = strings.TrimSpace(headerValue)
	if !ok || !validHeaderName(name) || strings.ContainsAny(headerValue, "\r\n") {
		return fmt.Errorf("invalid OTLP header %q, want Name=Value", value)
	}
	if *f == nil {
		*f = make(otlpHeadersFlag)
	}
	http.Header(*f).Add(name, headerValue)
	return nil
}

func (f *otlpHeadersFlag) Type() string { return "header" }

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && !strings.ContainsRune("!#$%&'*+-.^_`|~", r) {
			return false
		}
	}
	return true
}

type traceOptions struct {
	OTLPEndpoint string
	OTLPHeaders  http.Header
}

func parseTraceOptions(endpoint string, headers otlpHeadersFlag) (traceOptions, error) {
	if endpoint == "" {
		if len(headers) != 0 {
			return traceOptions{}, errors.New("--otlp-header requires --otlp-endpoint")
		}
		return traceOptions{}, nil
	}
	u, err := url.ParseRequestURI(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return traceOptions{}, fmt.Errorf("invalid OTLP endpoint %q, want an http(s) URL", endpoint)
	}
	return traceOptions{OTLPEndpoint: endpoint, OTLPHeaders: http.Header(headers)}, nil
}

func redactConfig(commonSecrets bool, keys redactKeysFlag, values redactValuesFlag, paths redactPathsFlag) proxy.RedactConfig {
	return proxy.RedactConfig{
		CommonSecrets: commonSecrets,
		Keys:          []string(keys),
		ValuePatterns: []string(values),
		Paths:         []proxy.RedactPath(paths),
	}
}

func main() { os.Exit(execute(os.Args[1:])) }

// Runner functions are indirected so tests can check routing and loaded state
// without spawning a server, launching the TUI, or binding a port. The HTTP
// seam matters most: without it a test that reaches RunHTTP binds the real
// --listen address and blocks until the package timeout, which fails the whole
// package rather than the one test.
var (
	runShimFn    = runShim
	runHubFn     = runHub
	runHTTPFn    = proxy.RunHTTP
	runOpenTUIFn = tui.RunOpen
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
	tui.Version = appVersion()      // surfaced in the help overlay
	exporter.Version = appVersion() // recorded as the HAR creator
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
		otlpEndpoint           string
		metricsListen          string
		noTrace, redactSecrets bool
		redactKeys             redactKeysFlag
		redactValues           redactValuesFlag
		redactPaths            redactPathsFlag
		otlpHeaders            otlpHeadersFlag
		historyLimit           int
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
				if historyLimit < 0 {
					return errors.New("--history-limit must be non-negative")
				}
				return codeOf(runHubFn(historyLimit, metricsListen))
			}
			if metricsListen != "" {
				return errors.New("--metrics-listen is only available when running the hub without a wrapped command")
			}
			cfg, ok, err := loadConfig()
			if err != nil {
				fmt.Fprintln(os.Stderr, "mcpsnoop:", err)
				return exitCode(1)
			}
			applyConfig(cmd.Flags(), cfg, ok, &label, &traceFile, &noTrace, &redactSecrets, &redactKeys, &redactValues, &redactPaths)
			// After applyConfig, so a label from a shared .mcpsnoop.toml is held
			// to the same rule as the flag.
			if err := paths.CheckLabel(label); err != nil {
				fmt.Fprintln(os.Stderr, "mcpsnoop:", err)
				return exitCode(2)
			}
			trace, err := parseTraceOptions(otlpEndpoint, otlpHeaders)
			if err != nil {
				return err
			}
			return codeOf(runShimFn(args, label, traceFile, noTrace, redactConfig(redactSecrets, redactKeys, redactValues, redactPaths), trace))
		},
	}
	flags := cmd.Flags()
	flags.SortFlags = false
	flags.StringVar(&label, "label", "", "server label shown in the TUI, defaults to the command name")
	flags.StringVar(&traceFile, "trace-file", "", "override the JSONL trace path, defaults to the well-known session log")
	flags.StringVar(&otlpEndpoint, "otlp-endpoint", "", "stream completed calls to an OTLP/HTTP JSON traces endpoint")
	flags.Var(&otlpHeaders, "otlp-header", "HTTP header for OTLP delivery as Name=Value, repeatable")
	flags.StringVar(&metricsListen, "metrics-listen", "", "listen on this address for Prometheus metrics in hub mode")
	flags.BoolVar(&noTrace, "no-trace", false, "disable tracing, pure passthrough")
	flags.BoolVar(&redactSecrets, "redact-secrets", false, "scrub common secret JSON keys in trace payloads")
	flags.Var(&redactKeys, "redact-key", "JSON key name to scrub in saved trace payloads, repeat or comma-separated")
	flags.Var(&redactValues, "redact-value", "regular expression to scrub inside observed string values, stderr, and non-JSON text, repeatable")
	flags.Var(&redactPaths, "redact-path", "JSONPath selecting values to scrub in saved trace payloads, repeatable")
	flags.IntVar(&historyLimit, "history-limit", hub.DefaultBackfillLimit, "maximum session logs to load in the TUI, 0 loads all")
	// Stop parsing at the first positional so the wrapped command keeps its flags.
	flags.SetInterspersed(false)

	cmd.SetVersionTemplate("mcpsnoop {{.Version}}\n")
	cmd.AddCommand(newHTTPCmd(), newExportCmd(), newCheckCmd(), newBaselineCmd(), newDiffCmd(), newOpenCmd(), newPruneCmd(), newInventoryCmd(), newStatsCmd(), newWrapCmd(), newUnwrapCmd(), newRemoteCmd(), newDemoCmd(), newVersionCmd())
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

// newSessionID names one proxy run: its log file, and the session the TUI and
// the export commands address.
//
// The PID is not enough on its own. A PID is unique only among live processes,
// and the kernel is free to reuse it the moment the old one is reaped, which
// happens fast wherever the PID space is small: containers, CI runners, and
// anything that starts a fresh server per job. Two runs that land on the same
// id then collide twice over. They append into one log file, and the hub, which
// deduplicates on a per-session high-water mark of Seq, sees the second run
// restart at Seq 1 and discards every frame of it as already seen. The live
// view stays empty for a server that is plainly running, and nothing reports
// the loss: the gap detector only notices Seq jumping forward, never back.
//
// The suffix is random rather than a start timestamp because the guarantee must
// not rest on the clock. Container clocks are coarse and can jump, and two
// shims can start inside the same tick. The PID stays in the id because it is
// the part a person uses to match a session to a process they can see.
func newSessionID(label string) string {
	return fmt.Sprintf("%s-%d-%s", label, os.Getpid(), sessionNonce())
}

// sessionNonce is the short random tag that tells apart two runs sharing a PID.
// crypto/rand.Read has no error path worth handling here: on every supported
// platform it either succeeds or panics, so there is nothing to degrade into.
//
// Six bytes, not three. A collision needs the label, the PID and the nonce to
// coincide. The case this exists for is a container where the server always
// starts under the same PID, hundreds of times a run, which holds the first two
// fixed and leaves the nonce carrying the whole guarantee. Three bytes is a 3%
// birthday collision over a thousand such runs, and a collision here is exactly
// the silent whole-session drop described above. Six settles the arithmetic for
// good, at the price of six characters in an id nobody types twice.
func sessionNonce() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// newExportCmd reads a persisted JSONL session and writes a portable export.
func newExportCmd() *cobra.Command {
	var (
		formatFlag, outFlag string
		redactSecrets       bool
		redactKeys          redactKeysFlag
		redactValues        redactValuesFlag
		redactPaths         redactPathsFlag
	)
	cmd := &cobra.Command{
		Use:   "export [session-id|log.jsonl|-]",
		Short: "Render a captured session to json, html, text, har, or otlp",
		Long:  "Render a captured session to a portable file. With no session, the newest session log is exported. Use - to read JSONL from stdin.",
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
			// Resolve a file or session before opening the output, so a bad
			// input never truncates the target. "-" streams stdin, like check.
			stdin := arg == "-"
			var inPath string
			if !stdin {
				inPath, err = exporter.ResolveSessionPath(arg)
				if err != nil {
					fmt.Fprintln(os.Stderr, "mcpsnoop export:", err)
					return exitCode(1)
				}
			}

			var (
				in     io.Reader
				source string
			)
			if stdin {
				in = cmd.InOrStdin()
				source = "stdin"
			} else {
				f, err := os.Open(inPath)
				if err != nil {
					fmt.Fprintln(os.Stderr, "mcpsnoop export:", err)
					return exitCode(1)
				}
				defer f.Close()
				in = f
				source = inPath
			}

			var out io.Writer = os.Stdout
			var target *exportTarget
			if outFlag != "-" {
				same, err := sameSource(in, inPath, outFlag)
				if err != nil {
					fmt.Fprintln(os.Stderr, "mcpsnoop export:", err)
					return exitCode(1)
				}
				if same {
					fmt.Fprintln(os.Stderr, "mcpsnoop export: input and output refer to the same file")
					return exitCode(1)
				}
				target, err = openExportTarget(outFlag)
				if err != nil {
					fmt.Fprintln(os.Stderr, "mcpsnoop export:", err)
					return exitCode(1)
				}
				defer target.abort()
				out = target
			}

			opts := exporter.Options{
				Format:    format,
				Redaction: redactConfig(redactSecrets, redactKeys, redactValues, redactPaths),
			}
			if err := exporter.Export(in, source, out, opts); err != nil {
				fmt.Fprintln(os.Stderr, "mcpsnoop export:", err)
				return exitCode(1)
			}
			if target != nil {
				if err := target.commit(); err != nil {
					fmt.Fprintln(os.Stderr, "mcpsnoop export:", err)
					return exitCode(1)
				}
			}
			return nil
		},
	}
	cmd.Flags().SortFlags = false
	cmd.Flags().StringVarP(&formatFlag, "format", "T", "json", "output format, one of json, html, text, har, otlp")
	cmd.Flags().StringVarP(&outFlag, "output", "o", "-", "output path, or - for stdout")
	cmd.Flags().BoolVar(&redactSecrets, "redact-secrets", false, "scrub common secret JSON keys in captured JSON-RPC payloads")
	cmd.Flags().Var(&redactKeys, "redact-key", "JSON key name to scrub in captured JSON-RPC payloads, repeat or comma-separated")
	cmd.Flags().Var(&redactValues, "redact-value", "regular expression to scrub inside captured JSON-RPC string values, stderr, and non-JSON text, repeatable")
	cmd.Flags().Var(&redactPaths, "redact-path", "JSONPath selecting values in captured JSON-RPC payloads to scrub, repeatable")
	return cmd
}

// sameSource reports whether the export would write over the file it is reading.
// Both sides are compared by stat, so two spellings of one path, a symlink and a
// hard link all reach the same answer, and a stdin the shell redirected from a
// real file is caught as well as a path given on the command line.
//
// A stdin arriving down a pipe cannot be traced back to a file, so this returns
// false there and the answer for that case is elsewhere. The export is written
// to a temporary file and renamed into place, which means the source is read in
// full before the destination is touched, rather than being truncated out from
// under the process still reading it.
func sameSource(in io.Reader, inPath, outPath string) (bool, error) {
	var (
		inputInfo os.FileInfo
		err       error
	)
	switch file, ok := in.(*os.File); {
	case inPath != "":
		inputInfo, err = os.Stat(inPath)
	case ok:
		inputInfo, err = file.Stat()
	default:
		return false, nil
	}
	if err != nil {
		return false, err
	}
	outputInfo, err := os.Stat(outPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return os.SameFile(inputInfo, outputInfo), nil
}

// exportTarget is where an export is written. A regular file, or a path that
// does not exist yet, is written through a temporary file beside it and renamed
// into place once the whole export succeeded. That is what keeps a failed or
// interrupted run from replacing a previous export with a stub, and it is the
// only protection left when the source arrives down a pipe, since the same-file
// check cannot see which file is behind stdin.
//
// Anything else, a device, a fifo, or the /dev/fd descriptor a shell builds for
// `-o >(cmd)` and for `-o /dev/stdout` in a pipeline, is written straight
// through. Those cannot be renamed over, and they cannot be truncated either,
// which is what made an explicit Truncate refuse them with EINVAL where the
// O_TRUNC it replaced had simply been ignored.
type exportTarget struct {
	file *os.File
	temp string
	dest string
}

func openExportTarget(path string) (*exportTarget, error) {
	if info, err := os.Stat(path); err == nil && !info.Mode().IsRegular() {
		file, err := os.OpenFile(path, os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		return &exportTarget{file: file}, nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// Created in the destination's own directory so the rename stays on one
	// filesystem, and named with a leading dot so a half-written export does not
	// look like a finished one to whatever is watching the directory.
	file, err := os.CreateTemp(dir, ".mcpsnoop-export-*")
	if err != nil {
		if !errors.Is(err, fs.ErrPermission) {
			return nil, err
		}
		// A writable file inside a directory that is not writable, which is what a
		// CI artifact directory with a pre-created placeholder looks like. That
		// worked before the atomic write and refusing it now would be a regression,
		// so fall back to writing the destination directly. The cost is that an
		// interrupted run leaves a partial file, which the caller says out loud.
		direct, openErr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if openErr != nil {
			return nil, err
		}
		fmt.Fprintln(os.Stderr, "mcpsnoop export: writing "+path+" in place, "+
			"the directory does not allow a temporary file, so an interrupted run may leave it partial")
		return &exportTarget{file: direct}, nil
	}
	return &exportTarget{file: file, temp: file.Name(), dest: path}, nil
}

func (t *exportTarget) Write(p []byte) (int, error) { return t.file.Write(p) }

func (t *exportTarget) commit() error {
	if err := t.file.Close(); err != nil {
		return err
	}
	if t.temp == "" {
		return nil
	}
	if err := os.Rename(t.temp, t.dest); err != nil {
		return err
	}
	t.temp = ""
	return nil
}

// abort is deferred unconditionally and does nothing once commit has run, so
// every path out of the command cleans up after itself.
func (t *exportTarget) abort() {
	_ = t.file.Close()
	if t.temp != "" {
		_ = os.Remove(t.temp)
	}
}

// runShim runs the transparent stdio proxy. It writes the durable session log
// AND streams live to the hub. Neither has to be running first.
func runShim(command []string, label, traceFile string, noTrace bool, redaction proxy.RedactConfig, trace traceOptions) int {
	if label == "" {
		label = labelFor(command)
	}
	sessionID := newSessionID(label)

	sink, file := traceSink(sessionID, traceFile, noTrace, redaction, trace)
	defer func() {
		_ = sink.Close()
		// Only the file sink losing an envelope leaves a hole in the saved trace,
		// so only that is reported. A live socket with no hub listening fills its
		// buffer on every ordinary run, and saying the trace is incomplete then
		// spent the one signal that means the capture cannot be trusted.
		if file != nil {
			if n := file.Dropped(); n > 0 {
				fmt.Fprintf(os.Stderr, "mcpsnoop: dropped %d envelope(s) under load, the saved trace is incomplete\n", n)
			}
		}
	}()
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
// traceSink builds the fan-out of sinks a proxy run writes to. The second return
// is the durable file sink alone, because only that one losing an envelope makes
// the capture incomplete. The live socket fills its buffer whenever no hub is
// running, which is the ordinary case, and an OTLP endpoint that is down is a
// delivery problem rather than a hole in the trace, so totalling every child made
// the one signal meaning "this capture cannot be trusted" fire on perfect ones.
func traceSink(sessionID, traceFile string, noTrace bool, redaction proxy.RedactConfig, trace traceOptions) (proxy.Sink, proxy.DropCounter) {
	if noTrace {
		return proxy.NopSink(), nil
	}
	if traceFile == "" {
		traceFile = paths.SessionLogPath(sessionID)
	}
	var (
		sinks []proxy.Sink
		file  proxy.DropCounter
	)
	if f, err := os.OpenFile(traceFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "mcpsnoop: cannot open trace file %q: %v (continuing without file trace)\n", traceFile, err)
	} else {
		async := proxy.NewAsyncSink(f, 0)
		file = async
		sinks = append(sinks, async)
	}
	// The live stream is best effort, so a too-long socket path degrades to a
	// file-only trace with an explanation rather than aborting the whole proxy.
	socketPath := paths.SocketPath()
	if err := paths.CheckSocketPath(socketPath); err != nil {
		fmt.Fprintf(os.Stderr, "mcpsnoop: live view disabled, %v\n", err)
	} else {
		sinks = append(sinks, proxy.NewSocketSink(socketPath, 0))
	}
	if trace.OTLPEndpoint != "" {
		sinks = append(sinks, otlpsink.New(otlpsink.Config{
			Endpoint: trace.OTLPEndpoint,
			Headers:  trace.OTLPHeaders,
		}))
	}
	sink := proxy.Sink(proxy.NewMultiSink(sinks...))
	if redaction.Enabled() {
		sink = proxy.NewRedactingSink(sink, redaction)
	}
	return sink, file
}

// newHTTPCmd runs the transparent HTTP proxy for a streamable-HTTP MCP server.
func newHTTPCmd() *cobra.Command {
	var (
		target, listen, label  string
		otlpEndpoint           string
		noTrace, redactSecrets bool
		redactKeys             redactKeysFlag
		redactValues           redactValuesFlag
		redactPaths            redactPathsFlag
		otlpHeaders            otlpHeadersFlag
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
			applyConfig(cmd.Flags(), cfg, ok, &label, nil, &noTrace, &redactSecrets, &redactKeys, &redactValues, &redactPaths)
			if err := paths.CheckLabel(label); err != nil {
				fmt.Fprintln(os.Stderr, "mcpsnoop http:", err)
				return exitCode(2)
			}
			if target == "" {
				fmt.Fprintln(os.Stderr, "mcpsnoop http: --target is required")
				return exitCode(2)
			}
			trace, err := parseTraceOptions(otlpEndpoint, otlpHeaders)
			if err != nil {
				return err
			}
			lbl := label
			if lbl == "" {
				if u, err := url.Parse(target); err == nil && u.Host != "" {
					lbl = u.Host
				} else {
					lbl = "http"
				}
			}
			sessionID := newSessionID(lbl)

			sink, file := traceSink(sessionID, "", noTrace, redactConfig(redactSecrets, redactKeys, redactValues, redactPaths), trace)
			defer func() {
				_ = sink.Close()
				if file != nil {
					if n := file.Dropped(); n > 0 {
						fmt.Fprintf(os.Stderr, "mcpsnoop: dropped %d envelope(s) under load, the saved trace is incomplete\n", n)
					}
				}
			}()

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			fmt.Fprintf(os.Stderr, "mcpsnoop: proxying %s → %s (session %s)\n", listen, target, sessionID)
			if err := runHTTPFn(ctx, proxy.HTTPConfig{
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
	f.StringVar(&otlpEndpoint, "otlp-endpoint", "", "stream completed calls to an OTLP/HTTP JSON traces endpoint")
	f.Var(&otlpHeaders, "otlp-header", "HTTP header for OTLP delivery as Name=Value, repeatable")
	f.BoolVar(&noTrace, "no-trace", false, "disable tracing, pure passthrough")
	f.BoolVar(&redactSecrets, "redact-secrets", false, "scrub common secret JSON keys in trace payloads")
	f.Var(&redactKeys, "redact-key", "JSON key name to scrub in saved trace payloads, repeat or comma-separated")
	f.Var(&redactValues, "redact-value", "regular expression to scrub inside observed string values, stderr, and non-JSON text, repeatable")
	f.Var(&redactPaths, "redact-path", "JSONPath selecting values to scrub in saved trace payloads, repeatable")
	return cmd
}

// runHub runs the live TUI, collecting traffic from all shims and past sessions.
func runHub(historyLimit int, metricsListen string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := tui.RunWithHistoryLimitAndMetrics(ctx, paths.SocketPath(), paths.SessionsDir(), historyLimit, metricsListen); err != nil {
		fmt.Fprintf(os.Stderr, "mcpsnoop: %v\n", err)
		return 1
	}
	return 0
}

// newOpenCmd opens a persisted JSONL session directly in the TUI.
func newOpenCmd() *cobra.Command {
	var (
		redactSecrets bool
		redactKeys    redactKeysFlag
		redactValues  redactValuesFlag
		redactPaths   redactPathsFlag
	)
	var replayTarget string
	var replayHeaders []string
	cmd := &cobra.Command{
		Use:   "open [session-id|session.jsonl|-]",
		Short: "Open a captured session in the TUI, or - to read from stdin",
		Long:  "Open a captured session in the TUI. With no session, the newest session log is opened. Use - to read from stdin.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var arg string
			if len(args) == 1 {
				arg = args[0]
			}
			target := replay.HTTPTarget{URL: strings.TrimSpace(replayTarget), Headers: replayHeaders}
			if target.URL == "" && len(replayHeaders) > 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop open: --replay-header needs --replay-target, since there is nowhere to send them")
				return codeOf(2)
			}
			// Checked here rather than dropped at send time. A header that never
			// reached the request produced a 401 telling the operator to pass the
			// credential they had already passed, which is the worst way to be told
			// about a typo.
			for _, h := range replayHeaders {
				name, _, ok := strings.Cut(h, ":")
				if !ok || strings.TrimSpace(name) == "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "mcpsnoop open: --replay-header %q is not a header, want 'Name: value'\n", h)
					return codeOf(2)
				}
			}
			return codeOf(runOpen(arg, redactConfig(redactSecrets, redactKeys, redactValues, redactPaths), target))
		},
	}
	cmd.Flags().SortFlags = false
	cmd.Flags().BoolVar(&redactSecrets, "redact-secrets", false, "scrub common secret JSON keys in captured JSON-RPC payloads")
	cmd.Flags().Var(&redactKeys, "redact-key", "JSON key name to scrub in captured JSON-RPC payloads, repeat or comma-separated")
	cmd.Flags().Var(&redactValues, "redact-value", "regular expression to scrub inside captured JSON-RPC string values, stderr, and non-JSON text, repeatable")
	cmd.Flags().Var(&redactPaths, "redact-path", "JSONPath selecting values in captured JSON-RPC payloads to scrub, repeatable")
	cmd.Flags().StringVar(&replayTarget, "replay-target", "", "MCP endpoint a replay of an HTTP-captured session posts to; a capture records the endpoint stripped of anything credential-shaped, so it is not an address to dial and this is where you say where a replay goes")
	cmd.Flags().StringArrayVar(&replayHeaders, "replay-header", nil, "extra header for a replayed request as 'Name: value', repeatable; this is how a credential reaches the server, since mcpsnoop never records one")
	return cmd
}

// runOpen loads a session (id, path, or - for stdin) and shows it in the TUI.
func runOpen(arg string, redaction proxy.RedactConfig, replayTarget replay.HTTPTarget) int {
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

	st, err := loadOpenStore(r, redaction)
	if err != nil {
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
		if err := tui.RunOpenWithInput(ctx, st, tty, tui.WithHTTPReplay(replayTarget)); err != nil {
			fmt.Fprintln(os.Stderr, "mcpsnoop open:", err)
			return 1
		}
	} else {
		if err := runOpenTUIFn(ctx, st, tui.WithHTTPReplay(replayTarget)); err != nil {
			fmt.Fprintln(os.Stderr, "mcpsnoop open:", err)
			return 1
		}
	}

	return 0
}

func loadOpenStore(r io.Reader, redaction proxy.RedactConfig) (*store.Store, error) {
	st := store.New()
	redactor := proxy.NewRedactor(redaction)
	if err := proxy.Decode(r, func(e proxy.Envelope) {
		st.Ingest(redactor.RedactEnvelope(e))
	}); err != nil {
		return nil, err
	}
	return st, nil
}

func resolveOpenSessionPath(arg string) (string, bool, error) {
	if arg == "-" {
		return "", true, nil
	}
	path, err := exporter.ResolveSessionPath(arg)
	return path, false, err
}
