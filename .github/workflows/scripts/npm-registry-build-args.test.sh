#!/usr/bin/env bash
# Runs npm-registry-build-args.sh (TXE-6357) under the runners it meets: one with no npm
# registry (GitHub-hosted), and an on-prem one whose LAN npm cache answers or is silent.
# A local HTTP server stands in for an answering cache, a closed port for a silent one.
# It also checks that each transports Dockerfile declares both registry ARGs in the stage
# that runs `npm ci`, before it, and that every image build of them passes the arguments.
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)"
ARGS="$ROOT/.github/workflows/scripts/npm-registry-build-args.sh"
WORK="$(mktemp -d)"
server_pid=
cleanup() {
  [[ -z $server_pid ]] || kill "$server_pid" 2>/dev/null || true
  rm -rf "${WORK:?}"
}
trap cleanup EXIT

failures=0
fail() { echo "FAIL: $*"; failures=$((failures + 1)); }

closed_port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')"
mkdir -p "$WORK/www/-"
echo pong > "$WORK/www/-/ping"
python3 -u -m http.server 0 --bind 127.0.0.1 --directory "$WORK/www" > "$WORK/server.log" 2>&1 &
server_pid=$!
open_port=
for _ in $(seq 1 50); do
  open_port="$(sed -n 's/.*port \([0-9][0-9]*\).*/\1/p' "$WORK/server.log" | head -1)"
  [[ -n $open_port ]] && curl -s -o /dev/null "http://127.0.0.1:$open_port/" && break
  sleep 0.1
done
[[ -n $open_port ]] || { echo "the stand-in npm cache did not start"; cat "$WORK/server.log"; exit 1; }
live="http://127.0.0.1:$open_port/"
dead="http://127.0.0.1:$closed_port/"

# args [--flags] -- VAR=VALUE...: the script's stdout under exactly that environment.
args() {
  local flag=()
  [[ $1 == --flags ]] && flag=(--flags)
  shift
  [[ $1 == -- ]] && shift
  env -i PATH="$PATH" HOME="$WORK" "$@" bash "$ARGS" ${flag[@]+"${flag[@]}"} 2> "$WORK/stderr"
}
expect() { # name, expected, actual
  [[ $3 == "$2" ]] || fail "$1: expected
$2
got
$3"
}

expect "a runner with no npm registry passes nothing" "" "$(args - --)"
grep -q "registry.npmjs.org" "$WORK/stderr" || fail "the public fallback is not reported"

expect "an answering cache goes to both names" \
"npm_config_registry=$live
pnpm_config_registry=$live" \
"$(args - -- npm_config_registry="$live")"

expect "--flags prints docker build words" \
"--build-arg
npm_config_registry=$live
--build-arg
pnpm_config_registry=$live" \
"$(args --flags -- npm_config_registry="$live")"

expect "a silent cache falls back to registry.npmjs.org" "" "$(args - -- npm_config_registry="$dead")"
grep -q "does not answer" "$WORK/stderr" || fail "a silent cache is not reported"

expect "a non-URL registry passes nothing" "" "$(args - -- npm_config_registry=registry.local)"

expect "the proxy is not used to reach the cache" \
"npm_config_registry=$live
pnpm_config_registry=$live" \
"$(args - -- npm_config_registry="$live" HTTPS_PROXY="$dead" HTTP_PROXY="$dead" http_proxy="$dead")"

# Every Dockerfile that runs npm ci declares both ARGs in that stage, before it.
for dockerfile in "$ROOT"/transports/Dockerfile*; do
  [[ -f $dockerfile ]] || continue
  missing="$(awk '
    toupper($1) == "FROM" { npm = 0; pnpm = 0; next }
    toupper($1) == "ARG" && $2 ~ /^npm_config_registry(=|$)/ { npm = 1 }
    toupper($1) == "ARG" && $2 ~ /^pnpm_config_registry(=|$)/ { pnpm = 1 }
    toupper($1) == "RUN" && /npm (ci|install|i)([[:space:];&|]|$)/ && !(npm && pnpm) { print NR }
  ' "$dockerfile")"
  [[ -z $missing ]] || fail "${dockerfile#"$ROOT"/}:$missing runs npm without both registry ARGs in scope"
done

# Every image build passes the arguments.
pipeline="$ROOT/.github/workflows/release-pipeline.yml"
builds="$(grep -c 'docker-build-push-action@' "$pipeline")"
passed="$(grep -c '\${{ steps.npm-routes.outputs.build-args }}' "$pipeline")"
routed="$(grep -c 'scripts/npm-registry-build-args.sh)' "$pipeline")"
[[ $builds -gt 0 && $passed == "$builds" && $routed == "$builds" ]] ||
  fail "release-pipeline.yml: $builds image builds, $routed npm-routes steps, $passed build-args that pass them"
grep -q 'npm-registry-build-args.sh" --flags' "$ROOT/.github/workflows/scripts/test-docker-image.sh" ||
  fail "test-docker-image.sh builds Dockerfile.local without the npm registry arguments"

if [[ $failures -gt 0 ]]; then
  echo "$failures check(s) failed"
  exit 1
fi
echo "npm-registry-build-args: all checks passed"
