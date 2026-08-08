package main

import "github.com/kerlenton/mcpsnoop/internal/paths"

// claudeDesktopClient is the --client value for Claude Desktop, and the default
// wrap and unwrap assume.
const claudeDesktopClient = "claude-desktop"

// This file is the whole of Claude Desktop's support for wrap and unwrap, and it
// is the template for the next client: copy it, change the fields, and the
// commands pick the client up with no edit to wrap.go.
//
// The entry shape really is client independent. Every MCP client so far stores
// its servers as one object per server under a single top-level key, so
// serversKey is all that varies and every key mcpsnoop does not model survives
// the rewrite. The location is not. VS Code reads .vscode/mcp.json in the
// workspace as well as an mcp.json in the user profile, and says "When you use
// multiple profiles, each profile can have its own MCP server configuration";
// Cursor and Claude Code split the same way. configPath returns one path and
// takes no arguments, so for those the user has to pass --config until it grows
// a notion of scope. wrapperPath is not client independent either once a
// workspace-scoped client is added, since a machine-local absolute path is
// wrong in a file a team commits.
func init() {
	registerWrapClient(wrapClient{
		name:       claudeDesktopClient,
		display:    "Claude Desktop",
		serversKey: "mcpServers",
		configPath: paths.ClaudeDesktopConfig,
		restartHint: "quit Claude Desktop completely and start it again, since MCP servers " +
			"are launched once at startup",
	})
}
