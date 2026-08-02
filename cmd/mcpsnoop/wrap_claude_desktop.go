package main

import "github.com/kerlenton/mcpsnoop/internal/paths"

// claudeDesktopClient is the --client value for Claude Desktop, and the default
// wrap and unwrap assume.
const claudeDesktopClient = "claude-desktop"

// This file is the whole of Claude Desktop's support for wrap and unwrap, and it
// is the template for the next client: copy it, change the four fields, and the
// commands pick the client up with no edit to wrap.go. Everything below the
// registry is client independent, because every MCP client so far stores its
// servers the same way, as one object per server under a single top-level key.
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
