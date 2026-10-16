# The capture format

Every session mcpsnoop records is saved as a capture, a JSON Lines file with one
envelope per line. An envelope is one thing mcpsnoop saw, which is a JSON-RPC
frame from the client or the server, a line the server wrote to stderr, or the
session's own description. `check`, `export`, `open`, `diff`, `mock`, `stats` and
`clients` all read captures, and so can anything else. This page is the contract
for doing that, and [capture.schema.json](capture.schema.json) is the same
contract as a JSON Schema for one envelope.

## What stays stable

- A field keeps its meaning. A release that needs to say something new adds a
  field rather than reusing one.
- Fields are only ever added, so ignore the ones you do not know.
- An absent field was not recorded. Either it does not apply to that envelope or
  the capture is older than the field, and the Since column below says which
  release started writing each one.
- `direction` and `transport` may gain values. Skip an envelope whose value you
  do not know rather than rejecting the whole capture.

There is no version number. Which fields are present says how a capture was
made.

## Where captures come from

`mcpsnoop -- <server>` writes one file per run, named `<session-id>.jsonl`, in
the sessions directory, and [Review past sessions](POST_MORTEM.md) says where
that is. `--trace-file <path>` writes it to a path you choose instead, making
the directory when it is missing. `mcpsnoop http` writes into the sessions
directory and takes `--trace-file` too.

A run appends and never rewrites a line it has written, so a `--trace-file`
reused across runs holds several sessions one after another. `check` checks
every session in every file it is given, and `export` writes the first.

The session id is `<label>-<pid>-<random hex>`. Treat it as an opaque name.

## Reading a capture

Each line is one compact JSON object, UTF-8 with one exception. mcpsnoop writes
a frame the way it arrived, so a peer that sent invalid UTF-8 inside a JSON
string leaves those bytes in the line. A reader that insists on valid UTF-8
loses that frame, so decide what to do with one before you meet it.

Within a session, `seq` starts at 1 and rises by one for each envelope in the
order mcpsnoop read the frames, and a session's lines are written in that order.
A frame takes its place before mcpsnoop passes it on, so whatever either peer
sent in answer to a frame comes after it. A stdio capture written before 0.28.0
numbered a frame once it had been passed on, and a client quick enough could
have its answer come first. A jump in `seq` means envelopes never reached the
file because the writer's queue was full. mcpsnoop counts every jump as missing
frames, which `check --fail-on incomplete` gates on, so count them too rather
than assuming a capture is whole.

`ts` is when mcpsnoop read the frame, in RFC 3339 with the machine's UTC offset
and up to nanosecond precision. It is not when the client or the server sent
the frame, and two envelopes can carry the same `ts`.

## The envelope

The first six fields are on every envelope. The rest appear only when they have
something to say, so a boolean is either `true` or absent. The table lists them
in the order they appear on a line.

| Field | Type | Since | Meaning |
|---|---|---|---|
| `session_id` | string | 0.1.0 | The session the envelope belongs to |
| `server_label` | string | 0.1.0 | The `--label` given to mcpsnoop, or else a name taken from the server command on stdio and the target's host on HTTP |
| `seq` | integer | 0.1.0 | The envelope's position in its session, from 1 |
| `ts` | string | 0.1.0 | When mcpsnoop read it |
| `direction` | string | 0.1.0 | `c2s`, `s2c`, `stderr` or `meta`, described below |
| `transport` | string | 0.1.0 | `stdio` or `http` |
| `raw` | any JSON | 0.1.0 | The frame, when it arrived as valid JSON |
| `text` | string | 0.1.0 | A stderr line, a protocol line that was not valid JSON, or the kept part of a frame cut at 16 MiB |
| `mcp_method` | string | 0.11.0 | The `Mcp-Method` header of an HTTP request |
| `mcp_name` | string | 0.11.0 | The `Mcp-Name` header of an HTTP request |
| `mcp_protocol_version` | string | 0.11.0 | The `MCP-Protocol-Version` header of an HTTP request |
| `mcp_param_headers` | array | 0.17.0 | The `Mcp-Param-*` headers of an HTTP request, described below |
| `batch` | boolean | 0.11.0 | The frame was one element of a JSON-RPC batch on HTTP |
| `conn_id` | string | 0.18.0 | The client end of the TCP connection an HTTP frame travelled on, as `host:port` |
| `exchange` | integer | 0.26.0 | Numbers the HTTP request a frame belongs to, from 1. The request's frame and every frame of its response share it |
| `undelivered` | boolean | 0.26.0 | On mcpsnoop's own 502, the target refused the connection, so the request never reached it |
| `abandoned` | boolean | 0.28.0 | On a `c2s` envelope with no message, the client left the exchange before the answer to its request came back |
| `truncated` | boolean | 0.12.0 | This copy of the frame was cut at 16 MiB. The whole frame still reached the other side |
| `redacted` | boolean | 0.17.0 | mcpsnoop's own redaction rewrote this envelope |
| `status` | integer | 0.15.0 | The HTTP status of the response a server frame arrived on |
| `auth_challenge` | string | 0.15.0 | That response's `WWW-Authenticate`, every challenge joined with `, ` |
| `transport_headers` | object | 0.19.0 | The HTTP headers the transport specification makes rules about, described below |
| `trace_context` | object | 0.28.0 | The W3C trace context headers of an HTTP request, described below |

Each entry of `mcp_param_headers` is an object.

| Field | Type | Since | Meaning |
|---|---|---|---|
| `name` | string | 0.17.0 | The header's name in Go's canonical spelling, such as `Mcp-Param-Region`, whatever the client sent, so compare it case-insensitively |
| `value` | string | 0.17.0 | The header's value |
| `redacted` | boolean | 0.18.0 | mcpsnoop's own redaction replaced the value |

`transport_headers` is an object too. A request carries what the client sent,
and a server frame carries the response's `content_type`. An empty object is a
response that had no `Content-Type`.

| Field | Type | Since | Meaning |
|---|---|---|---|
| `accept` | string | 0.19.0 | The request's `Accept` |
| `content_type` | string | 0.19.0 | The frame's own `Content-Type`, the request's on a client frame and the response's on a server frame |
| `origin` | string | 0.19.0 | The request's `Origin` |

`trace_context` is the trace context an HTTP request carried in its headers,
which is where OpenTelemetry's HTTP instrumentation puts it. It rides every
element of a batch, and it is absent from a request that sent neither header.
`baggage` is left out on purpose, since it is data a client forwards about its
own work, often its user, and a span needs none of it.

| Field | Type | Since | Meaning |
|---|---|---|---|
| `traceparent` | string | 0.28.0 | The request's `traceparent`, as sent and not validated |
| `tracestate` | string | 0.28.0 | The request's `tracestate`, several lines joined with commas |

## Directions

`c2s` is a frame from the client to the server, and `s2c` one from the server to
the client. On HTTP an `s2c` envelope can also stand for a response that carried
no message at all, which [On HTTP](#on-http) describes.

`stderr` is one line the server wrote to its stderr, in `text`. It only exists on
stdio, and an empty line has no `text`.

`meta` is the session's own description and always the first envelope, with
`seq` 1. On stdio it is written when the server starts. On HTTP it is written
together with the first frame, so a proxy nobody called leaves an empty file. Its
`raw` is an object.

| Field | Type | Since | Meaning |
|---|---|---|---|
| `command` | array of strings, or null | 0.1.0 | The server command on stdio, and `null` on HTTP |
| `cwd` | string | 0.1.0 | The directory the server ran in, on stdio |
| `target` | string | 0.20.0 | The HTTP endpoint with the userinfo, every query value and the fragment removed. It names the server and is not meant to be dialled |

## raw and text

A frame that arrived as valid JSON is in `raw` as a JSON value, not as a string.
mcpsnoop removes its insignificant whitespace and otherwise writes it as it
arrived. Member order, repeated member names, the spelling of numbers and string
escapes are all kept, and `<`, `>` and `&` are not escaped. U+2028 and U+2029
inside strings may be written as `\u2028` and `\u2029`, which decodes to the
same string. `raw` holds whatever JSON value arrived, which is usually an
object and on stdio can be a batch array.

A line on the protocol channel that is not valid JSON goes to `text` with the
same direction. That is how a server logging to stdout shows up, and `check`
reports it as `invalid`.

A frame over 16 MiB reaches the other side whole, but mcpsnoop keeps only its
first 16 MiB. A cut frame is not valid JSON, so the kept part goes to `text`, and
`truncated` says why.

## On stdio

Each line on the server's stdin and stdout is one envelope, and so is each line
on its stderr. A JSON-RPC batch is one line and so one envelope, whose `raw` is
the array.

## On HTTP

Only the body of a POST is recorded on the client side. A GET that opens a
stream and a DELETE that ends a session leave no `c2s` envelope, though what the
server sends back on them is recorded under that exchange.

A JSON response gives one `s2c` envelope per message, and a `text/event-stream`
response gives one per event, taken from its `data` lines.

A batch in either direction gives one envelope per element, each with `batch`
set. `mcp_method`, `mcp_name` and `mcp_param_headers` describe a single
operation, so they ride the first element only, while `mcp_protocol_version`,
`conn_id`, `exchange`, `status` and `auth_challenge` ride every element. Batch
elements carry no `transport_headers`.

A response with no message gives one `s2c` envelope with its `status` and
neither `raw` nor `text`. The 202 that acknowledges a notification looks like
that, and so does a bare 401 or 502.

When the target cannot be reached, mcpsnoop answers the client with a 502 of its
own and records it the same way, adding `undelivered` when the connection was
never made. A timeout is not undelivered, because the target may have received
the request before it stopped answering.

When the client leaves an exchange before the answer to the request it carried
comes back, by closing its connection or the event stream the answer was coming
on, mcpsnoop records one `c2s` envelope with that `exchange`, `abandoned` set,
and neither `raw` nor `text`. On 2026-07-28 that is how a client cancels a
request over HTTP, and the server must treat it as a cancellation. On earlier
revisions it is a disconnect, which the server should not.

mcpsnoop asks the target for an uncompressed response. One that arrives
compressed anyway is passed to the client and not recorded.

An HTTP capture never holds `Authorization`, cookies, `Mcp-Session-Id`,
`Last-Event-ID`, `Host`, or any header the tables above do not name.

## Redaction

Redaction is off unless you ask for it. With `--redact-secrets`, `--redact-key`,
`--redact-path` or `--redact-value`, mcpsnoop replaces what they match with the
string `[REDACTED]` before the envelope is written, and marks the envelope
`redacted`. An `Mcp-Param-*` value or an `Mcp-Name` that mirrors a scrubbed body
value is scrubbed with it. The marker is there so a reader can tell mcpsnoop's
placeholder from a peer that really sent those bytes. [Redaction](REDACTION.md)
has the rest.

## What a capture does not hold

- When the server process started or exited, or how it exited.
- When an HTTP connection opened or closed.
- When the client or the server wrote a frame, as opposed to when mcpsnoop saw
  it.

## Examples

A stdio session. The client initializes, lists the tools and calls one, and the
server writes a line to stderr before it answers.

```jsonl
{"session_id":"weather-97982-dac9f34a7100","server_label":"weather","seq":1,"ts":"2026-10-14T21:25:13.785116+03:00","direction":"meta","transport":"stdio","raw":{"command":["python3","server.py"],"cwd":"/home/me/weather"}}
{"session_id":"weather-97982-dac9f34a7100","server_label":"weather","seq":2,"ts":"2026-10-14T21:25:13.785269+03:00","direction":"c2s","transport":"stdio","raw":{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"example-client","version":"1.0.0"}}}}
{"session_id":"weather-97982-dac9f34a7100","server_label":"weather","seq":3,"ts":"2026-10-14T21:25:13.785284+03:00","direction":"c2s","transport":"stdio","raw":{"jsonrpc":"2.0","method":"notifications/initialized"}}
{"session_id":"weather-97982-dac9f34a7100","server_label":"weather","seq":4,"ts":"2026-10-14T21:25:13.785293+03:00","direction":"c2s","transport":"stdio","raw":{"jsonrpc":"2.0","id":1,"method":"tools/list"}}
{"session_id":"weather-97982-dac9f34a7100","server_label":"weather","seq":5,"ts":"2026-10-14T21:25:13.785296+03:00","direction":"c2s","transport":"stdio","raw":{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_forecast","arguments":{"city":"Lisbon"}}}}
{"session_id":"weather-97982-dac9f34a7100","server_label":"weather","seq":6,"ts":"2026-10-14T21:25:13.850331+03:00","direction":"stderr","transport":"stdio","text":"weather server starting"}
{"session_id":"weather-97982-dac9f34a7100","server_label":"weather","seq":7,"ts":"2026-10-14T21:25:13.850433+03:00","direction":"s2c","transport":"stdio","raw":{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"weather","version":"1.0.0"}}}}
{"session_id":"weather-97982-dac9f34a7100","server_label":"weather","seq":8,"ts":"2026-10-14T21:25:13.850477+03:00","direction":"s2c","transport":"stdio","raw":{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"get_forecast","description":"Forecast for a city","inputSchema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]},"annotations":{"readOnlyHint":true,"openWorldHint":true}}]}}}
{"session_id":"weather-97982-dac9f34a7100","server_label":"weather","seq":9,"ts":"2026-10-14T21:25:13.850494+03:00","direction":"s2c","transport":"stdio","raw":{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"Sunny, 21C"}]}}}
```

The same server over HTTP at 2025-11-25. Each request came on its own
connection, and the 202 that acknowledges the notification has no body, so its
envelope has a status and no message.

```jsonl
{"session_id":"weather-98275-8200bb024c85","server_label":"weather","seq":1,"ts":"2026-10-14T21:27:16.645031+03:00","direction":"meta","transport":"http","raw":{"command":null,"target":"http://127.0.0.1:47811/mcp"}}
{"session_id":"weather-98275-8200bb024c85","server_label":"weather","seq":2,"ts":"2026-10-14T21:27:16.645031+03:00","direction":"c2s","transport":"http","raw":{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"example-client","version":"1.0.0"}}},"conn_id":"127.0.0.1:59145","exchange":1,"transport_headers":{"accept":"application/json, text/event-stream","content_type":"application/json"}}
{"session_id":"weather-98275-8200bb024c85","server_label":"weather","seq":3,"ts":"2026-10-14T21:27:16.645887+03:00","direction":"s2c","transport":"http","raw":{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"weather","version":"1.0.0"}}},"conn_id":"127.0.0.1:59145","exchange":1,"status":200,"transport_headers":{"content_type":"application/json"}}
{"session_id":"weather-98275-8200bb024c85","server_label":"weather","seq":4,"ts":"2026-10-14T21:27:16.656554+03:00","direction":"c2s","transport":"http","raw":{"jsonrpc":"2.0","method":"notifications/initialized"},"mcp_protocol_version":"2025-11-25","conn_id":"127.0.0.1:59147","exchange":2,"transport_headers":{"accept":"application/json, text/event-stream","content_type":"application/json"}}
{"session_id":"weather-98275-8200bb024c85","server_label":"weather","seq":5,"ts":"2026-10-14T21:27:16.656805+03:00","direction":"s2c","transport":"http","conn_id":"127.0.0.1:59147","exchange":2,"status":202,"transport_headers":{}}
{"session_id":"weather-98275-8200bb024c85","server_label":"weather","seq":6,"ts":"2026-10-14T21:27:16.665248+03:00","direction":"c2s","transport":"http","raw":{"jsonrpc":"2.0","id":1,"method":"tools/list"},"mcp_protocol_version":"2025-11-25","conn_id":"127.0.0.1:59149","exchange":3,"transport_headers":{"accept":"application/json, text/event-stream","content_type":"application/json"}}
{"session_id":"weather-98275-8200bb024c85","server_label":"weather","seq":7,"ts":"2026-10-14T21:27:16.66547+03:00","direction":"s2c","transport":"http","raw":{"jsonrpc":"2.0","id":1,"result":{"tools":[]}},"conn_id":"127.0.0.1:59149","exchange":3,"status":200,"transport_headers":{"content_type":"application/json"}}
```
