<p align="center">
  <img src="assets/png/mcpsnoop-lockup.png" alt="mcpsnoop" width="440">
</p>

**Wireshark for MCP.** A transparent proxy that shows every real tool call
between your AI client and your MCP servers, live in your terminal.

[![CI](https://github.com/kerlenton/mcpsnoop/actions/workflows/ci.yml/badge.svg)](https://github.com/kerlenton/mcpsnoop/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/kerlenton/mcpsnoop.svg)](https://pkg.go.dev/github.com/kerlenton/mcpsnoop)
[![MIT](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

<p align="center">
  <img src="docs/demo.gif" alt="mcpsnoop demo">
</p>

## The problem

The official [MCP Inspector](https://github.com/modelcontextprotocol/inspector)
connects as its own client, so it never sees what *your* client (Cursor, Claude
Code, Codex) actually sends your server. And anything that waits for a request
to arrive can't show the call the model never made, or made with the wrong
arguments. When a tool silently isn't called, capabilities don't line up, or a
call just hangs, you're left digging through logs and guessing.

**mcpsnoop sits in the real data path instead.** Wrap your server command with
it and watch every JSON-RPC frame live, as your real client and server talk.

## Quick start

See it right away, with nothing to set up.

```bash
mcpsnoop demo
```

To use it for real, wrap your server in your client's MCP config.

```json
{
  "mcpServers": {
    "my-server": {
      "command": "mcpsnoop",
      "args": ["--", "node", "build/index.js"]
    }
  }
}
```

Everything after `--` is the command that normally launches your server. Swap in
whatever you already use, like `python server.py`, `npx -y @scope/server`, or a
compiled binary. Then use your client as usual and open the UI.

```bash
mcpsnoop
```

No flags, no socket paths, no startup order to remember. The shim and the UI find
each other on their own, and the UI backfills past sessions from disk.

For a streamable-HTTP server, run mcpsnoop as a reverse proxy.

```bash
mcpsnoop http --target http://localhost:3000/mcp --listen :7000
```

No server of your own? [Try it for real](docs/TRY_IT.md) against a published
test server, driven by your own client. To inspect a session after it happened,
see [review past sessions from logs](docs/POST_MORTEM.md).

### Config file

If you reuse the same shim flags across a project, put them in a
`.mcpsnoop.toml` file in the current working directory.

```toml
label = "filesystem"
trace-file = "trace.jsonl"
redact-secrets = true
redact-key = "token,authorization"
redact-path = "$.params.arguments.password"
no-trace = false
```

Those are all the keys it supports.

The file is only looked up in the current working directory, not in parent
directories.

Explicit command-line flags override values from the config file.

## Commands

| Command | What it does |
|---|---|
| `mcpsnoop -- <server>` | wrap a stdio server as a transparent shim |
| `mcpsnoop` | open the live TUI |
| `mcpsnoop http --target <url>` | proxy a streamable-HTTP server |
| `mcpsnoop export` | render a session to json, html, text, or otlp |
| `mcpsnoop check` | fail CI on errors, invalid frames, warnings, slow, or hung calls |
| `mcpsnoop diff` | compare tools and calls across two captured sessions |
| `mcpsnoop open` | open a saved session in the TUI |
| `mcpsnoop remote <user@host>` | print the SSH tunnel command |
| `mcpsnoop demo` | play a scripted session |

Run `mcpsnoop help` for the full list, or `mcpsnoop help <command>` for the flags of one.

## How it compares

| | MCP Inspector | mcpsnoop |
|---|:---:|:---:|
| Sees your real client and server traffic | no | yes |
| Flags slow and hung calls | no | yes |
| Flags stray output that corrupts the stream | no | yes |
| Flags malformed JSON-RPC frames | no | yes |
| Interactive terminal UI | no | yes |
| Zero-config, no flags or ordering | no | yes |
| Capability inspector | partial | yes |
| Replay a captured call | no | yes |
| Session export (json / html / text / otlp) | no | yes |
| Single binary, no runtime deps | no | yes |

## Install

### Go

```bash
go install github.com/kerlenton/mcpsnoop/cmd/mcpsnoop@latest
```

### Homebrew

```bash
brew install kerlenton/mcpsnoop/mcpsnoop
```

Prebuilt binaries for every platform are on the [Releases](https://github.com/kerlenton/mcpsnoop/releases) page.

### Shell completions

mcpsnoop ships completions for bash, zsh, fish, and PowerShell. Run
`mcpsnoop completion <shell> --help` for the setup steps, which cover enabling
completion and the install path for your OS.

## How it works

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/architecture-dark.svg">
    <img alt="mcpsnoop sits in the pipe between your AI client and your MCP servers, copying every JSON-RPC frame to a live terminal UI" src="assets/architecture-light.svg" width="760">
  </picture>
</p>

mcpsnoop is two roles in one binary. `mcpsnoop -- <server>` is the transparent
shim your client spawns, forwarding bytes verbatim while shipping a copy of every
frame to the hub. `mcpsnoop` with no arguments is that hub and its live TUI. They
pair through a well-known socket and on-disk logs, so neither has to start first.

Because it sits in the actual pipe, not off to the side like the Inspector, it
sees exactly what your real client and server say to each other, whatever the
server is written in.

## Keybindings

| Key | Action | | Key | Action |
|---|---|---|---|---|
| `enter` | inspect / drill in | | `/` | filter |
| `esc` | back | | `:` | command |
| `j` / `k` | move | | `r` | replay a call |
| `g` / `G` | top / bottom | | `c` | capabilities |
| `ctrl-f` / `ctrl-b` | page | | `s` | tool summary |
| `p` | pause | | `y` | copy |
| `shift`+`<key>` | sort by column | | `e` | export |
| `ctrl-d` | delete session | | `f` | follow |
| `?` | help | | | |

Press `?` in the app for the full list.

## Filtering the stream

Press `/` in a session and combine space-separated tokens, ANDed. Plain text
matches the method, tool, id, and payload.

| Token | Filters by | Example |
|---|---|---|
| `tool:` | tool name | `tool:search` |
| `method:` | JSON-RPC method | `method:tools/call` |
| `id:` | request id | `id:7` |
| `dir:` | direction (`c2s`, `s2c`) | `dir:s2c` |
| `kind:` | frame type (`req`, `resp`, `notify`, `stderr`, `invalid`) | `kind:invalid` |
| `status:` | call outcome (`ok`, `error`, `slow`, `pending`, `bad`, `warn`) | `status:slow` |

Stack tokens to get specific.

```text
tool:search status:slow           # slow calls to one search tool
method:tools/call status:error    # tool calls that failed
dir:s2c kind:req                  # server-initiated requests (sampling, roots)
```

## Exporting sessions

Turn any captured session into a portable file.

```bash
mcpsnoop export -T json|html|text|otlp [-o file|-] [session-id|log.jsonl|-]
```

| Format | What you get |
|---|---|
| `json` | correlated calls, per-tool counts and p50/p95/p99 latency, slowest calls, capabilities, and raw frames |
| `html` | a self-contained browser file with search and collapsible JSON |
| `text` | a pretty plain-text dump |
| `otlp` | OTLP JSON with a trace per session and a span per correlated call |

```bash
mcpsnoop export -T html -o out.html       # an HTML file to open in a browser
mcpsnoop export -T text server.py-48213   # a specific session, as text
mcpsnoop export -T json | jq              # the newest session, piped to jq
mcpsnoop export -T otlp -o trace.json     # import into an OTLP-compatible tracing backend
```

Omit `-o` to write to stdout, and omit the session to take the newest, or pass
`-` to read JSONL from stdin. In the TUI, press `e` to export the selected
session as HTML, or run `:export json|html|text|otlp [path]` from command mode.

### Stream completed calls to an OTLP collector

Send spans while the proxy is running by pointing it at an OTLP/HTTP JSON
traces endpoint. Repeat `--otlp-header` for collector authentication or tenant
headers.

```bash
mcpsnoop \
  --otlp-endpoint http://localhost:4318/v1/traces \
  --otlp-header "Authorization=Bearer $OTLP_TOKEN" \
  -- node build/index.js

mcpsnoop http \
  --target http://localhost:3000/mcp \
  --otlp-endpoint http://localhost:4318/v1/traces
```

Delivery is best-effort and never blocks proxied MCP traffic. If the collector
is unavailable, mcpsnoop retries in the background and drops new trace frames
when its bounded queue is full. The normal JSONL session log remains the durable
record.

## Comparing sessions

Compare two saved sessions by id or JSONL path.

```bash
mcpsnoop diff before-session after-session
mcpsnoop diff old.jsonl new.jsonl
```

The report shows tools that were added or removed, `inputSchema` changes,
matching tool calls whose status changed, and notable duration shifts. Calls are
matched by tool name and arguments, so reordered calls still compare correctly.
By default, duration changes must differ by at least 100 ms and 2x; use
`--duration-threshold` and `--duration-ratio` to adjust those cutoffs.

## Checking sessions in CI

Gate a recorded agent run on errors, stream corruption, protocol warnings, slow
calls, or calls that never got a response.

```bash
mcpsnoop check [--fail-on error,invalid,warn,slow,pending] [--slow-threshold 1s] [session-id|log.jsonl|-]
```

The three default signals (error, invalid, warn) fail the check. Add `slow` to
gate on calls longer than `--slow-threshold`, one second by default, and
`pending` to gate on calls that never got a response. Pass a comma-separated
subset to select only the conditions relevant to a job. Omit the session to
check the newest capture, or use `-` to read JSONL from stdin.

```bash
mcpsnoop check build-agent
mcpsnoop check --fail-on error,invalid artifacts/session.jsonl
mcpsnoop check --fail-on error,slow --slow-threshold 2s - < trace.jsonl
```

## Watching from another machine

Keep capture local to the machine where the traffic happens and use SSH for the
network hop, so mcpsnoop never needs a remote transport of its own.

### Live view

Run the TUI on your workstation and forward the remote machine's mcpsnoop socket
back to it. The live tunnel uses SSH Unix-socket forwarding, so both ends must
run Linux or macOS. On Windows, use the post-mortem log copy below.

```bash
# on your workstation, start the TUI
mcpsnoop

# create the remote socket directory once
ssh remote-user@remote-host 'mkdir -p ~/.local/state/mcpsnoop'

# print the tunnel command, then run the printed ssh -R line
mcpsnoop remote remote-user@remote-host

# on the remote host, wrap your server as usual
mcpsnoop -- node build/index.js
```

The socket lives under the remote's state directory, resolved as `MCPSNOOP_HOME`,
else `XDG_STATE_HOME/mcpsnoop`, else `~/.local/state/mcpsnoop`. By default mcpsnoop
assumes the Linux home `/home/<user>` from your `user@host` and prints a reminder
to stderr whenever it falls back to that guess. If the remote resolves elsewhere,
name the one non-default piece.

```bash
# a non-Linux or custom home, macOS is /Users/<user> and root is /root
mcpsnoop remote --remote-home /Users/remote-user remote-user@remote-host

# an explicit MCPSNOOP_HOME on the remote
mcpsnoop remote --remote-mcpsnoop-home /srv/mcpsnoop remote-user@remote-host

# an explicit XDG_STATE_HOME on the remote
mcpsnoop remote --remote-xdg-state-home /var/lib/state remote-user@remote-host
```

### Post-mortem

Stream a remote session straight into the TUI over SSH, no local copy needed.

```bash
ssh remote-user@remote-host 'cat ~/.local/state/mcpsnoop/sessions/session.jsonl' | mcpsnoop open -
```

To keep a local copy instead, scp the logs into your sessions directory and run
the TUI as normal.

```bash
# copy the remote logs into your local sessions directory
mkdir -p ~/.local/state/mcpsnoop/sessions
scp remote-user@remote-host:'~/.local/state/mcpsnoop/sessions/*.jsonl' \
  ~/.local/state/mcpsnoop/sessions/

# open the TUI, it backfills the copied sessions
mcpsnoop
```

## Security

mcpsnoop runs the server command you wrap, so only wrap servers you trust, and
run untrusted ones in a container. It never executes anything you didn't put in
your client config.

Captured frames can include prompts, tool arguments, credentials, and tool
results. If payloads can carry secrets, opt in to redaction to scrub the
observed trace copies while the proxied bytes still pass through unchanged.
Key-based redaction replaces whole values under matching JSON object keys.
Path-based redaction replaces only values selected by a JSONPath expression,
which is useful when a common key name is sensitive in one location but safe in
another. Repeat `--redact-path` to scrub more than one location.
Value-based redaction applies regular expressions to observed string values,
stderr text, and non-JSON text frames. All redaction modes are best effort.
Regexes can miss secrets, overmatch harmless text, or fail to see transformed
or encoded values.

```bash
# built-in preset of common secret keys
mcpsnoop --redact-secrets -- node build/index.js

# or name your own keys
mcpsnoop --redact-key token,api_key,password -- node build/index.js

# scrub one location without redacting every field named password
mcpsnoop --redact-path '$.params.arguments.password' -- node build/index.js

# wildcards scrub every matching array element
mcpsnoop --redact-path '$.params.arguments.accounts[*].password' -- node build/index.js

# scrub obvious token-shaped values outside known keys
mcpsnoop --redact-value 'sk-[A-Za-z0-9]+' -- node build/index.js

# combine the layers in http mode
mcpsnoop http --target http://localhost:3000/mcp --redact-secrets --redact-value 'Bearer\s+\S+'
```

For remote workflows, use SSH tunnelling or SSH file transfer so transport auth,
encryption, host verification, key rotation, and audit policy stay in your
existing SSH setup.

## Contributing

Issues and pull requests are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for
the details.

## License

[MIT](LICENSE)
