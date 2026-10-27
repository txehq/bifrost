#!/usr/bin/env bash
set -uo pipefail

# Tests that run-migration-tests.sh only accepts a port served by the bifrost it started
# (TXE-5283): a listener that is not BIFROST_PID or its descendant must fail the run, as
# must a port nobody listens on.
#
# This runs the script's own bifrost_owns_port against local Python listeners on free
# loopback ports; nothing reaches the network.
#
# Usage: ./migration-port-owner.test.sh
#        RUN_MIGRATION_TESTS=/path/to/other/run-migration-tests.sh ./migration-port-owner.test.sh

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TARGET="${RUN_MIGRATION_TESTS:-${SCRIPT_DIR}/run-migration-tests.sh}"

functions="$(awk '/^bifrost_owns_port\(\) \{$/,/^\}$/' "$TARGET")"
[ -n "$functions" ] || { echo "FAIL: bifrost_owns_port not found in $TARGET" >&2; exit 1; }
log_info() { :; }
log_error() { echo "[ERROR] $*" >&2; }
eval "$functions"

failures=0
fail() {
  echo "FAIL: $1" >&2
  failures=$((failures + 1))
}

free_port() {
  python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'
}

# listen <port>: a loopback listener that runs until killed.
listen() {
  python3 -c 'import socket, sys, time
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", int(sys.argv[1]))); s.listen(); time.sleep(30)' "$1"
}

wait_listening() {
  local i
  for i in $(seq 1 50); do
    lsof -nP -t -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1 && return 0
    sleep 0.1
  done
  return 1
}

PIDS=()
cleanup() {
  local p
  for p in "${PIDS[@]:-}"; do
    [ -n "$p" ] || continue
    pkill -P "$p" 2>/dev/null
    kill "$p" 2>/dev/null
  done
}
trap cleanup EXIT

# A listener this run did not start: BIFROST_PID is an unrelated sibling process.
BIFROST_PORT=$(free_port)
listen "$BIFROST_PORT" & PIDS+=($!)
wait_listening "$BIFROST_PORT" || fail "foreign listener did not start"
sleep 30 & BIFROST_PID=$!; PIDS+=("$BIFROST_PID")
if bifrost_owns_port 2>/dev/null; then fail "a listener that is not BIFROST_PID or its child was accepted"; else echo "ok - foreign listener refused"; fi

# The started process's child serves the port, as npx's does.
BIFROST_PORT=$(free_port)
bash -c 'listen() { python3 -c "import socket, sys, time
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind((\"127.0.0.1\", int(sys.argv[1]))); s.listen(); time.sleep(30)" "$1"; }; listen "$0"; wait' "$BIFROST_PORT" &
BIFROST_PID=$!; PIDS+=("$BIFROST_PID")
wait_listening "$BIFROST_PORT" || fail "owned listener did not start"
if bifrost_owns_port; then echo "ok - listener descended from BIFROST_PID accepted"; else fail "the started process's child was refused"; fi

# Nobody listens.
BIFROST_PORT=$(free_port)
if bifrost_owns_port 2>/dev/null; then fail "a port with no listener was accepted"; else echo "ok - port without a listener refused"; fi

if [ "$failures" -ne 0 ]; then
  echo "$failures check(s) failed" >&2
  exit 1
fi
echo "OK: the migration tests accept only a port served by the bifrost they started"
