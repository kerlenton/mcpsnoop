# Trying mcpsnoop for real

You don't need to write a server. Wrap a **published** one with `mcpsnoop` and
drive it with your own client. The `everything` test server works best. Its tools
have no local equivalent, so the client has to call them over MCP. A filesystem
server is a poorer choice, because clients like Claude or Cursor often use their
own native file tools and you would see only the handshake.

## Setup

Put mcpsnoop on your PATH.

```bash
go install github.com/kerlenton/mcpsnoop/cmd/mcpsnoop@latest
```

Wrap the server with mcpsnoop in your client. For Claude Code, one command does
it.

```bash
claude mcp add everything -- mcpsnoop -- npx -y @modelcontextprotocol/server-everything
```

For Claude Desktop, add the same wrap to your `claude_desktop_config.json`.

```json
{
  "mcpServers": {
    "everything": {
      "command": "mcpsnoop",
      "args": ["--", "npx", "-y", "@modelcontextprotocol/server-everything"]
    }
  }
}
```

## Watch it live

1. Run `mcpsnoop` in one terminal. The TUI opens and waits for MCP traffic.
2. Start a **new** client session, since MCP servers load at session start. The
   `everything` session shows up live with the handshake.
3. Ask the client to exercise the tools.

   > Use the everything MCP server to echo a short message, add 40 and 2, then
   > run its long-running operation.

4. Drill into a frame with `enter`, filter with `/`, inspect capabilities with
   `c`, and replay a call with `r`.

Frames stream in as the client works. The quick calls come back OK, and the long
one sends progress notifications and runs long enough to trip the **SLOW** flag.

The shim your client spawns and the TUI both use the default home, so they find
each other with no wiring. Leave `MCPSNOOP_HOME` unset.
