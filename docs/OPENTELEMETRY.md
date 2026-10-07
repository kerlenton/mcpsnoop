# OpenTelemetry and Prometheus

MCP revision 2026-07-28 deprecates protocol-level Logging. It stays in the
specification for at least twelve months, and the specification tells existing
implementations to move to `stderr` on stdio, or to OpenTelemetry for structured
observability. mcpsnoop already covers both. A stdio server's `stderr` lines show
up in the stream beside its frames, filterable with `kind:stderr`, and every
request becomes an OpenTelemetry span without instrumenting the client or the
server.

## Export spans from a capture

```bash
mcpsnoop export -T otlp -o trace.json   # import into an OTLP-compatible tracing backend
```

For OTLP, a request's `_meta.traceparent` supplies that call's trace and parent
span IDs, and `_meta.tracestate` rides along on the span. When the traceparent is
absent or invalid, mcpsnoop keeps the session-derived trace and carries no state.
mcpsnoop observes rather than participates, so it adds no vendor entry of its own
and passes the caller's state through unchanged.

## What the spans carry

The spans follow the
[OpenTelemetry semantic conventions for MCP](https://github.com/open-telemetry/semantic-conventions-genai/tree/main/model/mcp),
so a capture lands in a dashboard built for MCP rather than one built for
mcpsnoop. Nothing has to be instrumented for this. The server is not touched, the
client is not touched, and the spans describe what actually crossed the wire.

| Attribute | When |
|---|---|
| `mcp.method.name` | always, the one the convention requires |
| `mcp.protocol.version` | when a handshake was captured |
| `mcp.resource.uri` | on `resources/read`, `resources/subscribe`, `resources/unsubscribe` and `notifications/resources/updated` |
| `gen_ai.tool.name` | when the call names a tool |
| `gen_ai.operation.name` | `execute_tool`, on the span that stands for a tool execution and nothing else |
| `gen_ai.prompt.name` | on `prompts/get` |
| `jsonrpc.request.id` | when the request carried a non-null id |
| `rpc.response.status_code` | when the response carried a JSON-RPC error code |
| `error.type` | on the span that failed, the error code, or `tool_error` for a result with `isError` |
| `network.transport` | `pipe` on stdio, `tcp` on HTTP |
| `server.address`, `server.port` | on an HTTP capture |

Span names follow the convention's `{method} {target}`, so `tools/call search`
rather than every tool collapsing into one `tools/call`. The resource URI stays
out of the name on purpose, because a URI per span is the high-cardinality case
the convention tells instrumentation to avoid.

There is no `mcp.session.id`. It identifies a connection-scoped MCP session, and
2026-07-28 removed that concept, so a current capture has nothing to put there.
mcpsnoop's own capture id is a different identifier and travels as
`mcpsnoop.session.id` instead, rather than wearing the convention's name.

The payload argument and result attributes the convention marks opt-in are not
emitted. They carry the bytes themselves, which an export is not the place for by
default, and redaction is the flag that decides what leaves the machine.

## Operations that took more than one request

Under multi round-trip requests a server answers a tool call with
`input_required`, a person answers it, and the client retries under a new id.
The specification makes that retry an independent request, and the convention
defines an `mcp.client` span as one request and the wait for its answer. So an
operation like that is a span per request, each with its own
`jsonrpc.request.id` and a duration that is the server's time for that request
alone.

A tool call among them also gets a GenAI `execute_tool {tool}` span above its
hops, kind internal, covering the whole operation from the first request to the
final answer. A booking where the server worked 1.2 seconds and a person took 37
reads like this.

```text
execute_tool book_flight   38.2s   round_trips 3, server 1200ms, client 37000ms
├─ tools/call book_flight   0.4s   id 1, asked elicitation/create
├─ tools/call book_flight   0.3s   id 2, waited 12000ms, asked elicitation/create
└─ tools/call book_flight   0.5s   id 3, waited 25000ms
```

A latency panel built on the `tools/call` spans sees the server's time and not
the person's, and one built on `execute_tool` sees the whole thing.
`gen_ai.operation.name` sits on the parent only and `mcp.method.name` on the hops
only, so neither kind of panel counts one operation twice. Only the last hop and
the parent carry the outcome, since the earlier hops were answered with
`input_required` and succeeded as requests.

| Attribute | On | Meaning |
|---|---|---|
| `mcpsnoop.call.round_trips` | the parent | how many requests the operation took |
| `mcpsnoop.call.server_time_ms` | the parent | the server's share of the whole |
| `mcpsnoop.call.client_turnaround_ms` | the parent | the rest, mostly a person deciding |
| `mcpsnoop.call.id` | every hop | the id the operation opened with, which groups the hops |
| `mcpsnoop.hop.index` | every hop | its place in the operation, from zero |
| `mcpsnoop.hop.asked` | a hop that asked for input | what the answer asked the client for, as an array |
| `mcpsnoop.hop.client_turnaround_ms` | every hop after the first | the wait before it |

`prompts/get` and `resources/read` can take more than one request too. The
conventions define no span for one prompt fetch or one resource read, so mcpsnoop
does not invent one. Those keep their hops grouped by `mcpsnoop.call.id`, and the
last hop carries the operation's totals.

A live store can release old frames to stay inside its memory budget, and an
operation that outlives that can lose its first hop. It then goes out as one span
with `mcpsnoop.call.hops_complete` set to false, still carrying the exact server
and client split, which the store accumulates as frames arrive, rather than as a
partial breakdown passed off as the whole.

The same spans go to a live collector with `--otlp-endpoint`, with the same ids,
because the export and the live push share one code path.

## Stream completed calls to an OTLP collector

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

An operation goes out when it finishes. One that takes several requests goes out
whole once the last answer arrives, never a request at a time, and nothing is
sent for one still waiting on a person.

A cancelled call has usually finished without an answer, since a server receiving
the cancellation SHOULD not send one, and a client cancels exactly when a request
has hung past its timeout. It goes out two seconds after the cancellation, which
is time for an answer that was already in flight to arrive and go out with it.
The cancellation section has both sides handle that race. Closing the proxy sends
anything still waiting at once.

`--otlp-endpoint` is refused together with `--no-trace`, from the flag or from
`.mcpsnoop.toml`. `--no-trace` turns every observer off, the OTLP sink with them,
so the two together would run and send nothing.

Delivery is best-effort and never blocks proxied MCP traffic. If the collector
is unavailable, mcpsnoop retries in the background and drops new trace frames
when its bounded queue is full. The normal JSONL session log remains the durable
record.

## Prometheus metrics

Start a headless hub with an explicit metrics address to expose live tool-call
metrics (use bare `mcpsnoop` when you want the interactive TUI):

```bash
mcpsnoop --metrics-listen 127.0.0.1:9464
curl http://127.0.0.1:9464/metrics
```

The listener is separate from the MCP proxy listener and is disabled unless
`--metrics-listen` is provided. Startup history replay is not counted as new live
traffic.

Every series has `server`, `server_id` and `tool` labels. `server_id` is a stable
short fingerprint of the recorded server identity, so two servers with the same
label remain separate without exposing commands, paths, endpoints or session IDs.
Error counters also have `error_type`, either `tool` for `result.isError` or
`protocol` for a JSON-RPC or other protocol-level failure.

The public metrics are:

| Metric | Meaning |
|---|---|
| `mcpsnoop_tool_calls_total` | live tool-call requests observed |
| `mcpsnoop_tool_errors_total` | live tool errors, split by `error_type` |
| `mcpsnoop_tool_call_duration_seconds` | request-to-response latency histogram |
| `mcpsnoop_transport_errors_total` | live transport failures that carried no JSON-RPC message, by `status` |

The histogram exports the standard `_bucket`, `_sum` and `_count` series with
`0.005`, `0.01`, `0.025`, `0.05`, `0.1`, `0.25`, `0.5`, `1`, `2.5`, `5`, `10`
and `+Inf` second buckets. Pending and superseded calls, and calls cancelled
without a result, add no latency observation.

**The endpoint has no authentication.** Anything that can reach the address gets
the tool names of every server this hub is watching. The `127.0.0.1` above is
the example for a reason. Binding `:9464` publishes those names to the network.

The `tool` label comes off the wire, so it is bounded on the way in. A name over
128 bytes is truncated, and past two thousand distinct series per hub the rest
are counted together under `tool="(over-series-cap)"`. The totals stay right
either way. Without those bounds a peer choosing tool names decides how much
memory the hub uses and how large a scrape is, and one 4 MiB name measured out
at a 71 MB response, which Prometheus drops whole.

`mcpsnoop_tool_errors_total` counts errors that arrive as a JSON-RPC error or as
`result.isError`. A failure that never became a JSON-RPC message, such as a 502
from a gateway or a 401 challenge, goes to `mcpsnoop_transport_errors_total`
with the status instead. The stream and `check` mark the call it answered as
failed, but the metric keeps the two apart, since a gateway's 502 says something
about the gateway rather than about the tool. The family is exported even when
it is empty, so a graph of it on a healthy hub is flat rather than absent.

The endpoint reports what this hub has seen since it started. It is not a store
of record, and a hub restart starts the counters again, which Prometheus reads
as a counter reset.
