#!/usr/bin/env bash
set -euo pipefail

# Validates that config.schema.json stays in sync with the Go config structs.
#
# This name is kept for anyone who runs it by hand (the Helm changelog names it); the check
# itself is validate-schema-sync.sh (schemasync), which this script runs.
#
# Until TXE-7111 this file was a second implementation of the same rule: it read json tags
# from five structs with awk and compared them to the schema, with an exclusion list of its
# own. No workflow ran it, so its list was never updated, and it failed on main for eight
# fields that are left out of the schema on purpose (the MCP instruction caps, which
# config.json sets only under mcp.tool_manager_config, and the MCP client's response-only
# OAuth fields). schemasync walks the whole config type graph, covers those five structs and
# more, runs on every pull request (api-collections-offline.yml, TXE-7064), and records each
# deliberate omission with its reason in schemasync/main.go (ignoreGoFields). One rule, one
# list: a field left out of the schema on purpose is recorded there and nowhere else in the
# scripts.
#
# Usage: ./validate-go-config-fields.sh

if command -v readlink >/dev/null 2>&1 && readlink -f "$0" >/dev/null 2>&1; then
  SCRIPT_DIR="$(dirname "$(readlink -f "$0")")"
else
  SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd -P)"
fi

exec bash "$SCRIPT_DIR/validate-schema-sync.sh" "$@"
