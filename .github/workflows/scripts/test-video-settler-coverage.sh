#!/usr/bin/env bash
set -euo pipefail

# Every provider that implements video is settled, or exempt with a reason (TXE-7118).
#
# framework/jobaccounting keeps a hand-written list of the providers whose video jobs it
# settles. A provider that gained a video implementation without an entry had its jobs
# recorded and never charged (dashscope, TXE-6432; azure, TXE-7054). The test builds every
# standard provider from the workspace's core and fails for one that implements video and is
# in neither the list nor the exemption map. It dials loopback only and needs no credentials.
#
# It must test this checkout's core, not the released one framework/go.mod names, so it needs
# the repo's go.work. validate-schema-sync.sh provisions it; run this after that script.

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd -P)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

if [ ! -f "$REPO_ROOT/go.work" ]; then
  echo "❌ $REPO_ROOT/go.work not found: run validate-schema-sync.sh first, or make setup-workspace"
  exit 2
fi

cd "$REPO_ROOT/framework"
CGO_ENABLED=1 go test -timeout 10m \
  -run '^TestVideoSettler(CoversEveryProviderThatImplementsVideo|SupportsOnlyRealVideoProviders)$' \
  ./jobaccounting/
