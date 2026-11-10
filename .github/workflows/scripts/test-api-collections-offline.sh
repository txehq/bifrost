#!/usr/bin/env bash
set -euo pipefail

# The offline API-collection suites: Node and a newman on PATH, no gateway, no backing
# services, no provider credentials, no live calls.
#
# This script is the one list of these suites. The release's test-api-integrations.sh
# runs it before building anything, and api-collections-offline.yml runs it on every
# pull request (TXE-6055), so what the release checks after the merge is what the PR
# checked before it. Add a suite here, never inline in a caller:
# test-api-collections-offline-callers.py fails a caller that runs one of its own.
#
#   collections/collection-scripts.test.mjs   pins the collections' assertion scripts and
#       checks one "Provider Secret Redaction" row per StandardProviders entry
#       (core/schemas/bifrost.go) and per SecretVar field. A provider added without its
#       row fails here, on the PR, not in the release run that then publishes nothing
#       (TXE-6051, run 42335012114).
#   runners/newman-exit-when-done.test.mjs    the request-guard runner's newman exit hook
#       (TXE-5180): never an early or false success. Spawns newman against a loopback
#       server of its own.
#   tests/test_offline_test.sh                 scripts/test-offline.sh, the runner every local
#       test run goes through so no credential reaches it (TXE-7053): fails when any
#       *_API_KEY, secret, token or cloud credential variable reaches the child. Bash only.
#   every other tests/e2e/api/**/*.test.mjs     found, not listed (TXE-7067): the runners/lib
#       script tests and any test added later. A test under tests/e2e/api runs here by
#       existing; none was run by any job before, so a renamed harness row the smoke manifest
#       lists went unnoticed (smoke-manifest.test.mjs, folder 19). A test that needs a network
#       or a credential goes in OFFLINE_EXCLUDED below with its reason; none does today.
#
# Usage: ./test-api-collections-offline.sh

if command -v readlink >/dev/null 2>&1 && readlink -f "$0" >/dev/null 2>&1; then
  SCRIPT_DIR="$(dirname "$(readlink -f "$0")")"
else
  SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd -P)"
fi
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd -P)"
SUITES_DIR="$REPO_ROOT/tests/e2e/api"

if ! command -v newman >/dev/null 2>&1; then
  echo "❌ newman is required on PATH: npm ci in .github/workflows/tools, then add its node_modules/.bin" >&2
  exit 1
fi

echo "🧪 collection-scripts.test.mjs (collection assertion scripts, provider redaction matrix)"
node "$SUITES_DIR/collections/collection-scripts.test.mjs"

echo ""
echo "🧪 newman-exit-when-done.test.mjs (newman exit hook)"
node "$SUITES_DIR/runners/newman-exit-when-done.test.mjs"

echo ""
echo "🧪 test_offline_test.sh (no credential reaches a test run, TXE-7053)"
bash "$REPO_ROOT/tests/test_offline_test.sh"

# Every other *.test.mjs under tests/e2e/api, found rather than listed, one at a time (some
# start a loopback server on a fixed port). Each runs with plain node, which also runs a
# node:test file and exits non-zero when one of its tests fails. Output is shown only for a
# failure. test-api-collections-offline-callers.py keeps this loop, and the exclusions'
# reasons, in place.
OFFLINE_EXCLUDED=() # "<path under tests/e2e/api>: <why it cannot run offline>"; none today
echo ""
echo "🧪 every other tests/e2e/api *.test.mjs (runners/lib script tests, TXE-7067)"
suite_log="$(mktemp)"
trap 'rm -f "$suite_log"' EXIT
suite_count=0
suite_failures=()
while IFS= read -r -d '' suite; do
  rel="${suite#"$SUITES_DIR/"}"
  case "$rel" in
  collections/collection-scripts.test.mjs | runners/newman-exit-when-done.test.mjs) continue ;; # run above
  esac
  excluded=""
  for entry in ${OFFLINE_EXCLUDED[@]+"${OFFLINE_EXCLUDED[@]}"}; do
    [ "${entry%%:*}" = "$rel" ] && excluded="$entry"
  done
  if [ -n "$excluded" ]; then
    echo "  skip - $excluded"
    continue
  fi
  suite_count=$((suite_count + 1))
  if node "$suite" >"$suite_log" 2>&1; then
    echo "  ok - $rel"
  else
    echo "  FAIL - $rel"
    tail -n 60 "$suite_log" | sed 's/^/      /'
    suite_failures+=("$rel")
  fi
done < <(find "$SUITES_DIR" -name '*.test.mjs' -not -path '*/node_modules/*' -print0 | sort -z)
if [ "$suite_count" -eq 0 ]; then
  echo "❌ no *.test.mjs found under $SUITES_DIR: the discovery no longer matches" >&2
  exit 1
fi
if [ "${#suite_failures[@]}" -gt 0 ]; then
  echo "❌ ${#suite_failures[@]} of $suite_count tests/e2e/api script tests failed: ${suite_failures[*]}" >&2
  exit 1
fi
echo "✅ $suite_count tests/e2e/api script tests passed"
