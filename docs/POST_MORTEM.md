# Review past sessions from logs

mcpsnoop keeps a per-session JSONL trace while it proxies MCP traffic. That
means you can open the TUI after the client/server session already happened and
review the captured traffic from disk.

## Where session logs live

By default, session logs are stored under:

```text
~/.local/state/mcpsnoop/sessions/
```

Each session is one `.jsonl` file named after the session id:

```text
~/.local/state/mcpsnoop/sessions/<session-id>.jsonl
```

The base directory can be overridden with:

```bash
MCPSNOOP_HOME=/path/to/mcpsnoop-state
```

If `XDG_STATE_HOME` is set, mcpsnoop uses:

```text
$XDG_STATE_HOME/mcpsnoop/sessions/
```

## Open a past session

Run the TUI with no arguments:

```bash
mcpsnoop
```

On startup, the hub reads existing session logs from disk before accepting new
live shim connections. Past sessions appear in the sessions table alongside any
currently running sessions.

From the sessions table:

- Press `/` to filter by session name.
- Press `enter` to open the selected session stream.
- Press `:` and type part of a session name to jump to it.
- Press `y` on a selected session to copy its log path.
- Press `ctrl-d` only when you intentionally want to remove the selected session
  and its on-disk log.

## Review the captured stream

Inside a session stream:

- Press `/` to filter frames with tokens such as `tool:`, `method:`, `id:`,
  `kind:`, `dir:`, `status:`, or plain text.
- Press `enter` on a frame to inspect the full JSON.
- Press `/` while the inspector is open to search inside that JSON.
- Press `c` to inspect the negotiated capabilities for the session.
- Press `r` on a captured tool call to replay it against a fresh server process.
- Press `y` on a frame to copy its JSON.
- Press `esc` to return to the sessions table.

## Capture to a known file

For debugging reports or reproducible examples, write a session to a known path:

```bash
mcpsnoop --trace-file ./mcpsnoop-session.jsonl -- node build/index.js
```

You can keep that file as an attachment or move it into a test fixture.
Note that `--trace-file` writes the log only to that path, not to the default
sessions directory. A running TUI still shows the session live through the
socket, but because the log is not in the sessions directory, it will not
appear in the TUI backfill on a later start. For a session that shows up
automatically later, leave `--trace-file` off and use the default location.

## What to include in a bug report

When reporting a past-session problem, include:

- the wrapped server command;
- the session log path, or the smallest relevant JSON frame copied with `y`;
- the filter you used, if one made the problem visible;
- whether the session came from stdio mode or `mcpsnoop http`.

Avoid pasting secrets or private tool payloads directly into a public issue.
Trim or redact frames before sharing them.
