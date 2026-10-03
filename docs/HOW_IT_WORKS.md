# How it works

The two roles and the picture are in the [README](../README.md#how-it-works).
This page is what bounds their memory and their disk.

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

The history limit bounds what is loaded. `mcpsnoop prune` bounds what is kept.
It deletes saved session logs older than a cutoff, and never runs on its own.

```bash
mcpsnoop prune --older-than 30d --dry-run   # list what would go, remove nothing
mcpsnoop prune --older-than 30d             # delete after confirming
mcpsnoop prune --older-than 72h --yes       # skip the prompt in a script
```

`--older-than` is required (there is no default that would delete anything) and
accepts a day count like `30d` or a Go duration like `72h`. Tool baselines are
left alone, since a baseline is keyed by server label rather than by session.
