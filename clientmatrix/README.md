# Client matrix

What MCP clients actually send, measured on the wire rather than taken from
their documentation.

## How it works

`refserver` is a small MCP server built on the official Go SDK, which speaks
every revision from 2024-11-05 to 2026-07-28, so old and new clients connect to
the same server and are measured against the protocol's own implementation.
Each part of it exists to make a client show one behaviour.

| Part | What it draws out |
|---|---|
| `echo` tool | the baseline call, and the `_meta` a client sends on it |
| `confirm_action` tool | an elicitation, through multi round-trip requests under 2026-07-28 and through `elicitation/create` under older revisions |
| `slow_task` tool | progress tokens, and cancellation when a client gives up |
| `echo_read` and `slow_read` tools | the same two marked `readOnlyHint`, so whether calls asked for at once are sent at once shows |
| `add_note`, `count_notes` and `clear_notes` tools | one declared effect each, additive, read-only and destructive, every hint spelled out. `add_note` records its note before it answers, so a client that gives up on it and sends it again adds the note twice |
| `list_roots` tool | what a client reports as its roots, through multi round-trip requests or `roots/list` |
| `unlock_tool` and the `bonus_tool` it adds | whether a tool added mid-conversation is picked up, through `subscriptions/listen` under 2026-07-28 |
| `greeting` prompt and `ref://about` resource | whether a client lists and reads them |
| `ttlMs` of one minute on every list | whether a client re-lists inside the window |

Each client talks to it through mcpsnoop, which writes the capture, and
`mcpsnoop clients` folds the captures into the comparison. A client driven by a
model gets `scenario.md` as its prompt. What the model chooses to call is part
of the measurement, so the table reports what happened rather than what was
asked.

`goclient` is the official Go SDK client running a fixed scenario. It is the
column that needs no account, and it checks the harness end to end. It also
gives up on an `add_note` call after 300ms and sends it again, which is the
retry spec issue
[#3394](https://github.com/modelcontextprotocol/modelcontextprotocol/issues/3394)
reproduces, so `mcpsnoop check --fail-on duplicate` has a real capture to
report on.

## Run it

```bash
clientmatrix/run.sh go-sdk
clientmatrix/run.sh go-sdk-2025-11-25
clientmatrix/run.sh claude-code
mcpsnoop clients --format markdown clientmatrix/captures/*.jsonl
```

`claude-code` runs Claude Code headless with your own account, so it spends
your usage. Captures land in `clientmatrix/captures`, which git ignores.

## Add a client

Point the client's MCP configuration at
`mcpsnoop --label ref --trace-file <capture> -- <refserver>` and give it
`scenario.md`. Add a case to `run.sh` once the command is known to work.

## What it does not measure

Only the wire. What a client does with a result, how it renders one, or what
it truncates before the model sees it, never reaches the server and so never
reaches the capture. A headless run also has no person to answer an
elicitation, so how a client answers one there is part of the result, and an
interactive session may answer differently.
