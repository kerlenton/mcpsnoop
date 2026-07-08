# Review past sessions from logs

mcpsnoop keeps a per-session JSONL trace while it proxies MCP traffic. So you can
open the TUI after a client and server session already happened and review the
captured traffic straight from disk.

## Where session logs live

By default each session is one `.jsonl` file under your state directory, named
after the session id.

```text
~/.local/state/mcpsnoop/sessions/<session-id>.jsonl
```

Set `MCPSNOOP_HOME` to move that base directory somewhere else, or set
`XDG_STATE_HOME` to keep sessions under `$XDG_STATE_HOME/mcpsnoop/sessions`.
`MCPSNOOP_HOME` wins when both are set.

## Open a past session

Run the TUI with no arguments.

```bash
mcpsnoop
```

On startup the hub reads existing session logs from disk before it accepts live
shim connections, so past sessions appear in the sessions table next to any
running ones. From there a few keys do the work.

- Press `/` to filter by session name.
- Press `enter` to open the selected session stream.
- Press `:` and type part of a session name to jump to it.
- Press `y` to copy the selected session log path.
- Press `ctrl-d` to remove the selected session and its on-disk log, only when
  you mean to.

To open one specific log that is not in the sessions directory, for example a
`--trace-file` capture or a log a teammate sent you, pass its path to `open`.

```bash
mcpsnoop open ./session.jsonl
```

Use `-` to read from stdin, handy for streaming a remote log over SSH without
copying it down first.

```bash
ssh remote-user@remote-host 'cat ~/.local/state/mcpsnoop/sessions/session.jsonl' | mcpsnoop open -
```

## Review the captured stream

Inside a session stream, a few more keys apply.

- Press `/` to filter frames with tokens such as `tool:`, `method:`, `id:`,
  `kind:`, `dir:`, `status:`, or plain text.
- Press `enter` on a frame to inspect the full JSON.
- Press `/` while the inspector is open to search inside that JSON.
- Press `c` to inspect the negotiated capabilities for the session.
- Press `r` on a captured tool call to replay it against a fresh server process.
- Press `y` on a frame to copy its JSON.
- Press `esc` to return to the sessions table.

## Capture to a known file

For a bug report or a reproducible example, write a session straight to a path
you choose.

```bash
mcpsnoop --trace-file ./mcpsnoop-session.jsonl -- node build/index.js
```

Keep that file as an attachment or move it into a test fixture. `--trace-file`
writes the log only to that path, not to the default sessions directory. A
running TUI still shows the session live through the socket, but since the log
lives outside the sessions directory it will not show up in the backfill on a
later start. For a session that reappears automatically, leave `--trace-file`
off and use the default location.

To keep common secret fields out of saved traces, pass `--redact-secrets` when
you wrap the server. Add `--redact-key` for project-specific fields.

```bash
mcpsnoop --redact-secrets --redact-key tenant_api_key -- node build/index.js
```

## What to include in a bug report

A useful report about a past session covers a few things.

- The wrapped server command.
- The session log path, or the smallest relevant JSON frame copied with `y`.
- The filter that made the problem visible, if you used one.
- Whether the session ran in stdio mode or through `mcpsnoop http`.

Keep secrets and private tool payloads out of public issues. Trim or redact
frames before you share them.
