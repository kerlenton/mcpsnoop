# Checking sessions in CI

The GitHub Action, with its inputs and outputs, is described in the
[README](../README.md#the-github-action), which is also its Marketplace listing.
This page is everything underneath it, which is `mcpsnoop check` and the signals
it can fail on.

Gate a recorded agent run on errors, stream corruption, protocol warnings,
routing-header mismatches, calls that never got a response, results that came
back after a cancel, tool-definition drift, tool annotations that loosened after
approval, use of deprecated protocol features, dropped frames that leave the
capture incomplete, schemas that travel badly across clients, or tools that
declare their behaviour badly or not at all.

```bash
mcpsnoop check [--format text|junit|sarif] [--fail-on error,invalid,warn,mismatch,pending,late-result,drift,loosened,deprecated,incomplete,schema,annotations] [session-id|log.jsonl|-]
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
| `loosened` | a tool's annotations claiming less risk than the approved baseline, such as a write that now says it is read-only |
| `deprecated` | a feature the specification has deprecated |
| `incomplete` | frames dropped upstream, which makes every other count a floor rather than a total |
| `schema` | an advertised schema using a construct or a dialect that travels badly across clients |
| `annotations` | a tool that declares no behaviour hint, leaves one of the three main hints to its default, or claims to be read-only and destructive at once |

Every signal is counted whether or not it is gating, so a run says what it found
before you decide what should fail on it.

```
session build-agent: errors=1 invalid=0 warnings=0 mismatches=0 pending=0 late_results=0 deprecated=0 missing_frames=0 schema_findings=1 annotation_findings=0
schema findings:
  oneOf: search
check failed: error
```

The dropped-frame count travels with the artifacts too, so a capture that
understates itself says so wherever it is opened:

- `missing_frames` in the JSON export
- `log.comment` in HAR
- the `mcpsnoop.session.missing_frames` resource attribute in OTLP

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

## Assert what must and must not happen

Beyond the signal counts, assert the shape of the run. These compose with each
other and with `--fail-on`, and any failure exits 1, the code that means the
check ran and found something.

| Flag | Fails when |
|---|---|
| `--max-duration <dur>` | one or more completed tool calls exceeded the budget, reporting their count and the worst call |
| `--expect-tool <name>` | the named tool was never called (repeatable) |
| `--forbid-tool <name>` | the named tool was called (repeatable) |

```bash
# a contract for the run: search must run, delete must not, nothing over 2s
mcpsnoop check --expect-tool search --forbid-tool delete --max-duration 2s run.jsonl
```

## Tell the server's latency from the user's

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

## Report it where CI already looks

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
log was read from.

- A path inside the working directory becomes a relative one, which code
  scanning resolves against the repository root.
- A path elsewhere on disk, or a session id resolved out of the state
  directory, becomes an absolute `file://` URI.
- Reading from stdin gives a result no location at all, since there is no file
  to point at.

The alert renders with its surrounding lines only when that path is a file in the
analysed commit, so a capture the workflow generated into `artifacts/` opens an
alert carrying the message, the rule and the line number but no source view.
Committing a capture you want rendered in full is the only way to get one.

Code scanning rejects a file whose run holds more than 25,000 results and
displays only the top 5,000 of what it accepts, so the report is capped at 5,000:
the findings the gate failed on first, then a `mcpsnoop/report-truncated` result
saying how many were left out. The text and junit formats stay complete.

## Or wire it up yourself

The action is four steps and no magic. Doing it by hand takes the same care it
takes. The upload has to run on the runs that have a report, which are the ones
that exited 0 or 1 and not the ones that exited 2, and the step that fails the
job has to come after it, or the findings never reach the tab they exist to
reach.

```yaml
permissions:
  # required for all workflows
  security-events: write
  # only required for workflows in private repositories
  actions: read
  contents: read

steps:
- name: Check captured MCP session
  id: check
  run: |
    code=0
    mcpsnoop check --format sarif artifacts/session.jsonl > mcpsnoop.sarif || code=$?
    echo "exit-code=$code" >> "$GITHUB_OUTPUT"
    # 2 means the check never happened, so there is no report to publish and
    # nothing was verified. Stop here rather than uploading an empty file.
    [ "$code" -le 1 ] || exit 1
- name: Upload mcpsnoop SARIF report
  if: ${{ !cancelled() }}
  uses: github/codeql-action/upload-sarif@v4
  with:
    sarif_file: mcpsnoop.sarif
    category: mcpsnoop
- name: Fail on findings
  # Separate, and after the upload, so the findings reach the Security tab on
  # exactly the runs that have some.
  if: ${{ !cancelled() && steps.check.outputs.exit-code == '1' }}
  run: exit 1
```

## Comparing sessions

Compare two saved sessions by id or JSONL path.

```bash
mcpsnoop diff before-session after-session
mcpsnoop diff old.jsonl new.jsonl
```

The report shows tools that were added or removed, description and `inputSchema`
changes, matching tool calls whose status changed, and notable duration shifts. Calls
are matched by tool name and arguments, so reordered calls still compare correctly.
By default, duration changes must differ by at least 100 ms and 2x. Use
`--duration-threshold` and `--duration-ratio` to adjust those cutoffs.

Pass `--exit-code` to gate CI on regressions. It exits non-zero when the after
session:

- drops a tool
- changes a tool description, title, input schema, output schema or annotations
- has a call whose status got worse
- slows down

An icon change does not, since it alters how a tool looks without changing what
it does. Improvements, meaning added tools, fixed calls and speedups, still exit
zero.

## What each signal catches

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
them. `Content-Type` was the sharpest case. The response side already read it to
tell an SSE stream from a JSON body, then threw it away.

An HTTP frame now carries the headers the transport states rules about, and two
of those rules are checkable.

| Rule | Reported as |
|---|---|
| the client MUST send an `Accept` listing both `application/json` and `text/event-stream` | `warn` on the request |
| a server answering a JSON-RPC request MUST return `Content-Type: application/json` or `text/event-stream` | `warn` on the response |

Both sentences read the same in 2025-11-25 and 2026-07-28, so unlike the drift
and extension checks these need no revision gate. `Origin` is recorded too, since
servers MUST validate it and MUST answer `403` when it is invalid, but
mcpsnoop cannot know your allowed origins so it shows the value rather than
judging it.

Wildcards count. A client sending `*/*` has offered both types and is never
reported, and a `charset` parameter on a `Content-Type` is ignored. A log
captured before mcpsnoop recorded these headers stays silent rather than
reporting every frame in it for a header nobody wrote down, and stdio never has
them at all.

`Authorization` is deliberately not captured. Turning a challenge into token
facts is its own problem and putting a bearer token on disk is not the answer to
it. `Mcp-Session-Id` and `Last-Event-ID` are not captured either. The
2026-07-28 revision removed both and tells a server to ignore them, so there is
no rule left to check.

### Detect tool definition drift

The first complete `tools/list` observed for a server label becomes its trusted
baseline. Later sessions compare that baseline field by field:

- the description
- the title
- the input and output schemas
- the annotations and the icons

Tools that were added or removed are compared too, which is a set comparison
rather than a field one.

Annotations matter most, since a tool approved with `readOnlyHint` that later
declares itself destructive is the rug-pull this check exists for, and the spec
tells clients to treat annotations as untrusted. The title and the icons are
tracked because they are what the user sees, and the spec ranks a tool's `title`
above `annotations.title` and its name. The sessions table and tool summary flag
drift without blocking or changing MCP traffic.

Annotations are compared through their spec defaults, so a server that starts
spelling out a hint it was already relying on is not reported. A baseline
recorded before mcpsnoop tracked a field keeps working for the fields it does
record and says which ones it cannot answer for. Re-record with
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
failure. Without `drift` in `--fail-on`, recording one is business as usual and
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

`drift` is opt-in for `check`. The default `error,invalid,warn` gate is unchanged.

### Catch annotations that loosened after approval

Drift says that a tool's annotations changed. It also says which way they moved,
because clients act on the direction.

```
definition drift:
  annotations changed: delete_file
  annotations loosened, delete_file: readOnlyHint false → true
```

`mcpsnoop diff` prints the same lines between two captures, and the tool summary,
opened with `s`, lists them as `loosened` and `tightened` rows.

A tool's annotations loosen when they claim less risk than the baseline trusted.
The tool turned read-only, stopped calling itself destructive, became safe to
retry, narrowed itself to a closed world, or started sending a hint that is not a
boolean. A client that acts on hints trusts such a tool more, and these three
already do.

| Client | What a loosened hint switches off |
|---|---|
| VS Code | the confirmation it shows before any tool not marked `readOnlyHint`, per its [MCP developer guide](https://code.visualstudio.com/api/extension-guides/ai/mcp) |
| Codex | in its default `auto` approval mode, the approval it asks for before a tool its hints call destructive or open-world, per [`requires_mcp_tool_approval`](https://github.com/openai/codex/blob/main/codex-rs/core/src/mcp_tool_call.rs) |
| Claude Code | running the tool alone. Measured with 2.1.292 through the [client matrix](../clientmatrix/README.md), it ran read-only tools side by side and unannotated ones one call at a time, so a write that claims to be read-only can race the calls beside it |

No client here is known to act on `idempotentHint` yet. It is counted because it
is the hint a client would consult before retrying a call whose answer never
arrived, the case spec issue
[#3394](https://github.com/modelcontextprotocol/modelcontextprotocol/issues/3394)
raises, and a tool that only claims to be safe to retry may then run twice.

Only hints a client can act on count. The spec makes `destructiveHint` and
`idempotentHint` meaningful only for a tool that is not read-only, so editing
them on a read-only tool is drift and never loosening. The exception is an
explicit `destructiveHint: true`, which Codex asks about on any tool, so dropping
one from a read-only tool does loosen it. Spelling out a default the tool already
relied on, the way ChatGPT's app review asks servers to, moves nothing.

The other direction is reported as tightened, and that is not a clean bill of
health. Codex remembers an approval by server and tool name, so an approval given
while a tool said it was read-only goes on covering it after it admits to
destroying things. Catching that is what `drift` is for.

`loosened` fails a run on the one direction alone, so a gate can hold every tool
to what it was approved with while a server that starts declaring more risk still
passes. It needs a baseline for the reason drift does. A run gated on it that only
recorded one does not pass, and neither does one whose baseline predates
annotation tracking, since that baseline cannot say whether anything loosened.
Re-record it with `mcpsnoop baseline --accept`.

```bash
mcpsnoop check --fail-on loosened --baseline .mcpsnoop/baselines session.jsonl
```

`loosened` is opt-in, and the default `error,invalid,warn` gate is unchanged.

### Catch a feature neither side negotiated

SEP-2133 moved optional features out of the core protocol and into extensions,
advertised in the `extensions` map of each side's capabilities. Tasks is one of
them, so on 2026-07-28 a `tasks/get`, a `notifications/tasks` or a `tools/call`
answered with a task handle only means anything when the other side said it
speaks Tasks.

When it did not, the spec is explicit. The supporting party MUST either fall
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
schema may declare whatever dialect it likes. `no root` is the exception. The
`Tool` definition requires `inputSchema` and pins its root type to `"object"`, so
a client validating a listing rejects that tool outright and it never becomes
callable, with nothing on the wire to say why. `no root` leads the column for
that reason, and a schema mcpsnoop's own redaction scrubbed is never reported,
since an unreadable schema is not a wrong one.

That split decides what `check` does with them. `no root` is a warning on the
`tools/list` frame, so it fails the default `error,invalid,warn` gate with no
flag at all, which is the point. A server that ships an unusable tool answers
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

### Lint tool annotations

Annotations are how a server tells a client what a tool does, so the client can
decide how careful to be. `readOnlyHint` says the tool changes nothing,
`destructiveHint` that a write may destroy something, `idempotentHint` that
repeating a call does nothing more, and `openWorldHint` that the tool reaches
beyond a closed set of things. A hint left out takes the spec's default, and every
default assumes the worst. A tool with no annotations is taken to write, to
destroy, and to reach the open world.

A server pays for that without seeing it. VS Code confirms every call to such a
tool, Codex asks for approval in its default mode, and Claude Code runs its calls
one at a time where it runs read-only tools side by side. ChatGPT's
[app submission guidelines](https://developers.openai.com/apps-sdk/app-submission-guidelines)
go further and ask for `readOnlyHint`, `destructiveHint` and `openWorldHint` as
explicit booleans on every tool.

| Kind | Means |
|---|---|
| `unannotated` | no behaviour hint at all. A `title` alone counts as none, since it names the tool and says nothing about what it does |
| `implicitHints` | at least one of `readOnlyHint`, `destructiveHint` and `openWorldHint` left to its default |
| `contradictoryHints` | `readOnlyHint` and `destructiveHint` both true, which VS Code runs without asking and Codex stops to ask about |

None of these breaks a rule, so `annotations` is opt-in. A default run counts them
as `annotation_findings` and lists them under `annotation findings:`, and only
fails when you add `annotations` to `--fail-on`. They reach `--format junit` and
`--format sarif`, and `export` carries the per-tool list under
`summary.definitions.per_tool[].annotation_findings`.

```bash
mcpsnoop check --fail-on annotations session.jsonl
```

The spec says clients should never make tool use decisions from the annotations
of a server they do not trust. Declaring them well is still what decides how a
client that trusts the server treats each tool, and `loosened` above is what
watches a trusted server for claiming less over time.

### Catch a tool list no client can read

mcpsnoop reads a tool listing leniently and shows every tool it can. The official
SDK clients do not. They check each value against the type the specification
gives it and reject the whole listing over one they cannot read, so a single bad
value costs the client every tool the server offers, and nothing on the wire says
why.

Measured with one well-formed tool beside one whose `readOnlyHint` was the string
`"true"`.

| Client | What it got |
|---|---|
| TypeScript SDK 1.32.1 | a validation error and no tools |
| Go SDK 1.8.0 | a decode error and no tools |
| Python SDK 2.3.0 | both tools, reading `"true"` as true |
| Claude Code 2.1.292 | no tools from that server, after asking for the list four times |

The Python SDK is lenient where it can be. It reads `"true"`, `"yes"` and `1` as
true and `null` as absent, and still rejects a string it cannot read as a
boolean, or annotations that are not an object. The TypeScript SDK rejects `null`
as well.

So mcpsnoop warns on the `tools/list` response, naming each value and what it
should have been, and a default `check` run fails on it.

```
tool "odd" sends annotations.readOnlyHint as a string instead of a boolean
the result sends nextCursor as null instead of a string
tools[3] sends name as a number instead of a string
```

The values judged are the ones mcpsnoop reads. That is each entry of `tools` and
its `name`, `title` and `description`, its annotations, and the listing's own
`tools` array and `nextCursor`. The input schema has its own rule, `no root`
above. Each field is held to its type from the revision that introduced it, so a
session that negotiated 2024-11-05 is not held to annotations, which arrived in
2025-03-26, or to a tool `title`, which arrived in 2025-06-18. A value mcpsnoop's
own redaction replaced is never reported.

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
reject it afterwards. Retiring is counted rather than silent. The stream footer
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
