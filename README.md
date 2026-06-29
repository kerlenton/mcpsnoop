<p align="center">
  <img src="assets/png/mcpsnoop-lockup.png" alt="mcpsnoop" width="440">
</p>

**Wireshark for MCP.** A transparent proxy that shows every real tool call
between your AI client and your MCP servers, live in your terminal.

[![CI](https://github.com/kerlenton/mcpsnoop/actions/workflows/ci.yml/badge.svg)](https://github.com/kerlenton/mcpsnoop/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/kerlenton/mcpsnoop.svg)](https://pkg.go.dev/github.com/kerlenton/mcpsnoop)
[![MIT](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

![demo](docs/demo.gif)

## The problem

The official [MCP Inspector](https://github.com/modelcontextprotocol/inspector)
connects as its own client. It never sees the traffic between *your* client
(Claude Desktop, Cursor, Claude Code) and your server. A breakpoint in your own
server only fires once a request arrives. It can't show you the call the real
client never made, or made with arguments you didn't expect. So when a tool
silently isn't called, capabilities don't line up, or a call just hangs, you're
back to `tail`-ing a log in `/tmp` and guessing.

mcpsnoop sits in the real data path instead. Wrap your server command with it
and watch every JSON-RPC frame in a live terminal UI as your real client and
server talk.

## Quick start

Want to see it first, with nothing to set up? Run `mcpsnoop demo` for a scripted
session that plays into the live UI.

To use it for real, wrap your server in your client's MCP config:

```jsonc
{ "mcpServers": {
    "my-server": { "command": "mcpsnoop", "args": ["--", "node", "build/index.js"] }
}}
```

Everything after `--` is the command that normally launches your server (here, a
TypeScript build run with `node`). Swap in whatever you already use, like
`python server.py`, `npx -y @scope/server`, or a compiled binary.

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

- **Live JSON-RPC stream.** Requests, responses, notifications and server stderr,
  colour-coded, with errors and slow calls flagged, including tool-level
  `result.isError`, not just JSON-RPC errors.
- **Replay.** Re-run any captured tool call against a fresh, isolated copy of the
  server. The fastest loop for iterating on a tool.
- **Capability inspector** (`c`). See exactly what the client and server agreed on
  at the handshake.
- **Frame inspector** (`enter`). Full, pretty-printed JSON with in-frame search.
- **Hung-call detection.** In-flight requests show `PENDING` with a live timer, so
  a stuck tool is obvious at a glance.
- **A real filter query.** Narrow the stream with `tool:`, `status:`, `dir:`,
  `kind:`, `id:` or plain text.

## How it compares

| | MCP Inspector | mcp-trace | mcpsnoop |
|---|:---:|:---:|:---:|
| Sees your real client↔server traffic | no | yes | yes |
| Interactive terminal UI | no | yes | yes |
| Zero-config, no flags or ordering | no | no | yes |
| Capability inspector | partial | no | yes |
| Replay a captured call | no | no | yes |
| Single binary, no runtime deps | no | varies | yes |

## Install

```bash
go install github.com/kerlenton/mcpsnoop/cmd/mcpsnoop@latest
```

Or with [Homebrew](https://brew.sh):

```bash
brew tap kerlenton/mcpsnoop
brew install mcpsnoop
```

Or grab a prebuilt binary for your platform from the
[Releases](https://github.com/kerlenton/mcpsnoop/releases) page.

## How it works

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/architecture-dark.svg">
    <img alt="mcpsnoop sits in the pipe between your AI client and your MCP servers, copying every JSON-RPC frame to a live terminal UI" src="assets/architecture-light.svg" width="760">
  </picture>
</p>

The official Inspector connects as a *second* client, off to the side. mcpsnoop
sits in the actual pipe, so it sees exactly what your real client and server say
to each other, whatever the server is written in.

It's two roles in one binary: `mcpsnoop -- <server>` is the transparent shim your
client spawns (forwarding bytes verbatim while shipping a copy of each frame),
and `mcpsnoop` with no arguments is the hub and TUI. They pair through a
well-known socket and on-disk logs, so neither has to start first.

## Keybindings

`enter` drill in · `esc` back · `r` replay · `c` capabilities · `y` copy ·
`/` filter · `:` command · `p` pause · `f` follow · `ctrl-d` delete. Move with
`j`/`k`, page with `ctrl-f`/`ctrl-b`, `g`/`G` for top and bottom, `shift`+column
to sort. Press `?` in the app for the full list.

## Filtering the stream

In a session, press `/` and combine space-separated tokens (ANDed): plain text
matches the method, tool, id and payload, while `tool:` `method:` `id:` `kind:`
`dir:` `status:` filter by field. So `tool:search status:slow` shows slow calls
to a search tool, and `dir:s2c kind:req` surfaces server-initiated requests
(sampling, roots). The `?` help lists each token and the values it accepts.

## Security

mcpsnoop runs the server command you wrap, so only wrap servers you trust and
run untrusted ones in a container. It never executes anything you didn't put in
your client config.

## Contributing

Issues and pull requests are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for
the dev setup and the `make check` gate. mcpsnoop is pre-1.0 and follows
[SemVer](https://semver.org): while on `0.x`, minor releases may change
user-facing behaviour, and patch releases are bug fixes.

## License

[MIT](LICENSE)
