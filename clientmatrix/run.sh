#!/usr/bin/env bash
# Capture one MCP client talking to the reference server through mcpsnoop.
#
# usage: clientmatrix/run.sh <client> [capture-dir]
#
# Every client gets the same server and, if it is driven by a model, the same
# prompt in scenario.md. The capture lands in capture-dir/<client>.jsonl, and
# `mcpsnoop clients capture-dir/*.jsonl` turns them into the comparison.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
root="$(dirname "$here")"
client="${1:-}"
out="${2:-$here/captures}"

usage() {
	echo "usage: clientmatrix/run.sh go-sdk|go-sdk-2025-11-25|claude-code [capture-dir]" >&2
	exit 2
}
[ -n "$client" ] || usage

bin="$(mktemp -d)"
trap 'rm -rf "$bin"' EXIT
(cd "$root" && go build -o "$bin/mcpsnoop" ./cmd/mcpsnoop)
(cd "$here" && go build -o "$bin/refserver" ./refserver && go build -o "$bin/goclient" ./goclient)

mkdir -p "$out"
capture="$out/$client.jsonl"
rm -f "$capture"
wrap=("$bin/mcpsnoop" --label ref --trace-file "$capture" -- "$bin/refserver")

case "$client" in
go-sdk)
	"$bin/goclient" -- "${wrap[@]}"
	;;
go-sdk-2025-11-25)
	# The same SDK held to the older revision, so the server-initiated channel
	# the newer one replaced is exercised too.
	"$bin/goclient" -revision 2025-11-25 -name go-sdk-on-2025-11-25 -- "${wrap[@]}"
	;;
claude-code)
	config="$bin/claude.json"
	python3 - "$config" "${wrap[@]}" <<'PY'
import json, sys
path, command, *args = sys.argv[1:]
with open(path, "w") as f:
    json.dump({"mcpServers": {"ref": {"command": command, "args": args}}}, f)
PY
	# Run from an empty directory, so no project instructions or project settings
	# shape what the model does, and only the scenario and the server's own
	# descriptions reach it.
	(cd "$bin" && claude -p "$(cat "$here/scenario.md")" \
		--mcp-config "$config" --strict-mcp-config \
		--allowedTools "mcp__ref__echo,mcp__ref__confirm_action,mcp__ref__slow_task,mcp__ref__list_roots,mcp__ref__unlock_tool,mcp__ref__bonus_tool,mcp__ref__echo_read,mcp__ref__slow_read,mcp__ref__add_note,mcp__ref__count_notes,mcp__ref__clear_notes,ListMcpResourcesTool,ReadMcpResourceTool")
	;;
*)
	usage
	;;
esac
echo "capture: $capture"
