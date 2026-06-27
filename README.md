# mcpsnoop

**Wireshark for MCP.** A transparent proxy that shows every real tool call
between your AI client and your MCP servers — live, in a k9s-style terminal UI.

[![CI](https://github.com/kerlenton/mcpsnoop/actions/workflows/ci.yml/badge.svg)](https://github.com/kerlenton/mcpsnoop/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/kerlenton/mcpsnoop.svg)](https://pkg.go.dev/github.com/kerlenton/mcpsnoop)
![License: MIT](https://img.shields.io/badge/license-MIT-blue)
![Single binary](https://img.shields.io/badge/deps-zero-brightgreen)

![demo](docs/demo.gif)

---

## Why

The official [MCP Inspector](https://github.com/modelcontextprotocol/inspector)
connects as its **own** client — so it can't see the traffic between *your*
client (Claude Desktop, Cursor, Claude Code) and your server. When something
breaks in real use — a tool isn't called, capabilities don't negotiate, a call
is mysteriously slow — you're left tailing a log file in `/tmp`.

**mcpsnoop sits in the real data path** and shows you everything, with full MCP
awareness: tool calls, arguments, responses, timings, errors, and the
capabilities both sides actually negotiated. Then it lets you **replay** any
captured call against a fresh copy of the server — no need to re-drive your
whole client.

## Quick start

**1. Wrap your server** in your client's MCP config:

```jsonc
{ "mcpServers": {
    "my-server": { "command": "mcpsnoop", "args": ["--", "node", "build/index.js"] }
}}
```

**2. Use your client normally.**

**3. Open the live UI:**

```bash
mcpsnoop
```

That's it. No flags, no socket wiring, no ordering — the shim and the UI find
each other automatically, and the UI backfills past sessions from disk, so you
can open it **before or after** your client starts.

> **Streamable HTTP server?** Run mcpsnoop as a reverse proxy and point your
> client at it instead of the real endpoint:
> ```bash
> mcpsnoop http --target http://localhost:3000/mcp --listen :7000
> ```

> **No server of your own yet?** [docs/DEMO.md](docs/DEMO.md) shows how to point
> Claude at a published test server (`@modelcontextprotocol/server-everything`)
> through mcpsnoop and watch real traffic flow.

## Features

- 🔴 **Live JSON-RPC stream** — requests, responses, notifications, server stderr, colour-coded, with error & slow-call highlighting
- 🧭 **k9s-style TUI** — drill into a session, `/` filter, `:` command jump, `shift+col` sort, `?` help
- 🔁 **Replay** any captured tool call against a fresh, isolated server copy — the killer feature for iterative dev
- 🤝 **Capability inspector** — exactly what client & server negotiated at the handshake (`c`)
- 🔍 **Inspect** any frame as pretty-printed, scrollable JSON with in-frame search (`enter`, `/`)
- ⏳ **Hung-call detection** — in-flight requests show `PENDING` with a live timer, so stuck tools are obvious
- 📋 **Copy any frame's JSON** to the clipboard (`y`) — paste straight into a bug report or test
- 🗂 **Multiple servers at once**, each its own session
- 🌐 **stdio _and_ streamable HTTP** (JSON + SSE)
- 📦 **Single binary, zero runtime deps** — `brew install` or `go install`

## mcpsnoop vs the alternatives

| | MCP Inspector | mcp-trace | **mcpsnoop** |
|---|:---:|:---:|:---:|
| Sees your **real** client↔server traffic | ❌ (own client) | ✅ | ✅ |
| Interactive TUI | ❌ (web) | ✅ | ✅ (k9s-style) |
| Zero-config (no flags / ordering) | — | ❌ | ✅ |
| Capability inspector | partial | ❌ | ✅ |
| **Replay** a captured call | ❌ | ❌ | ✅ |
| Single binary, zero deps | ❌ (Node) | varies | ✅ |

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

mcpsnoop is two roles in one binary:

- `mcpsnoop -- <server>` is a **dumb, transparent stdio shim** the client
  spawns. It forwards bytes verbatim (it can never corrupt the pipe) and ships a
  copy of every JSON-RPC frame to the hub best-effort, while also writing a
  durable per-session log to disk.
- `mcpsnoop` (no args) is the **hub + TUI**. It listens on a well-known socket,
  correlates requests with responses (deriving timings), parses tool calls and
  capabilities, and renders it all live.

Because the log is on disk and the socket is well-known, neither side has to
start first — open the UI whenever, and history is right there.

## Keys

`↑↓`/`jk` move · `enter` drill in · `esc` back · `/` filter/search · `:` command ·
`shift+<col>` sort · `r` replay · `c` capabilities · `y` copy JSON ·
`ctrl-d` delete session · `p` pause · `f` follow · `?` help · `:q` quit

## Security

mcpsnoop executes the server command you wrap. Only wrap servers you trust; run
untrusted servers in a container. It never auto-executes anything without your
explicit config.

## License

[MIT](LICENSE)
