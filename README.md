# mcpsnoop

**Wireshark for MCP.** A transparent proxy that shows every real tool call
between your AI client and your MCP servers, live in your terminal.

[![CI](https://github.com/kerlenton/mcpsnoop/actions/workflows/ci.yml/badge.svg)](https://github.com/kerlenton/mcpsnoop/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/kerlenton/mcpsnoop.svg)](https://pkg.go.dev/github.com/kerlenton/mcpsnoop)
[![MIT](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

![demo](docs/demo.gif)

## The problem

The official [MCP Inspector](https://github.com/modelcontextprotocol/inspector)
connects as its own client. It never sees the traffic between *your* client
(Claude Desktop, Cursor, Claude Code) and your server. So when a tool silently
isn't called, capabilities don't line up, or a call just hangs, you're back to
`tail`-ing a log in `/tmp` and guessing.

mcpsnoop sits in the real data path instead. Wrap your server command with it
and every JSON-RPC frame shows up in a live terminal UI: tool calls, arguments,
responses, timings, errors, and the capabilities both sides actually negotiated.
You can also replay any captured call against a fresh server, without driving
your whole client again.

## Quick start

Wrap your server in your client's MCP config:

```jsonc
{ "mcpServers": {
    "my-server": { "command": "mcpsnoop", "args": ["--", "node", "build/index.js"] }
}}
```

Use your client as usual, then open the UI:

```bash
mcpsnoop
```

No flags, no socket paths, no startup order to remember. The shim and the UI
find each other on their own, and the UI backfills past sessions from disk, so
it doesn't matter whether you open it before or after your client.

For a streamable-HTTP server, run mcpsnoop as a reverse proxy and point your
client at it:

```bash
mcpsnoop http --target http://localhost:3000/mcp --listen :7000
```

No server of your own to test against? [docs/DEMO.md](docs/DEMO.md) walks through
pointing Claude at a published test server through mcpsnoop.

## Features

- **Live JSON-RPC stream.** Requests, responses, notifications and server
  stderr, colour-coded, with errors and slow calls called out.
- **Replay.** Re-run any captured tool call against a fresh, isolated copy of
  the server. The fastest loop for iterating on a tool.
- **Capability inspector** (`c`). See exactly what the client and server agreed
  on at the handshake.
- **Frame inspector** (`enter`). Full, pretty-printed JSON with in-frame search.
- **Hung-call detection.** In-flight requests show `PENDING` with a live timer,
  so a stuck tool is obvious at a glance.
- **A real filter query.** Narrow the stream with `tool:`, `status:`, `dir:`,
  `kind:`, `id:` or plain text.
- **Tool-level errors too.** A response with `result.isError: true` is flagged,
  not just JSON-RPC errors.
- **Copy** (`y`) any frame's JSON straight to the clipboard.
- **Multiple servers** at once, each in its own session.
- **stdio and streamable HTTP** (JSON and SSE).
- **One static binary, no runtime dependencies.**

## How it compares

| | MCP Inspector | mcp-trace | mcpsnoop |
|---|:---:|:---:|:---:|
| Sees your real client↔server traffic | no (own client) | yes | yes |
| Interactive terminal UI | no (web) | yes | yes |
| Zero-config (no flags or ordering) | — | no | yes |
| Capability inspector | partial | no | yes |
| Replay a captured call | no | no | yes |
| Single binary, no deps | no (Node) | varies | yes |

## Install

```bash
brew install kerlenton/tap/mcpsnoop
# or
go install github.com/kerlenton/mcpsnoop/cmd/mcpsnoop@latest
```

From source:

```bash
git clone https://github.com/kerlenton/mcpsnoop && cd mcpsnoop
go build -o mcpsnoop ./cmd/mcpsnoop
```

## How it works

mcpsnoop is two roles in one binary.

`mcpsnoop -- <server>` is a transparent stdio shim that your client spawns
instead of the server. It forwards bytes verbatim (it can never corrupt the
pipe) while shipping a copy of each JSON-RPC frame to the hub, and it also writes
a per-session log to disk.

`mcpsnoop` with no arguments is the hub and TUI. It listens on a well-known
socket, pairs requests with responses to derive timings, parses tool calls and
capabilities, and renders everything live.

Because the log is on disk and the socket is well-known, neither side has to
start first.

## Keybindings

`j`/`k` move · `enter` drill in · `esc` back · `/` filter and search ·
`:` command · `shift`+column to sort · `r` replay · `c` capabilities ·
`y` copy · `ctrl-d` delete session · `p` pause · `?` help · `:q` quit

## Security

mcpsnoop runs the server command you wrap, so only wrap servers you trust and
run untrusted ones in a container. It never executes anything you didn't put in
your client config.

## License

[MIT](LICENSE)
