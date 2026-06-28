# Trying mcpsnoop for real

You don't need to write a server. Wrap a **published** one with `mcpsnoop` and
drive it with a **real client** (Claude). The `everything` test server is ideal
because its tools have no built-in equivalent, so the client is forced to go
through MCP (with a filesystem server, Claude/Cursor often use their own native
file tools and you'd only see the handshake).

## Setup

```bash
go install github.com/kerlenton/mcpsnoop/cmd/mcpsnoop@latest   # puts mcpsnoop on PATH

# wrap a real server with mcpsnoop in your client:
# Claude Code:
claude mcp add everything -- mcpsnoop -- npx -y @modelcontextprotocol/server-everything

# …or Claude Desktop (~/Library/Application Support/Claude/claude_desktop_config.json):
# { "mcpServers": { "everything": {
#     "command": "mcpsnoop",
#     "args": ["--", "npx", "-y", "@modelcontextprotocol/server-everything"] } } }
```

## Watch it live

1. In one terminal: `mcpsnoop`  (the TUI, which shows "Waiting for MCP traffic…").
2. Start a **new** client session (MCP servers load at session start), e.g. `claude`.
   The `server-everything` session appears live with the handshake.
3. Ask the client to use the tools:
   > Use the everything MCP server: echo "hello", get-sum 40+2, then trigger-long-running-operation.

   Frames stream into mcpsnoop in real time: `echo`/`get-sum` (OK), progress
   notifications, and `trigger-long-running-operation` (~5s, **SLOW**).
4. Drill in (`enter`), search (`/`), capabilities (`c`), replay (`r`).

(Both the shim, spawned by the client, and the TUI use the default home, so
they find each other automatically. Don't set `MCPSNOOP_HOME`.)
