#!/usr/bin/env bash
# Proves the GOPROXY list that ./.github/actions/setup-go sets rides out a transient proxy failure
# (TXE-7621). Offline: go-fault-proxy.py serves a synthetic module from memory and fails the first
# requests for each path (a 500, or half a .zip and a closed connection).
#
#   1. Control: a plain GOPROXY (one entry) fails on one injected failure, so the injection bites.
#   2. The list from goproxy-list.sh builds a module that imports the synthetic one (the lazy fetch
#      inside `go build`, the path the CI failure took) through one injected failure per path:
#      the .info, the .mod and the .zip each fail once.
#   3. Two failures per path still succeed.
#   4. Three failures per path fail: three attempts is the bound, and the failure names the proxy.
#
# Needs go and python3. Run from anywhere.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
LIST="$ROOT/.github/actions/setup-go/goproxy-list.sh"
PROXY="$ROOT/.github/workflows/scripts/go-fault-proxy.py"
WORK="$(mktemp -d)"
PROXY_PID=""
cleanup() {
  [[ -n "$PROXY_PID" ]] && kill "$PROXY_PID" 2>/dev/null || true
  chmod -R u+w "$WORK" 2>/dev/null || true
  rm -r "$WORK"
}
trap cleanup EXIT

mkdir -p "$WORK/consumer"
cat >"$WORK/consumer/go.mod" <<'EOF'
module example.test/consumer

go 1.21

require example.test/retrydep v1.0.0
EOF
cat >"$WORK/consumer/main.go" <<'EOF'
package main

import (
	"fmt"

	"example.test/retrydep"
)

func main() { fmt.Println(retrydep.Answer) }
EOF

start_proxy() {
  local fail_times="$1" port_file="$WORK/port"
  rm -f "$port_file"
  python3 -I "$PROXY" --port-file "$port_file" --fail-times "$fail_times" 2>"$WORK/proxy.log" &
  PROXY_PID=$!
  for _ in $(seq 1 50); do
    [[ -s "$port_file" ]] && break
    sleep 0.1
  done
  [[ -s "$port_file" ]] || { echo "FAIL: go-fault-proxy.py did not start"; cat "$WORK/proxy.log"; exit 1; }
  PORT="$(cat "$port_file")"
}

stop_proxy() {
  kill "$PROXY_PID" 2>/dev/null || true
  wait "$PROXY_PID" 2>/dev/null || true
  PROXY_PID=""
}

# Builds the consumer with a fresh module cache. GOSUMDB=off: the synthetic module has no checksum
# database entry; -mod=mod lets the go command write go.sum.
build() {
  local goproxy="$1"
  rm -rf "$WORK/consumer/go.sum"
  [[ -d "$WORK/modcache" ]] && chmod -R u+w "$WORK/modcache" && rm -r "$WORK/modcache"
  (cd "$WORK/consumer" && env GOPROXY="$goproxy" GOMODCACHE="$WORK/modcache" GOFLAGS=-mod=mod \
    GOSUMDB=off GONOSUMDB= GOPRIVATE= GOWORK=off GOTOOLCHAIN=local go build -o "$WORK/consumer.bin" . 2>&1)
}

injected() { grep -c 'INJECTED' "$WORK/proxy.log" || true; }

fails=0
fail() { echo "FAIL: $*"; fails=$((fails + 1)); }

# 1. Control.
start_proxy 1
if out="$(build "http://127.0.0.1:$PORT")"; then
  fail "a single-entry GOPROXY built through an injected failure; the fault proxy is not injecting"
else
  echo "ok: a single-entry GOPROXY fails on one injected failure ($(injected) injected)"
fi
stop_proxy

# 2. The list, one failure per path.
start_proxy 1
goproxy="$("$LIST" "http://127.0.0.1:$PORT")"
if out="$(build "$goproxy")"; then
  n="$(injected)"
  if (( n >= 3 )); then
    echo "ok: GOPROXY=$goproxy built through $n injected failures (.info, .mod, .zip)"
  else
    fail "only $n injected failures; want one each for .info, .mod and .zip: $(cat "$WORK/proxy.log")"
  fi
  [[ "$("$WORK/consumer.bin")" == 42 ]] || fail "the built consumer did not print 42"
else
  fail "the retry list did not ride out one failure per path: $out"
fi
stop_proxy

# 3. Two failures per path.
start_proxy 2
if out="$(build "$("$LIST" "http://127.0.0.1:$PORT")")"; then
  echo "ok: two failures per path ride out ($(injected) injected)"
else
  fail "two failures per path failed the build: $out"
fi
stop_proxy

# 4. The bound.
start_proxy 3
if out="$(build "$("$LIST" "http://127.0.0.1:$PORT")")"; then
  fail "three failures per path built; the list has more than three attempts or the injection is off"
elif grep -q "127.0.0.1:$PORT" <<<"$out"; then
  echo "ok: three failures per path fail, naming the proxy"
else
  fail "three failures per path failed without naming the proxy: $out"
fi
stop_proxy

if (( fails > 0 )); then
  echo "go-proxy-retry: $fails failure(s)"
  exit 1
fi
echo "go-proxy-retry: all cases pass"
