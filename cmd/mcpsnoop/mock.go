package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kerlenton/mcpsnoop/internal/exporter"
	"github.com/kerlenton/mcpsnoop/internal/mock"
)

func newMockCmd() *cobra.Command {
	var allowRedacted bool
	cmd := &cobra.Command{
		Use:   "mock <session-id|log.jsonl>",
		Short: "Serve a captured session back as a stdio MCP server",
		Long: `Serve a captured session back as a stdio MCP server.

The mock reads newline-delimited JSON-RPC from stdin and writes
newline-delimited JSON-RPC to stdout, answering every request from the
capture. Matching is strict: method plus structurally normalized params, with
only the volatile _meta keys (progressToken, client identity and
capabilities, logging level, traceparent, tracestate, baggage) ignored. A
request the capture never recorded gets a JSON-RPC error naming the method,
never an unrelated recorded answer. Notifications draw no reply. The response
carries the incoming request id, never the recorded one.

What was observed is what is served: an input_required result and its
continuation replay as the recorded hops, task calls replay as recorded
exchanges, and a server/discover the capture never recorded is answered with
an error rather than a synthesised result. Responses only, though. A recorded
server notification, progress and logging among them, is not replayed, so a
mocked call completes without the progress the original reported.

Stdio captures only: a session explicitly captured on another transport
(HTTP) is refused, since ConnID and transport semantics are out of scope for
this issue. A legacy log whose frames name no transport is accepted.

There is no - stdin form. The mock serves MCP requests on stdin, so a capture
piped on stdin would consume the very stream the server role needs; passing -
is rejected rather than silently serving nothing.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMock(cmd, args[0], allowRedacted)
		},
	}
	cmd.Flags().SortFlags = false
	cmd.Flags().BoolVar(&allowRedacted, "allow-redacted", false, "serve a capture containing redacted frames; the responses still carry the redacted placeholders")
	return cmd
}

func runMock(cmd *cobra.Command, arg string, allowRedacted bool) error {
	if arg == "-" {
		fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop mock: reading the capture from stdin cannot also serve MCP requests on stdin; pass a session id or log.jsonl path instead")
		return exitCode(2)
	}
	path, err := exporter.ResolveSessionPath(arg)
	if err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop mock:", err)
		return exitCode(1)
	}
	srv, err := mock.LoadFile(path)
	if err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "mcpsnoop mock:", err)
		return exitCode(1)
	}
	return codeOf(srv.Serve(cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(), mock.Policy{AllowRedacted: allowRedacted}))
}
