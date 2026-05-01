#!/usr/bin/env bash
# Send MCP initialize + tool call via stdio. JSON-RPC framing is line-delimited.
set -euo pipefail
BIN="${1:-./hami-mcp-server}"
TOOL="${2:-get_cluster_summary}"
ARGS="${3:-{}}"

{
  printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"shell-test","version":"0.0.1"}}}'
  printf '%s\n' '{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}'
  printf '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"%s","arguments":%s}}\n' "$TOOL" "$ARGS"
} | "$BIN"
