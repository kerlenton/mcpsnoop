# OpenTelemetry and Prometheus

MCP revision 2026-07-28 deprecates protocol-level Logging. It stays in the
specification for at least twelve months, and the specification tells existing
implementations to move to `stderr` on stdio, or to OpenTelemetry for structured
observability. mcpsnoop already covers both. A stdio server's `stderr` lines show
up in the stream beside its frames, filterable with `kind:stderr`, and every call
becomes an OpenTelemetry span without instrumenting the client or the server.

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
| `gen_ai.operation.name` | `execute_tool`, on a tool call and nothing else |
| `gen_ai.prompt.name` | on `prompts/get` |
| `jsonrpc.request.id` | when the request carried a non-null id |
| `rpc.response.status_code` | when the response carried a JSON-RPC error code |
| `error.type` | when the call failed, the error code, or `tool_error` for a result with `isError` |
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

The same spans go to a live collector with `--otlp-endpoint`, because the export
and the live push share one code path.

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
from a gateway or a 401 challenge, cannot be attributed to a tool, because
nothing in the response says which request it answered. Those go to
`mcpsnoop_transport_errors_total` with the status, and the family is exported
even when it is empty, so a graph of it on a healthy hub is flat rather than
absent.

The endpoint reports what this hub has seen since it started. It is not a store
of record, and a hub restart starts the counters again, which Prometheus reads
as a counter reset.
