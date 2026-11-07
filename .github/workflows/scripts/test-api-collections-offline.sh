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
