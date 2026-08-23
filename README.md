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
compiled binary.

On Claude Desktop you don't have to make that edit by hand.

```bash
mcpsnoop wrap my-server     # route my-server through mcpsnoop
mcpsnoop unwrap my-server   # put it back
```

`wrap` finds `claude_desktop_config.json`, copies it to
`claude_desktop_config.json.mcpsnoop.bak` the first time, and rewrites only that
one server's entry, so your formatting and every other server are left alone.
Inside the rewritten entry the keys come back in alphabetical order. `unwrap`
restores the file, and removes the backup once no server is wrapped any more.
Restart Claude Desktop after either, since MCP servers are launched once at
startup.

Then use your client as usual and open the UI.

```bash
mcpsnoop
```

No flags, no socket paths, no startup order to remember. The shim and the UI find
each other on their own, and the UI backfills past sessions from disk.

For a streamable-HTTP server, run mcpsnoop as a reverse proxy.

```bash
mcpsnoop http --target http://localhost:3000/mcp --listen :7000
```

The HTTP status of every response shows in the stream, so a response that carries
no JSON-RPC message of its own is still a visible frame rather than nothing: the
401 challenge, the 403 on a rejected Origin, the 202 that acknowledges a
notification, and the 502 when the target cannot be reached at all. A 401's
`WWW-Authenticate` header is kept verbatim and shown in the inspector, since it
names the auth scheme and the resource metadata to go to next. Filter by status
with `status:401` in the TUI, or by any failure with `status:err`. A 4xx or 5xx
counts as an error, so a default `mcpsnoop check` run fails on it.

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
redact-value = "sk-[A-Za-z0-9]+"
redact-path = "$.params.arguments.password"
no-trace = false
```

Repeat `redact-key`, `redact-value`, and `redact-path` on their own lines to add
more than one of each.

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
| `mcpsnoop export` | render a session to json, html, text, har, or otlp |
| `mcpsnoop check` | fail CI on errors, invalid frames, warnings, routing mismatches, hung calls, late results, or a latency budget |
| `mcpsnoop baseline` | inspect, accept, or reset trusted tool definitions |
| `mcpsnoop diff` | compare tools and calls across two captured sessions |
| `mcpsnoop open` | open a saved session in the TUI |
| `mcpsnoop inventory` | list every server that has run through mcpsnoop on this machine |
| `mcpsnoop stats` | fold every stored capture into one row per server and tool |
| `mcpsnoop prune` | delete saved session logs older than a cutoff |
| `mcpsnoop wrap <server>` | route one of Claude Desktop's servers through mcpsnoop |
| `mcpsnoop unwrap <server>` | put that server's entry back the way it was |
| `mcpsnoop remote <user@host>` | print the SSH tunnel command |
| `mcpsnoop demo` | play a scripted session |

Run `mcpsnoop help` for the full list, or `mcpsnoop help <command>` for the flags of one.

## How it compares

| | MCP Inspector | mcpsnoop |
|---|:---:|:---:|
| Sees your real client and server traffic | no | yes |
| Flags hung calls and stream errors | no | yes |
| Flags stray output that corrupts the stream | no | yes |
| Flags malformed JSON-RPC frames | no | yes |
| Detects tool definition drift after approval | no | yes |
| Interactive terminal UI | no | yes |
| Zero-config, no flags or ordering | no | yes |
| Capability inspector | partial | yes |
| Replay a captured call | no | yes, over stdio and over HTTP |
| Session export (json / html / text / otlp) | no | yes |
| Single binary, no runtime deps | no | yes |

## Install

### npm

No Go toolchain needed. Most MCP servers are written in Node or Python, so this
is the shortest way in.

```bash
npx mcpsnoop -- node build/index.js
```

The npm package ships no code of its own. Six platform packages each carry one
build, and npm installs the single one that matches your machine, so there is
nothing to download at install time and nothing to unblock in a proxy. To keep it
around rather than fetching it each run, `npm i -g mcpsnoop`.

### Go

```bash
go install github.com/kerlenton/mcpsnoop/cmd/mcpsnoop@latest
```

### Homebrew

```bash
brew install mcpsnoop
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

The hub loads the newest 100 saved sessions by default, keeping startup work
bounded without deleting older traces. Use `mcpsnoop --history-limit N` to pick
another limit, or `mcpsnoop --history-limit 0` to load the full history. Older
sessions remain available through `mcpsnoop open <session-id>` and
`mcpsnoop export <session-id>`.

The history limit bounds how many sessions are loaded. Inside a session, the live
TUI is bounded twice over, because a hub left watching a chatty server otherwise
grows until it is killed. It keeps at most 64 MiB of frame bodies, releasing the
oldest first, and at most 200,000 frames, dropping the oldest entirely past that.
The first bound is what a capture of large payloads runs into and the second what
a long stream of small notifications does.

Neither bound changes an answer. A frame whose body was released keeps its row,
its verdict and its place in the timeline, and its inspector says the body is
gone rather than showing an empty frame. A frame that was dropped outright takes
its tool call's statistics with it into the running totals first, so the tool
summary and what the server costs you in context describe every call the session
made, not only the recent ones. The stream footer says how many older frames are
on disk only, and `r` refuses a frame whose params it no longer holds rather than
replaying something else.

`mcpsnoop open <session-id>` reads the log and holds all of it, and exporting
from the TUI reads the log too, so neither is bounded. `check`, `export` and
`diff` build an unbounded store on purpose, since a gate that under-reports on a
large capture is worse than one that uses the memory.

The history limit bounds what is loaded; `mcpsnoop prune` bounds what is kept.
It deletes saved session logs older than a cutoff, and never runs on its own.

```bash
mcpsnoop prune --older-than 30d --dry-run   # list what would go, remove nothing
mcpsnoop prune --older-than 30d             # delete after confirming
mcpsnoop prune --older-than 72h --yes       # skip the prompt in a script
```

`--older-than` is required (there is no default that would delete anything) and
accepts a day count like `30d` or a Go duration like `72h`. Tool baselines are
left alone, since a baseline is keyed by server label rather than by session.

Because it sits in the actual pipe, not off to the side like the Inspector, it
sees exactly what your real client and server say to each other, whatever the
server is written in.

## Keybindings

| Key | Action | | Key | Action |
|---|---|---|---|---|
| `enter` | inspect / drill in | | `/` | filter |
| `esc` | back | | `:` | command |
| `j` / `k` | move | | `r` / `R` | replay / edit and replay |
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
| `id:` | request id, and any retry continuing it | `id:7` |
| `task:` | task id | `task:01J...` |
| `dir:` | direction (`c2s`, `s2c`) | `dir:s2c` |
| `kind:` | frame type (`req`, `resp`, `notify`, `stderr`, `invalid`) | `kind:invalid` |
| `status:` | call outcome (`ok`, `error`, `cancel`, `late`, `cancelled`, `pending`, `bad`, `warn`, `mismatch`, or an HTTP status like `401`) | `status:error` |

Stack tokens to get specific.

```text
tool:search status:pending        # in-flight calls to one search tool
status:cancel                     # calls the client gave up on (status:cancelled is a cancelled task)
status:late                       # results that arrived after the cancellation
method:tools/call status:error    # tool calls that failed
dir:s2c kind:req                  # server-initiated requests (servers before 2026-07-28)
```

The last one only finds anything on a server speaking 2025-11-25 or earlier. The
2026-07-28 revision removed server-initiated requests, and a server that needs
something from the client now answers the client's own request asking for it,
then the client retries. mcpsnoop links those retries back to the request they
continue, so the exchange reads as one call rather than several.

## Exporting sessions

Turn any captured session into a portable file.

```bash
mcpsnoop export -T json|html|text|har|otlp [-o file|-] [session-id|log.jsonl|-]
```

| Format | What you get |
|---|---|
| `json` | correlated calls, per-tool counts and p50/p95/p99 latency, slowest calls, capabilities, and raw frames |
| `html` | a self-contained browser file with search and collapsible JSON |
| `text` | a pretty plain-text dump |
| `har` | one entry per correlated call, openable in browser devtools and anything else that reads HAR |
| `otlp` | OTLP JSON with a span per correlated call; W3C trace context joins caller traces, otherwise one trace is used per session |

MCP is not HTTP, so a HAR entry's URL, status code, and timings are a deliberate
mapping of each call rather than a wire transcript.

For OTLP, a request's `_meta.traceparent` supplies that call's trace and parent
span IDs, and `_meta.tracestate` rides along on the span. When the traceparent is
absent or invalid, mcpsnoop keeps the session-derived trace and carries no state.
mcpsnoop observes rather than participates, so it adds no vendor entry of its own
and passes the caller's state through unchanged.

```bash
mcpsnoop export -T html -o out.html                    # an HTML file to open in a browser
mcpsnoop export -T text server.py-48213-7f3a1c9e2b04   # a specific session, as text
mcpsnoop export -T json | jq                           # the newest session, piped to jq
mcpsnoop export -T har -o session.har                  # a HAR file to open in browser devtools
mcpsnoop export -T otlp -o trace.json                  # import into an OTLP-compatible tracing backend
```

Omit `-o` to write to stdout, and omit the session to take the newest, or pass
`-` to read JSONL from stdin. In the TUI, press `e` to export the selected
session as HTML, or run `:export json|html|text|har|otlp [path]` from command mode.

To scrub an existing capture before inspecting or sharing it, pass the same
redaction flags used during capture to `export` or `open`:

```bash
mcpsnoop export session.jsonl --redact-secrets --redact-key project_token -o shared.json
mcpsnoop open session.jsonl --redact-path '$.params.arguments.password'
```

These flags rewrite the exported file or the in-memory TUI view, never the
source JSONL. `export` refuses an output that names the same file as its input,
and writes through a temporary file that is renamed into place, so a run that
fails leaves the previous file whole.

A tool's `inputSchema` and `outputSchema`, as advertised in a `tools/list`
result, are left alone by `--redact-key` and `--redact-secrets`. A name inside a
schema is a type declaration rather than a value, the name itself stays in the
log either way, and scrubbing the subschema under a property called `token`
would take the tool's own checks with it. The exemption is that position only,
so an argument that happens to be called `inputSchema` is scrubbed like any
other, and it stops at `default`, `const`, `examples` and `enum`, which hold
data rather than structure. Use `--redact-path` to name something inside a
schema, or `--redact-value`, which matches text wherever it sits except in the
two keywords mcpsnoop parses, `type` and `x-mcp-header`.

What each flag reaches differs, so check the result rather than assuming. All
four scrub JSON-RPC payloads, and `--redact-key`, `--redact-path` and
`--redact-secrets` reach only those. Only `--redact-value` also scrubs stderr,
other non-JSON text, and the inside of a string. An `Mcp-Param-*` header is
scrubbed alongside the body value it mirrors; the other envelope metadata,
server labels, `Mcp-Name`, `Mcp-Method` and the HTTP status, is left as
captured. Redaction is best effort, so use a separate output path and read the
result before sharing it.

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

The report shows tools that were added or removed, description and `inputSchema`
changes, matching tool calls whose status changed, and notable duration shifts. Calls
are matched by tool name and arguments, so reordered calls still compare correctly.
By default, duration changes must differ by at least 100 ms and 2x; use
`--duration-threshold` and `--duration-ratio` to adjust those cutoffs.

Pass `--exit-code` to gate CI on regressions: it exits non-zero when the after
session drops a tool, changes a tool description, title, input schema, output
schema or annotations, has a call whose status got worse, or slows down.
Improvements (added tools, fixed calls, speedups) still exit zero, and so does an
icon change, which alters how a tool looks without changing what it does.

## Checking sessions in CI

Gate a recorded agent run on errors, stream corruption, protocol warnings,
routing-header mismatches, calls that never got a response, dropped frames that
leave the capture incomplete, tool-definition drift, or use of deprecated
protocol features.

```bash
mcpsnoop check [--format text|junit|sarif] [--fail-on error,invalid,warn,mismatch,pending,late-result,drift,deprecated,incomplete,schema] [session-id|log.jsonl|-]
```

`error`, `invalid` and `warn` fail the check on their own. The rest are opt-in.
Pass a comma-separated subset to gate on only what a job cares about, omit the
session to check the newest capture, or use `-` to read JSONL from stdin.

| Signal | Fails on |
|---|---|
| `error` | a call answered with a JSON-RPC error, a result marked `isError`, or a task that ended in a failure |
| `invalid` | a frame on the protocol channel that is not valid JSON-RPC, usually a server logging to stdout |
| `warn` | a frame breaking an expectation the MCP or JSON-RPC specification sets |
| `mismatch` | a routing header disagreeing with the body, riding a batch, or missing where the revision requires it |
| `pending` | a request still open when the capture ended, so the caller was left waiting |
| `late-result` | a response that arrived after its request was cancelled |
| `drift` | an advertised tool definition changing after the baseline was approved |
| `deprecated` | a feature the specification has deprecated |
| `incomplete` | frames dropped upstream, which makes every other count a floor rather than a total |
| `schema` | an advertised schema using a construct or a dialect that travels badly across clients |

Every signal is counted whether or not it is gating, so a run says what it found
before you decide what should fail on it.

```
session build-agent: errors=1 invalid=0 warnings=0 mismatches=0 pending=0 late_results=0 deprecated=0 missing_frames=0 schema_findings=1
schema findings:
  oneOf: search
check failed: error
```

The dropped-frame count travels with the artifacts too, so a capture that
understates itself says so wherever it is opened: `missing_frames` in the JSON
export, `log.comment` in HAR, and the `mcpsnoop.session.missing_frames` resource
attribute in OTLP.

```bash
mcpsnoop check build-agent
mcpsnoop check --fail-on error,invalid artifacts/session.jsonl
mcpsnoop check --fail-on mismatch gateway-run.jsonl
```

The exit code says which of two things happened, and a CI wrapper needs the
difference. **1 means the check ran and something failed the gate**, so the
findings are real and worth publishing. **2 means the check never happened**: a
path that is not there, a file that is not a session log, a state directory
holding nothing, a flag that does not parse. Nothing is written to stdout on a 2,
so a pipeline never uploads an empty report as though it were a verdict.

Beyond the signal counts, assert the shape of the run. These compose with each
other and with `--fail-on`, and any failure exits non-zero.

| Flag | Fails when |
|---|---|
| `--max-duration <dur>` | one or more completed tool calls exceeded the budget; reports their count and the worst call |
| `--expect-tool <name>` | the named tool was never called (repeatable) |
| `--forbid-tool <name>` | the named tool was called (repeatable) |

```bash
# a contract for the run: search must run, delete must not, nothing over 2s
mcpsnoop check --expect-tool search --forbid-tool delete --max-duration 2s run.jsonl
```

### Report it where CI already looks

`--format junit` writes one `<testcase>` per signal and session, and its failures
follow the same `--fail-on` selection as the text output.

```yaml
- name: Check captured MCP session
  run: |
    mkdir -p test-results
    mcpsnoop check --format junit artifacts/session.jsonl > test-results/mcpsnoop.xml
- name: Upload mcpsnoop JUnit report
  if: always()
  uses: actions/upload-artifact@v4
  with:
    name: mcpsnoop-junit
    path: test-results/mcpsnoop.xml
```

`--format sarif` writes a SARIF 2.1.0 log instead. Where junit reports one
aggregate per signal, SARIF reports one result per finding, carrying the session,
the frame `Seq` and the frame's own warning or drift text, and pointing at the
line of the log the frame was decoded from. A signal named in `--fail-on` is
reported at level `error` and one outside it at level `note`, so the report and
the gate never disagree.

A result points at the log the finding came from, and how depends on where the
log was read from. A path inside the working directory becomes a relative one,
which code scanning resolves against the repository root. Anything else, a path
elsewhere on disk or a session id resolved out of the state directory, becomes an
absolute `file://` URI. Reading from stdin gives a result no location at all,
since there is no file to point at.

The alert renders with its surrounding lines only when that path is a file in the
analysed commit, so a capture the workflow generated into `artifacts/` opens an
alert carrying the message, the rule and the line number but no source view.
Committing a capture you want rendered in full is the only way to get one.

Code scanning rejects a file whose run holds more than 25,000 results and
displays only the top 5,000 of what it accepts, so the report is capped at 5,000:
the findings the gate failed on first, then a `mcpsnoop/report-truncated` result
saying how many were left out. The text and junit formats stay complete.

To put the findings in the Security tab, hand the SARIF log to
`upload-sarif`. The job needs `security-events: write`, or the upload answers
403. `check` exits non-zero on a finding, so the upload step needs `if: always()`
to run at all on the runs that have something to report; `continue-on-error`
hands the verdict to the code scanning check, which fails on an `error`-level
alert and can be made a required check. Drop it if you would rather the check
step itself be what turns the job red.

```yaml
permissions:
  # required for all workflows
  security-events: write
  # only required for workflows in private repositories
  actions: read
  contents: read

steps:
- name: Check captured MCP session
  continue-on-error: true
  run: mcpsnoop check --format sarif artifacts/session.jsonl > mcpsnoop.sarif
- name: Upload mcpsnoop SARIF report
  if: always()
  uses: github/codeql-action/upload-sarif@v4
  with:
    sarif_file: mcpsnoop.sarif
    category: mcpsnoop
```

### Catch a routing header that disagrees with the body

On the streamable-HTTP transport a gateway routes on `Mcp-Method` and `Mcp-Name`
while the server reads the body, so a header that disagrees with the body means
the two are looking at two different requests. The `mismatch` signal covers that,
a header riding a batch it cannot address, and a required header missing
entirely.

In 2026-07-28 a missing routing header is a validation failure, and a compliant
server rejects the request with `400` and `-32020`. mcpsnoop raises it only once
the session is known to speak that revision or later, since earlier revisions do
not define these headers at all and omitting them there is correct. A server's
own `-32020` rejection counts as the same signal.

A name or resource URI that will not fit in an HTTP field value travels Base64 in
a `=?base64?…?=` sentinel, which is decoded before the comparison, so a client
that encodes correctly is never flagged.

On HTTP `tools/call` requests mcpsnoop also shows each `Mcp-Param-{Name}` header
and, when the matching advertised tool definition is known, compares it with the
annotated argument path. Nested properties, the Base64 sentinel, booleans and
numeric-equivalent safe integers are handled without string-comparison false
positives. Unknown parameter headers and sessions without a matching tool
definition stay observational. Key- and value-based redaction applies to captured
parameter-header values before they reach a sink, and a value mcpsnoop scrubbed
itself is never reported as a disagreement.

### Check the transport headers the spec makes mandatory

The routing headers above were the only ones a frame carried, so the rest of the
Streamable HTTP transport's mandatory headers reached nothing that could check
them. `Content-Type` was the sharpest case: the response side already read it to
tell an SSE stream from a JSON body, then threw it away.

An HTTP frame now carries the headers the transport states rules about, and two
of those rules are checkable.

| Rule | Reported as |
|---|---|
| the client **MUST** send an `Accept` listing both `application/json` and `text/event-stream` | `warn` on the request |
| a server answering a JSON-RPC request **MUST** return `Content-Type: application/json` or `text/event-stream` | `warn` on the response |

Both sentences read the same in 2025-11-25 and 2026-07-28, so unlike the drift
and extension checks these need no revision gate. `Origin` is recorded too, since
servers **MUST** validate it and **MUST** answer `403` when it is invalid, but
mcpsnoop cannot know your allowed origins so it shows the value rather than
judging it.

Wildcards count. A client sending `*/*` has offered both types and is never
reported, and a `charset` parameter on a `Content-Type` is ignored. A log
captured before mcpsnoop recorded these headers stays silent rather than
reporting every frame in it for a header nobody wrote down, and stdio never has
them at all.

`Authorization` is deliberately not captured. Turning a challenge into token
facts is its own problem and putting a bearer token on disk is not the answer to
it. `Mcp-Session-Id` and `Last-Event-ID` are not captured either: 2026-07-28
removed both and tells a server to ignore them, so there is no rule left to check.

### Detect tool definition drift

The first complete `tools/list` observed for a server label becomes its trusted
baseline. Later sessions compare that baseline field by field: the description,
the title, the input and output schemas, the annotations and the icons, plus
tools that were added or removed. Annotations matter most, since a tool approved
with `readOnlyHint` that later declares itself destructive is the rug-pull this
check exists for, and the spec tells clients to treat annotations as untrusted.
The title and the icons are tracked because they are what the user sees, and the
spec ranks a tool's `title` above `annotations.title` and its name. The sessions
table and tool summary flag drift without blocking or changing MCP traffic.

Annotations are compared through their spec defaults, so a server that starts
spelling out a hint it was already relying on is not reported. A baseline
recorded before mcpsnoop tracked a field keeps working for the fields it does
record and says which ones it cannot answer for; re-record with
`mcpsnoop baseline --accept` once you trust the current definitions.

Changing what redaction records changes what drift compares. A baseline taken
without `--redact-value` and then checked against a capture taken with one
reports the scrubbed fields as changed, which is correct, since the recorded
definition really did change. Re-record with `--accept` after changing redaction
settings.

Use a stable, unique `--label` for each server whose command name or target host
would otherwise collide. Baselines are stored under the normal mcpsnoop state
directory, so `MCPSNOOP_HOME` and `XDG_STATE_HOME` apply.

```bash
mcpsnoop check --fail-on drift session.jsonl
mcpsnoop baseline session.jsonl
mcpsnoop baseline --accept session.jsonl  # trust a legitimate definition change
mcpsnoop baseline --reset session.jsonl   # trust the next complete tools/list
```

In ephemeral CI the state directory starts empty, so a run has nothing to
compare against and records the baseline instead of verifying it. **A run that
asked to fail on drift and then verified nothing does not pass**, and says which
directory to persist. That is the only case where recording a baseline is a
failure: without `drift` in `--fail-on`, recording one is business as usual and
changes no exit code.

So the baseline has to survive between runs for a drift gate to mean anything.
Point `--baseline` at a checked-in or cached directory, or set `MCPSNOOP_HOME` to
a persisted path.

```
recorded first-seen tool baseline (trusted, not verified)
check failed: drift
```

```bash
mcpsnoop check --fail-on drift --baseline .mcpsnoop/baselines session.jsonl
```

`drift` is opt-in for `check`; the default `error,invalid,warn` gate is unchanged.

### Catch a feature neither side negotiated

SEP-2133 moved optional features out of the core protocol and into extensions,
advertised in the `extensions` map of each side's capabilities. Tasks is one of
them, so on 2026-07-28 a `tasks/get`, a `notifications/tasks` or a `tools/call`
answered with a task handle only means anything when the other side said it
speaks Tasks.

When it did not, the spec is explicit: the supporting party **MUST** either fall
back to core behaviour or reject the request. Doing it anyway is why a feature
appears to be wired up and then quietly does nothing, and what a reader gets
instead is a `-32601` or a `-32021` several frames later, or a task that never
progresses. mcpsnoop warns on the frame that reached for the extension and names
which side never advertised it.

```
tool "slow" answered with a task handle uses the io.modelcontextprotocol/tasks
extension, which the client never advertised
```

It is a `warn`, so a default `check` run fails on it. It stays quiet whenever the
capture cannot show what was negotiated, which is a capture that starts after the
handshake or one whose capabilities your own redaction scrubbed, and on revisions
before 2026-07-28, where `tasks/*` are core protocol and using them is correct.

### Flag deprecated protocol features

The 2026-07-28 revision deprecates Roots, Sampling, and Logging. They keep working
for at least a year, so mcpsnoop marks them rather than treating them as errors.
The stream, the capability inspector, and the export all flag them, and each marker
names the replacement.

Two of the three are now reachable only through a multi round-trip request, where
the method name sits inside the server's `inputRequests` map rather than on the
frame itself. Those are flagged too, so a server that moved to the new pattern
does not silently stop reporting.

```bash
mcpsnoop check --fail-on deprecated session.jsonl
```

Like `drift`, `deprecated` is opt-in. A default run reports the count and stays
green, so a session using a still-legal deprecated feature never turns CI red on
its own.

### Flag schema constructs clients handle badly

A server can be perfectly valid and still be hard for an agent to use. Clients
differ in how much of JSON Schema they really support, and a tool the model keeps
calling wrongly is often a tool whose schema asked for more than the client
delivers.

The tool summary, opened with `s`, has a SCHEMA column naming the most notable
thing about each advertised tool's schema, with a trailing `+` when there is more
than one kind.

| Shown | Means |
|---|---|
| `no root` | the `inputSchema` is absent, is not a JSON object, or has a root type other than `"object"` |
| `dialect` | a `$schema` naming a dialect other than the 2020-12 the revision defaults to |
| `ext ref` | a `$ref` pointing outside the document, which is also the case the spec warns implementers not to follow blindly |
| `oneOf`, `anyOf`, `allOf`, `not` | a composition keyword, handled inconsistently across clients |
| `ref` | a `$ref` pointing inside the same document |
| `untyped` | a property that declares no type and no other way of saying what it accepts |

All but the first are observations rather than verdicts. A schema using `oneOf`
is not wrong, only likely to be read differently by different clients, and a
schema may declare whatever dialect it likes. `no root` is the exception: the
`Tool` definition requires `inputSchema` and pins its root type to `"object"`, so
a client validating a listing rejects that tool outright and it never becomes
callable, with nothing on the wire to say why. `no root` leads the column for
that reason, and a schema mcpsnoop's own redaction scrubbed is never reported,
since an unreadable schema is not a wrong one.

That split decides what `check` does with them. `no root` is a warning on the
`tools/list` frame, so it fails the default `error,invalid,warn` gate with no
flag at all, which is the point: a server that ships an unusable tool answers
every handshake normally and simply never receives a `tools/call`. The
observations are counted as `schema_findings` and reported under `schema
findings:`, and only fail the run when you add `schema` to `--fail-on`. Both
reach `--format junit` and `--format sarif`, and `export` carries the per-tool
list under `summary.definitions.per_tool[].findings`.

```bash
mcpsnoop check session.jsonl                     # a non-object root already fails this
mcpsnoop check --fail-on schema session.jsonl    # and now so do the observations
```

The column carries the warning color and never the red of the ERR column, and
mcpsnoop still changes nothing about the traffic it forwards.

Nothing is resolved or fetched. An external `$ref` is recognized by its form
alone, and the schema it points at is never read.

### Replay a call captured over HTTP

`r` re-issues a captured call against a live server. For a stdio capture the
command is in the log, so mcpsnoop launches an isolated copy and sends the
request to that. An HTTP capture has no command to launch, and the endpoint it
records is stripped of its userinfo and every query value, so it names the server
without being an address to dial.

So you say where a replay goes, and mcpsnoop never dials a production endpoint
because somebody pressed a key.

```bash
mcpsnoop open --replay-target https://api.example.com/mcp session.jsonl
mcpsnoop open --replay-target https://api.example.com/mcp \
  --replay-header 'Authorization: Bearer sk-…' session.jsonl
```

Without `--replay-target` an HTTP session says so rather than offering a key that
cannot work. With one, `r` still asks before the first send of a session, the
same way a recorded command is answered for before it is run.

A credential reaches the server through `--replay-header` and nowhere else.
mcpsnoop records no `Authorization` header and replays none, so there is nothing
captured for a replay to leak.

The replayed POST carries what the transport makes mandatory, which a POST of the
bare captured body does not: `MCP-Protocol-Version`, an `Accept` listing both
`application/json` and `text/event-stream`, `Mcp-Method`, `Mcp-Name` where the
spec requires it, and every captured `Mcp-Param-*`. Those are re-sent verbatim
from the capture, base64 sentinel and all, so they cannot disagree with the body
the way a re-derivation could. The one header that is not copied is the protocol
version, because the replayed body declares the revision mcpsnoop speaks and the
header has to match the body.

`Mcp-Name` is derived from the body being sent rather than copied, because the
spec sources it from `params.name` or `params.uri` and requires a server to
reject a header that disagrees with the body, so an edit that renames the tool
would otherwise send the old name. The `Mcp-Param-*` headers mirror the captured
arguments, so an edited replay sends none of them rather than asserting
something about a body somebody rewrote. A capture can only set headers in that
one family: a log is a file people hand around, and letting it name any header
would let it overwrite the mandatory ones or add a credential nobody passed.

A `Mcp-Param-*` a redaction rule scrubbed stops the replay with a reason. Sending
the placeholder would put mcpsnoop's own bytes on a live server as though a user
had typed them.

A redirect is refused rather than followed. The address is the one you named and
answered for, and following a 307 would hand that choice to the far end, resending
the body and, on a hop that only changes the port, the credential too. mcpsnoop
reports where the server wanted to send it and lets you decide whether to name
that one instead.

An answer arriving as a single JSON object and one arriving as an event stream
are both read, and a failure is named rather than numbered: a 401 reports the
scheme the server demanded, a `-32020` reports what it objected to, and a
non-JSON-RPC 400 or 404 says the address is not a Streamable HTTP endpoint of
this revision.

### Tell the server's latency from the user's

Under multi round-trip requests one tool call is several requests, and the
seconds a person spent answering an elicitation sit inside the span. That is
deliberate, since that interval is usually the one you most want to see, but it
means one number cannot answer both questions.

On a `book_flight` chain where the server worked 1.2 seconds while the user took
37, `check --max-duration 5s` blames the tool for 38.2 seconds. It still does,
because changing what that flag means would loosen every pipeline that already
sets it. Two siblings name what they measure instead.

```bash
mcpsnoop check --max-server-duration 1s session.jsonl   # the server's share alone
mcpsnoop check --max-round-trips 2 session.jsonl        # how chatty a tool is
```

```
assertion failed: 1 tool call exceeded the 1s server budget (worst: tool "book_flight" held for 1.2s)
assertion failed: 1 tool call exceeded the 2 round trip budget (worst: tool "book_flight" took 3)
```

Both are off by default, so a default `check` run is unaffected, and both are
read off frame timestamps and a link mcpsnoop already inferred, so neither
guesses at intent.

Press `i` in the TUI for the breakdown, or read `interactions` in the json,
text and html exports. Each entry is one logical operation with its round trip
count, its total, the share the server held it for and the share it was waiting
on the client, plus a per-hop line naming what each answer asked for. The
per-tool summary gains a `TRIPS` column so a chatty tool is visible without
opening anything.

`export --format har` puts the server's share in `wait` and the rest in
`blocked`, which is what that field is for, so a viewer stops drawing a 38-second
server wait that never happened.

The counts and the two shares are accumulated as frames arrive rather than
derived when you ask, because the live store releases old frames to stay inside
its budget and a derived answer would quietly be a window instead of a chain.
The per-hop breakdown is read from the frames still held, and says so when it is
only part of one. `ServerTime + ClientTurnaround` equals the total by
construction rather than by arithmetic anyone has to trust.

`--max-round-trips` judges a chain that is still running, because every request
it has already made is countable and a server asking again and again produces
exactly the operation nobody ever finishes. `--max-server-duration` waits for an
ending, which is the rule `--max-duration` already applies, since an operation
still open has no latency to judge.

An operation mcpsnoop could not link stays its own single-hop entry. `matchRetry`
refuses an ambiguous link on purpose, and this view does not fill that gap in.

An operation that took one request carries no hop breakdown, because a single
hop restates the totals above it word for word. A chain reports one hop per
request, and says so when the store no longer holds every frame or when work
settled off the request and answer pair a hop is made of, which a task handle
does.

### See what a server asked your user for

Elicitation is the one path in MCP where a person types data into a server, and
under MRTR the question and the answer are no longer two halves of one exchange.
The question is buried in an `InputRequiredResult`, the answer comes back inside
`inputResponses` on a retry under a different id, and the only thing that ties
them together is the link mcpsnoop already infers.

Without that pairing a declined password request reads as a plain tool error.

```
tools/call login_legacy [form] creds: decline after 3s
  password string
```

Press `l` in the TUI, or read `elicitations` in the json, text and html exports.
Each row names the operation the question interrupted, the mode, the message,
what was asked for, what the user did and how long they took. A question no
retry ever answered shows as pending, which MRTR makes an ordinary outcome
rather than an error, since the spec tells servers not to assume a client will
retry at all.

Form rows list the `requestedSchema` property names and their declared types. A
property whose subschema a redaction rule replaced shows an unknown type rather
than the placeholder, because a placeholder is not something the server
declared. URL rows carry the address whole, which the spec makes a client show
before consent, and name the host on its own, which it says to highlight against
subdomain spoofing.

**The ledger never carries a submitted value.** What a user typed stays in the
capture for whoever needs it, and leaving it out of a summary surface built to
be exported and pasted around is what keeps this out of the redaction story
entirely. It matters most in url mode, where the spec puts credentials on
purpose.

A retry answers the round it was issued from and no other. MRTR tells a server
that when a client omits some of what was asked it should ask again in a new
round, so an earlier round holding one unanswered key beside an answered one is
ordinary traffic, and the unanswered half stays pending rather than borrowing the
later round's answer.

One recorded question is bounded. The message, the url and the field list are
held for the life of the session, outside the frame budget that releases bodies,
so a server cannot make one arbitrarily expensive. The limits are far above any
real question and a truncated message says it was truncated.

Nothing here warns and nothing here changes a `check` exit code. A ledger
records what happened; it does not judge it.

### Find the tool that fails one run in four

`check` reads one session and `diff` reads exactly two, so a tool that fails
occasionally stays invisible until somebody opens the captures by hand. Over
sixteen captures of a server whose `run_query` answers `isError` about a quarter
of the time, `check` reports the newest one, honestly, as clean.

```bash
mcpsnoop stats
mcpsnoop stats --since 7d --label prod
mcpsnoop stats --limit 20 --format json
```

```
read 16 logs of 16 in ~/.local/state/mcpsnoop/sessions

SERVER       TOOL          CALLS   ERR  PROTO    FAIL%       SESS       p50      p95      p99      DEF
flaky-demo   run_query        13     3      0    23.1%       3/13     434ms    519ms    519ms     195B
docs-mirror  run_query         3     1      0    33.3%        1/3     357ms    434ms    434ms     195B
docs-mirror  search_docs      12     0      0     0.0%        0/3     377ms    386ms    386ms     200B
flaky-demo   search_docs      52     0      0     0.0%       0/13      42ms     58ms      59ms    200B
```

`ERR` and `PROTO` are separate columns because the specification makes them
separate things. A tool answering `isError` is reporting something a model can
act on and retry; a JSON-RPC error is the request or the server being wrong.
`SESS` is the count of sessions that saw a failure over the sessions that called
the tool, which is the "one run in ten" question a rate over calls cannot
answer.

Rows key on the server and the label together. The server is the recorded
command and working directory for stdio and the endpoint for HTTP, the same
identity `inventory` uses. Either half alone pools something it should not: the
label alone merges two servers that derive one name, which happens whenever two
checkouts of a project run the same entry point, and the identity alone merges
one command deliberately run as `prod` and again as `staging`. Both mistakes
smear two clean distributions into one that describes neither.

When two rows do share a label, the `SERVER` cell carries the working directory
or endpoint that tells them apart, and the JSON carries `command`, `cwd` and
`endpoint` on every row. A name that was never ambiguous is left alone, so the
ordinary table is unchanged.

Every session in a log is folded, not just the first, so a file made by
concatenating captures counts all of them.

Percentiles are pooled over the raw durations. A median of medians is a median
of nothing. One multi round-trip operation is one call with one duration however
many requests it took, and a call still open counts toward `CALLS` while
contributing no latency.

One capture is resident at a time. A log is loaded, folded into the running
counters, and dropped before the next one opens, so a directory of hundreds
costs the largest single capture rather than their sum.

`--limit` defaults to a hundred of the newest logs and the header says how many
of how many were read, so a bounded answer never passes for a complete one.
`stats` reports and does not gate: it writes nothing, touches no baseline, opens
no socket, and exits 0 whenever the walk succeeded.

### See which servers have actually run here

The finding people keep repeating about Shadow MCP is that organisations
discover several times more MCP servers running than anyone approved, because a
server is often just a dependency somebody added to an IDE plugin. The same
thing happens in miniature on one laptop, and mcpsnoop has been recording the
answer the whole time without ever showing it.

```bash
mcpsnoop inventory
mcpsnoop inventory --tools          # also count what each server last advertised
mcpsnoop inventory --format json    # for something else to read
```

One row per server rather than per session. The row key is the recorded command
and working directory, never the label, because the label comes from the
command's last path element and `node ~/one/build/index.js` and
`node ~/two/build/index.js` both derive `index.js`. An HTTP session keys on the
endpoint it proxied instead, since mcpsnoop launched nothing there.

Reading is one envelope per log, the meta frame the proxy writes first, so this
stays cheap over a directory of large captures. `--tools` is the exception and
reads one log per server, the most recent run of each, which is why it is a flag
rather than a column. Even then the read is bounded, because a tool inventory is
session state the store folds in as it goes, so a hundred-megabyte capture is
read through a fixed window rather than held whole to produce one integer.

When there is no count the row says which of three things happened, because a
log that could not be read is not a server that advertised nothing, and one
sentence for both would make mcpsnoop state something false.

A command a `--redact` rule rewrote is printed as recorded and marked, rather
than passed off as the command that ran. Two runs of one server, one scrubbed
and one not, are two rows: mcpsnoop cannot know what the placeholder replaced,
and merging them would mean guessing the hidden halves matched. One server run
under two `--label` values is one row carrying both names, since the key is the
command rather than the name.

Nothing in a row is written by mcpsnoop. A command comes from whoever installed
the server, a working directory comes off the filesystem, and a derived label
comes from the command. A value holding a control character is quoted rather
than printed raw, so a directory whose name contains a newline cannot close the
field it is printed in and make the following lines read as servers that never
ran. An argument containing a space is quoted too, because `node "~/My Project/
build/index.js"` is otherwise indistinguishable from two arguments.

Anything the walk could not fold in is named in the header rather than dropped.
Empty logs are counted apart from damaged ones, since a zero-byte log is the
ordinary residue of a run whose exec failed or of an HTTP proxy nobody called.

Output is sorted by name rather than by recency so two runs over one directory
produce the same bytes, which is what makes it usable as a baseline to diff
against later.

Two gaps are there by construction rather than by oversight. A run with
`--trace-file` wrote outside the sessions directory and will not appear, and
`prune` deletes logs, so first seen is only ever as old as what is still on
disk. mcpsnoop reports what ran on this machine through it; it scans no network,
reads no client config it was not pointed at, and judges nothing.

### Tell a broken server from a tool that says no

A tool answering `result.isError` is working. It looked and found nothing, or it
rejected the input. A server answering a JSON-RPC error is broken. Both were one
number in the tool summary, which meant a well-behaved tool that reports domain
failures looked exactly like a broken server, and sorted above one.

The `ERR` column separates them. Red is the server side, which is a JSON-RPC
error or a task that ended failed without saying why. The warning color is the
tool's own `isError`. A tool with both shows the counts joined, red first, and a
line under the table names the two totals whenever there is a warn number to
explain. The export carries the same split as `protocol_errors` and
`tool_errors` beside the `errors` total they always add up to.

`check --fail-on error` is unchanged and still fires on either, since a gate
that ignored one of them would be a gate a server could switch off by returning
the other.

```bash
mcpsnoop export -T json | jq '.summary.tools[] | {name, errors, protocol_errors, tool_errors}'
```

### See what the server costs you in context

Tool definitions enter the model's context on every conversation, and tool
results on every call. The tool summary (`s`) measures both from the session
you actually captured.

The `definitions` line is the fixed cost: what this server's `tools/list`
weighs before a single call is made. The `DEF` column breaks that down per
tool and `RESULT` is what each tool's answers have cost so far. The table stays
sorted by errors and latency, so scan `DEF` to find the expensive definitions;
the export lists them heaviest first. A line under the table names the single
heaviest result, which a total hides.

Definition figures are the JSON with insignificant whitespace removed, so a
server that pretty-prints its `tools/list` is not counted as more expensive than
one that does not, and the same server measures the same across captures.
`RESULT` is the bytes as they arrived: a result is a one-off payload rather than
a contract worth normalising.

```bash
mcpsnoop export -T json | jq '.summary.definitions'
```

The export carries the same figures, per tool and split into description and
schema bytes, so a fat description and a fat schema stay separable and either
can be tracked across captures. `mcpsnoop diff` tells you a description or
schema changed between two sessions; the export is where the size of that
change lives.

These are **bytes, not tokens**. A token count depends on the model, so
measuring one would mean shipping a tokeniser and picking whose. Bytes are
exact and you can apply your own ratio. An unfinished `tools/list` reports what
it saw as a floor and says so, rather than passing a partial sum off as the
total.

### Detect a client that mangles server state

Under the multi round-trip pattern the server hands the client an opaque
`requestState` and the client must echo it back untouched on the retry. The
server is told to treat it as attacker-controlled input, because a client that
tampers with it can try to alter server behaviour or bypass an authorization
check.

Sitting in the pipe, mcpsnoop sees the value leave and come back, so it can say
when the contract was broken. Three ways it can break, each reported as a
protocol warning on the retry.

| Reported | Means |
|---|---|
| `MRTR retry changed requestState` | the client sent back something other than what the server issued |
| `MRTR retry is missing requestState` | the server issued one and the retry omitted it |
| `MRTR retry invented requestState` | the retry carried one the server never issued |

These are protocol violations by the client rather than observations of ours, so
they ride the ordinary warning signal and **a default `check` run fails on one**.
That is deliberate. A client mangling server state is worth stopping a build for.

The value itself is never displayed or logged, and nothing decodes or parses it.
It may be an encrypted blob carrying a principal and a token, and comparing
opaque bytes is the whole check.

One case is out of reach. When a server answers with a `requestState` and no
`inputRequests`, a tampered retry matches nothing and answers no keys, so there
is nothing left to tie it to the original request and it reads as an unrelated
call rather than a violation.

An abandoned exchange does not disturb the next one, and is not kept forever
either. Sixty-four open exchanges is far more than any client has at once, so a
session holding more is holding ones nobody will finish, and the oldest are
retired because the spec tells servers to give that state a short expiry and
reject it afterwards. Retiring is counted rather than silent: the stream footer
shows `N unlinked` and the export carries `session.retired_exchanges`, because a
retry that does arrive for a retired operation reads as its own call, and a
reader comparing counts deserves to be told.

Retiring one also lets the live store release it. A parked operation stays
pending on purpose, so its duration spans the whole exchange, and the store
refuses to forget a pending call because a response may still be coming. Once
the cap has retired an operation nothing can answer it, so holding it keeps a
call alive that no reader can reach. What the session reports does not move. It
is still counted pending and still counted in `N unlinked`, because how much
memory a record occupies and what the record says are different questions.

An abandoned exchange does not disturb the next one. MRTR tells servers they
must not assume a client will ever retry, so a user declining an elicitation
leaves an operation that no later frame will ever settle. mcpsnoop looks first
among the operations whose `requestState` presence agrees with the retry's,
which the spec makes a rule in both directions, so a conforming retry still
finds the one operation it continues even when an abandoned exchange on the same
tool is sitting beside it. The check that reports the three violations above
runs only when nothing agrees, so a genuinely non-conforming retry is still
named.

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

Key-based redaction replaces whole values under matching JSON object keys, and
the same key set is applied best effort to the wrapped server's command-line
arguments, so `--api-key=sk-x` and `--token sk-x` are scrubbed under
`--redact-secrets`. An argument that carries a secret without a recognizable flag
name cannot be detected.

The HTTP endpoint is not part of any of that, because it is not a payload you
chose to send. `--target` is a flag you have to pass to run the proxy at all, so
its URL would reach the session log whatever your redaction settings are.
mcpsnoop writes it down with the userinfo, every query value and the fragment
already removed, always, by construction rather than by pattern. Query keys
survive, since they are what tells two endpoints of one host apart, and the
fragment is dropped because it never reached the server to begin with. What is
recorded identifies the server and is not an address to dial.

Path-based redaction replaces only values selected by a JSONPath expression,
which is useful when a common key name is sensitive in one location but safe in
another. Repeat `--redact-path` to scrub more than one location.

Value-based redaction applies regular expressions to observed string values,
stderr text, and non-JSON text frames.

All three are best effort. Regexes can miss secrets, overmatch harmless text, or
fail to see transformed or encoded values.

Redaction never turns into an accusation. Every check that compares one observed
thing with another, a routing header against the body, a `Mcp-Param` value
against the argument it mirrors, a tool's schema against what the revision
requires of one, knows when mcpsnoop was the side that rewrote the bytes and
stays silent rather than reporting a server for the user's own privacy setting.
Tool-definition drift is the exception, and deliberately so, since turning
redaction on changes what is recorded and therefore what a baseline holds. See
[Detect tool definition drift](#detect-tool-definition-drift).

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
