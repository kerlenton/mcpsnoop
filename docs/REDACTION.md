# Redaction

## Redacting what you capture

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
[Detect tool definition drift](CI.md#detect-tool-definition-drift).

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

## Scrub a capture you already have

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
result, are left alone by `--redact-key` and `--redact-secrets`, for three reasons.

- A name inside a schema is a type declaration rather than a value.
- The name itself stays in the log either way.
- Scrubbing the subschema under a property called `token` would take the tool's
  own checks with it.

The exemption is that position only, so an argument that happens to be called
`inputSchema` is scrubbed like any other, and it stops at `default`, `const`,
`examples` and `enum`, which hold data rather than structure. Use
`--redact-path` to name something inside a schema, or `--redact-value`, which
matches text wherever it sits except in the two keywords mcpsnoop parses, `type`
and `x-mcp-header`.

What each flag reaches differs, so check the result rather than assuming. All
four scrub JSON-RPC payloads, and `--redact-key`, `--redact-path` and
`--redact-secrets` reach only those. Only `--redact-value` also scrubs stderr,
other non-JSON text, and the inside of a string. An `Mcp-Param-*` header is
scrubbed alongside the body value it mirrors. The other envelope metadata,
server labels, `Mcp-Name`, `Mcp-Method` and the HTTP status, is left as
captured. Redaction is best effort, so use a separate output path and read the
result before sharing it.
