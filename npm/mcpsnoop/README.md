# mcpsnoop

**Wireshark for MCP.** A transparent proxy that shows every real tool call
between your AI client and your MCP servers, live in your terminal.

No Go toolchain needed. Put it in front of the server you are writing and watch
the wire.

```bash
npx mcpsnoop -- node build/index.js
```

Everything the client and the server say to each other goes through, unchanged,
and shows up in the UI. Open it in another terminal.

```bash
npx mcpsnoop
```

For a streamable-HTTP server, run it as a reverse proxy instead.

```bash
npx mcpsnoop http --target http://localhost:3000/mcp --listen :7000
```

## What this package is

The binary is written in Go. This package ships none of it. The six platform
packages under the `@mcpsnoop` scope each carry one build, and npm installs the
single one that matches your machine. So there is no download step at install
time, nothing to unblock in a proxy, and it works under `--ignore-scripts`.

Supported are macOS, Linux and Windows on x64 and arm64. On anything else,
install from source.

```bash
go install github.com/kerlenton/mcpsnoop/cmd/mcpsnoop@latest
```

## Documentation

The full README, including what the UI shows, the recording format, and
`mcpsnoop check` for CI, is at
[github.com/kerlenton/mcpsnoop](https://github.com/kerlenton/mcpsnoop).

MIT licensed.
