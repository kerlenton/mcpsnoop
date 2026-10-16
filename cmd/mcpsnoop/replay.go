package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kerlenton/mcpsnoop/internal/exporter"
	"github.com/kerlenton/mcpsnoop/internal/paths"
	"github.com/kerlenton/mcpsnoop/internal/proxy"
	"github.com/kerlenton/mcpsnoop/internal/replay"
	"github.com/kerlenton/mcpsnoop/internal/sessiondiff"
	"github.com/kerlenton/mcpsnoop/internal/wiretext"
)

type replayOptions struct {
	session, out, label string
	wait, timeout       time.Duration
	skipTools           []string
	readOnly            bool
	allowRedacted       bool
	exitOnRegression    bool
	durationThreshold   time.Duration
	durationRatio       float64
	redactSecrets       bool
	redactKeys          redactKeysFlag
	redactValues        redactValuesFlag
	redactPaths         redactPathsFlag
}

func newReplayCmd() *cobra.Command {
	var opts replayOptions
	cmd := &cobra.Command{
		Use:   "replay <session-id|log.jsonl> [flags] -- <server> [args...]",
		Short: "Drive a server with the client side of a recorded session",
		Long: `Drive a server with the client side of a recorded session.

The recorded client's frames go to the server in the order the client sent
them, each one after the answers and server requests it followed in the
recording, so what the client did at once still happens at once and what it
did in turn still waits its turn. The minutes a model spent thinking between
calls are not replayed. Values the server handed the client and the client sent
back, a cursor, a requestState, a task id, the id of a record a tool created,
are taken from the server's new answers. The server's own requests are
answered with what the client answered then, its pings as they come, and a
cancellation goes out after the delay it did, unless the call was answered
first.

The replay is recorded through the same proxy as a live session, so the
capture it writes reads like any other, and check, diff and export work on it.
At the end it compares the replay with the recording the way diff does,
pairing calls by their request id, and with --exit-code exits 1 when the
server got worse: a tool removed or changed, a call that failed or went
unanswered, a call that got notably slower.

It runs every call the recording made, including ones that change things, so
point it at a server and data you can afford to change, or hold calls back
with --skip-tool or --read-only.

Exit codes: 0 when the replay ran, 1 when it ran and --exit-code found a
regression, 2 when it could not run at all.`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runReplay(cmd, args, &opts)
		},
	}
	f := cmd.Flags()
	f.SortFlags = false
	f.StringVar(&opts.session, "session", "", "session id within the capture to replay, defaults to the first")
	f.StringVar(&opts.out, "out", "", "where to write the replayed capture, defaults to the session log directory")
	f.StringVar(&opts.label, "label", "", "server label of the replayed session, defaults to the recorded one")
	f.DurationVar(&opts.wait, "wait", replay.DefaultWait, "how long to wait for any one answer the recording got")
	f.DurationVar(&opts.timeout, "timeout", 5*time.Minute, "limit for the whole replay")
	f.StringArrayVar(&opts.skipTools, "skip-tool", nil, "do not call this tool, repeatable")
	f.BoolVar(&opts.readOnly, "read-only", false, "call only the tools the recording's own tools/list marked readOnlyHint true")
	f.BoolVar(&opts.allowRedacted, "allow-redacted", false, "replay client frames that were redacted when recorded, sending the placeholders")
	f.BoolVar(&opts.exitOnRegression, "exit-code", false, "exit 1 when the replay regressed against the recording")
	f.DurationVar(&opts.durationThreshold, "duration-threshold", sessiondiff.DefaultDurationThreshold, "minimum absolute duration change to report")
	f.Float64Var(&opts.durationRatio, "duration-ratio", sessiondiff.DefaultDurationRatio, "minimum slowdown or speedup ratio to report")
	f.BoolVar(&opts.redactSecrets, "redact-secrets", false, "scrub common secret JSON keys in the replayed capture")
	f.Var(&opts.redactKeys, "redact-key", "JSON key name to scrub in the replayed capture, repeat or comma-separated")
	f.Var(&opts.redactValues, "redact-value", "regular expression to scrub inside observed string values, stderr, and non-JSON text, repeatable")
	f.Var(&opts.redactPaths, "redact-path", "JSONPath selecting values to scrub in the replayed capture, repeatable")
	return cmd
}

func runReplay(cmd *cobra.Command, args []string, opts *replayOptions) error {
	stderr := cmd.ErrOrStderr()
	fail := func(format string, a ...any) error {
		fmt.Fprintf(stderr, "mcpsnoop replay: "+format+"\n", a...)
		return exitCode(2)
	}
	dash := cmd.ArgsLenAtDash()
	if dash != 1 || len(args) < 2 {
		return fail("give one capture, then -- and the server command, as in mcpsnoop replay session.jsonl -- node build/index.js")
	}
	if opts.wait <= 0 || opts.timeout <= 0 {
		return fail("--wait and --timeout must be positive")
	}
	if opts.durationThreshold < 0 || opts.durationRatio < 1 || math.IsNaN(opts.durationRatio) || math.IsInf(opts.durationRatio, 0) {
		return fail("--duration-threshold must not be negative, and --duration-ratio must be finite and at least 1")
	}
	command := args[dash:]

	recordedPath, err := exporter.ResolveSessionPath(args[0])
	if err != nil {
		return fail("%v", err)
	}
	rec, err := loadRecording(recordedPath, opts.session)
	if err != nil {
		return fail("%s: %v", recordedPath, err)
	}
	if rec.ClientFrames() == 0 {
		return fail("%s: the session holds no client frames to replay", recordedPath)
	}
	if redacted := rec.RedactedClientFrames(); len(redacted) > 0 && !opts.allowRedacted {
		return fail("%s: %d client frame(s) were redacted when recorded, so the server would receive the placeholders, frames %s. Pass --allow-redacted to replay them anyway",
			recordedPath, len(redacted), joinSeqs(redacted))
	}

	label := opts.label
	if label == "" {
		label = rec.Label
	}
	if err := paths.CheckLabel(label); err != nil {
		return fail("%v", err)
	}
	sessionID := newSessionID(label)
	out := opts.out
	if out == "" {
		out = paths.SessionLogPath(sessionID)
	}
	if same, _ := sameFile(out, recordedPath); same {
		return fail("--out names the recording itself, which the replay would overwrite")
	}
	file, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fail("%v", err)
	}
	fileSink := proxy.NewAsyncSink(file, 0)
	sinks := []proxy.Sink{fileSink}
	// The live view is a convenience, so a socket path that will not fit costs
	// it and nothing else, as it does for the shim.
	if socket := paths.SocketPath(); paths.CheckSocketPath(socket) == nil {
		sinks = append(sinks, proxy.NewSocketSink(socket, 0))
	}
	sink := proxy.Sink(proxy.NewMultiSink(sinks...))
	if redaction := redactConfig(opts.redactSecrets, opts.redactKeys, opts.redactValues, opts.redactPaths); redaction.Enabled() {
		sink = proxy.NewRedactingSink(sink, redaction)
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), opts.timeout)
	defer cancel()
	report, replayErr := replay.ReplaySession(ctx, rec, replay.SessionOptions{
		Command:   command,
		Label:     label,
		SessionID: sessionID,
		Sink:      sink,
		Wait:      opts.wait,
		SkipTools: opts.skipTools,
		ReadOnly:  opts.readOnly,
	})
	_ = sink.Close()
	if replayErr != nil {
		return fail("%v", replayErr)
	}
	if dropped := fileSink.Dropped(); dropped > 0 {
		fmt.Fprintf(stderr, "mcpsnoop replay: dropped %d envelope(s), the replayed capture is incomplete\n", dropped)
	}

	w := cmd.OutOrStdout()
	writeReplayReport(w, rec, report, command, out)

	before, err := loadDiffSessionAt(recordedPath, rec.SessionID)
	if err != nil {
		return fail("%s: %v", recordedPath, err)
	}
	after, err := loadDiffSessionAt(out, sessionID)
	if err != nil {
		return fail("%s: %v", out, err)
	}
	diff := sessiondiff.Compare(before, after, sessiondiff.Options{
		DurationThreshold: opts.durationThreshold,
		DurationRatio:     opts.durationRatio,
		PairByRequestID:   true,
	})
	fmt.Fprintln(w)
	if err := sessiondiff.WriteText(w, diff); err != nil {
		return fail("%v", err)
	}
	if opts.exitOnRegression && diff.HasRegression() {
		return exitCode(1)
	}
	return nil
}

func loadRecording(path, session string) (*replay.Recording, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return replay.LoadRecording(f, session)
}

func loadDiffSessionAt(path, sessionID string) (exporter.SessionExport, error) {
	st, _, err := exporter.LoadFile(path)
	if err != nil {
		return exporter.SessionExport{}, err
	}
	return exporter.Build(st, sessionID)
}

func writeReplayReport(w io.Writer, rec *replay.Recording, report replay.SessionReport, command []string, out string) {
	fmt.Fprintf(w, "replayed %d of %d client frames from session %s against %s\n",
		report.Sent, rec.ClientFrames(), wiretext.OneLine(rec.SessionID), wiretext.OneLine(strings.Join(command, " ")))
	fmt.Fprintf(w, "capture: %s\n", out)
	if rec.Transport == proxy.TransportHTTP {
		fmt.Fprintln(w, "the recording was captured over HTTP, and its frames were replayed over stdio")
	}
	section := func(title string, lines []string) {
		if len(lines) == 0 {
			return
		}
		fmt.Fprintln(w, title+":")
		for _, line := range lines {
			fmt.Fprintf(w, "  %s\n", wiretext.OneLine(line))
		}
	}
	section("not sent", report.Skipped)
	section("waited for in vain", report.Missing)
	section("server requests nobody answered", report.Unanswered)
	if report.ServerExited {
		fmt.Fprintf(w, "the server exited with code %d before the replay finished\n", report.ExitCode)
	}
}

func joinSeqs(seqs []uint64) string {
	parts := make([]string, 0, len(seqs))
	for _, seq := range seqs {
		parts = append(parts, fmt.Sprint(seq))
	}
	return strings.Join(parts, ", ")
}

func sameFile(a, b string) (bool, error) {
	ai, err := os.Stat(a)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(ai, bi), nil
}
