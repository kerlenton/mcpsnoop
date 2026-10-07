# Using mcpsnoop

## Keybindings

| Key | Action | | Key | Action |
|---|---|---|---|---|
| `enter` | inspect / drill in | | `/` | filter |
| `esc` | back / undo filter or clear | | `:` | command |
| `j` / `k` | move | | `r` / `R` | replay / edit and replay |
| `g` / `G` | top / bottom | | `c` | capabilities |
| `ctrl-f` / `ctrl-b` | page | | `s` | tool summary |
| `p` | pause | | `y` | copy |
| `shift`+`<key>` | sort by column | | `e` | export |
| `ctrl-d` | delete session | | `f` | follow |
| `ctrl-l` | clear stream view, `esc` restores | | | |
| `?` | help | | | |

Press `?` in the app for the full list.

`enter` on a frame opens the inspector, which shows the frame's own bytes and,
above them, everything mcpsnoop concluded about it, the warning, an observation,
a deprecation or a cache refetch, each in full. The stream's detail column cuts
a long one short, so the inspector is where to read it.

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
| `otlp` | OTLP JSON with a span per correlated call, carrying the OpenTelemetry semantic conventions for MCP. See [OPENTELEMETRY.md](OPENTELEMETRY.md) |

MCP is not HTTP, so a HAR entry's URL, status code, and timings are a deliberate
mapping of each call rather than a wire transcript.

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

## Replay a call captured over HTTP

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
bare captured body does not. That is `MCP-Protocol-Version`, an `Accept` listing both
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
one family. A log is a file people hand around, and letting it name any header
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
are both read, and a failure is named rather than numbered:

- a 401 reports the scheme the server demanded
- a `-32020` reports what it objected to
- a non-JSON-RPC 400 or 404 says the address is not a Streamable HTTP endpoint
  of this revision

## See what a server asked your user for

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

The ledger never carries a submitted value. What a user typed stays in the
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
records what happened. It does not judge it.

## Tell a broken server from a tool that says no

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

## See what the server costs you in context

Tool definitions enter the model's context on every conversation, and tool
results on every call. The tool summary (`s`) measures both from the session
you actually captured.

The `definitions` line is the fixed cost, what this server's `tools/list`
weighs before a single call is made. The `DEF` column breaks that down per
tool and `RESULT` is what each tool's answers have cost so far. The table stays
sorted by errors and latency, so scan `DEF` to find the expensive definitions.
The export lists them heaviest first. A line under the table names the single
heaviest result, which a total hides.

Definition figures are the JSON with insignificant whitespace removed, so a
server that pretty-prints its `tools/list` is not counted as more expensive than
one that does not, and the same server measures the same across captures.
`RESULT` is the bytes as they arrived, since a result is a one-off payload rather than
a contract worth normalising.

```bash
mcpsnoop export -T json | jq '.summary.definitions'
```

The export carries the same figures, per tool and split into description and
schema bytes, so a fat description and a fat schema stay separable and either
can be tracked across captures. `mcpsnoop diff` tells you a description or
schema changed between two sessions. The export is where the size of that
change lives.

**These are bytes, not tokens.** A token count depends on the model, so
measuring one would mean shipping a tokeniser and picking whose. Bytes are
exact and you can apply your own ratio. An unfinished `tools/list` reports what
it saw as a floor and says so, rather than passing a partial sum off as the
total.

## Find the tool that fails one run in four

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
act on and retry. A JSON-RPC error is the request or the server being wrong.
`SESS` is the count of sessions that saw a failure over the sessions that called
the tool, which is the "one run in ten" question a rate over calls cannot
answer.

Rows key on the server and the label together. The server is the recorded
command and working directory for stdio and the endpoint for HTTP, the same
identity `inventory` uses. Either half alone pools something it should not. The
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
`stats` reports and does not gate. It writes nothing, touches no baseline, opens
no socket, and exits 0 whenever the walk succeeded.

## See what each client actually sends

The tables that say which client supports what are written from documentation.
`clients` is read from traffic, so a client that documents elicitation and never
answers one, or that sends trace context nobody wrote down, shows up as what it
did.

```bash
mcpsnoop clients
mcpsnoop clients --since 7d --label my-server
mcpsnoop clients --format markdown claude.jsonl codex.jsonl > CLIENTS.md
```

```
read 2 named logs, 2 sessions with client traffic

oldie 0.9
  sessions                 1 (stdio)
  revision                 2025-11-25
  opens with               initialize
  names itself             in initialize
  declares                 elicitation, roots, sampling
  lists                    tools/list ×1
  re-fetches inside ttlMs  none
  trace context            0 of 4 requests
  tool calls               1 (1 no hints)
  progress tokens          0 of 1 tool calls
  multi round-trip         not exercised
  elicitation answers      decline 1
  server requests          elicitation/create answered ×1, roots/list error -32601 ×1
  cancellations            none
  calls in flight          others 1 at a time
  repeated calls           none
  deprecated in use        logging ×1
  protocol warnings        none

probe 1.4.0
  sessions                 1 (stdio)
  revision                 2026-07-28
  opens with               server/discover, then stateless
  names itself             on 5 of 5 requests
  declares                 elicitation (form, url)
  lists                    tools/list ×2
  re-fetches inside ttlMs  1
  trace context            4 of 5 requests
  tool calls               2 (2 no hints)
  progress tokens          1 of 2 tool calls
  multi round-trip         1 input_required, 1 retried
  elicitation answers      accept 1
  server requests          none
  cancellations            none
  calls in flight          others 1 at a time
  repeated calls           none
  deprecated in use        none
  protocol warnings        none
```

Sessions are grouped by the `clientInfo` each client sent, name and version, so
two versions of one client get two blocks. Under 2025-11-25 that name arrives in
`initialize`, and under 2026-07-28 a client SHOULD put it in the `_meta` of every
request, which is what `names itself` counts. A client that never named itself is
reported as unidentified rather than guessed at.

`opens with` tells the two handshakes apart. A 2026-07-28 client sends its
version and capabilities on each request and may probe with `server/discover`
first, and one that probes and then sends `initialize` fell back to an older
server, the way the specification describes for stdio. `multi round-trip` counts
the `input_required` answers the server gave and the retries that continued
them, with any retry whose `requestState` came back changed, missing or invented.
`server requests` is the older channel, where the server asked through a request
of its own and the client answered it or refused.

`trace context` counts requests carrying a W3C `traceparent` in `_meta`, which is
what lets a client's own spans and mcpsnoop's join one trace. `re-fetches inside
ttlMs` counts list and read requests repeated inside the freshness window the
server declared, so a client ignoring the caching hints is visible. `deprecated
in use` names features 2026-07-28 deprecated and the client still exercised.

`tool calls` breaks the calls down by what each called tool declares,
read-only, additive or destructive, or no hints for a tool that declares none or
was never listed. `calls in flight` is the most calls the client had going at
once among the ones the server answered, read-only tools apart from the rest,
because that is the line clients draw. Claude Code 2.1.292 reads `read-only up to
2 at once, others 1 at a time`. `repeated calls` counts the calls a client sent
again before it could know what became of the first attempt, the ones
`check --fail-on duplicate` reports, and how many of them the server answered
twice.

With no arguments it walks the sessions directory the way `stats` does, with the
same `--since`, `--label` and `--limit`. Name logs to read exactly those, which
is how a published comparison is built from captures kept for the purpose, and a
named log that cannot be read is an error rather than a skip. `--format
markdown` prints one table with a column per client, and every value in it came
off the wire, so a pipe or an escape sequence in a client's name is quoted rather
than rendered. `clients` reports and does not gate, and exits 0 whenever the read
succeeded.

## See which servers have actually run here

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
and one not, are two rows. mcpsnoop cannot know what the placeholder replaced,
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
disk. mcpsnoop reports what ran on this machine through it. It scans no network,
reads no client config it was not pointed at, and judges nothing.

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

## Config file

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
