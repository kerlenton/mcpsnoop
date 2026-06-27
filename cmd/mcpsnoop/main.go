// Command mcpsnoop is a transparent proxy debugger for MCP traffic.
//
// Two modes, one binary:
//
//	mcpsnoop -- <server command>   run as a transparent stdio shim (the client
//	                              spawns this; it proxies stdio to the real
//	                              server and traces every JSON-RPC frame).
//	mcpsnoop                       run the live TUI in your terminal: collect
//	                              traffic from all shims and past sessions.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/kerlenton/mcpsnoop/internal/paths"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/tui"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// `mcpsnoop http ...` is a separate subcommand with its own flags.
	if args := os.Args[1:]; len(args) > 0 && args[0] == "http" {
		os.Exit(runHTTP(args[1:]))
	}

	fs := flag.NewFlagSet("mcpsnoop", flag.ExitOnError)
	var (
		label     = fs.String("label", "", "server label shown in the TUI (default: command name)")
		traceFile = fs.String("trace-file", "", "override the JSONL trace path (default: well-known session log)")
		noTrace   = fs.Bool("no-trace", false, "disable tracing; pure passthrough")
		showVer   = fs.Bool("version", false, "print version and exit")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "mcpsnoop %s — Wireshark for MCP\n\n", version)
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  mcpsnoop [flags] -- <server command> [args...]   run as transparent stdio shim\n")
		fmt.Fprintf(os.Stderr, "  mcpsnoop http --target <url> [--listen :7000]     run as transparent HTTP proxy\n")
		fmt.Fprintf(os.Stderr, "  mcpsnoop                                          run the live TUI (collector)\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(os.Args[1:])

	if *showVer {
		fmt.Println("mcpsnoop", version)
		return
	}

	if command := fs.Args(); len(command) > 0 {
		os.Exit(runShim(command, *label, *traceFile, *noTrace))
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

// labelFor derives a friendly session name from the wrapped command: it skips
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

// runShim runs the transparent stdio proxy. It writes the durable session log
// AND streams live to the hub; neither has to be running first.
func runShim(command []string, label, traceFile string, noTrace bool) int {
	if label == "" {
		label = labelFor(command)
	}
	sessionID := fmt.Sprintf("%s-%d", label, os.Getpid())

	sink := traceSink(sessionID, traceFile, noTrace)
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

// traceSink builds the shared sink: a durable per-session JSONL log plus a
// best-effort live stream to the hub. Returns a no-op sink when disabled.
func traceSink(sessionID, traceFile string, noTrace bool) proxy.Sink {
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
	return proxy.NewMultiSink(sinks...)
}

// runHTTP runs the transparent HTTP proxy subcommand.
func runHTTP(args []string) int {
	fs := flag.NewFlagSet("mcpsnoop http", flag.ExitOnError)
	var (
		target  = fs.String("target", "", "real MCP server endpoint, e.g. http://localhost:3000/mcp (required)")
		listen  = fs.String("listen", ":7000", "address to listen on")
		label   = fs.String("label", "", "server label shown in the TUI (default: target host)")
		noTrace = fs.Bool("no-trace", false, "disable tracing; pure passthrough")
	)
	_ = fs.Parse(args)
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

	sink := traceSink(sessionID, "", *noTrace)
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
