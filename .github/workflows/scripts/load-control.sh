#!/usr/bin/env bash
# Builds bifrost-http at an earlier release tag, as the load test's same-runner control
# (TXE-7762). The tree is exported with git archive, so no worktree is created, and built the
# way load-test.sh builds the candidate when it has no prebuilt binary: the tag's own Go
# workspace script, and a placeholder for the embedded UI, which the load test never serves.
# A release binary and a build of the same commit without the UI measured the same: paired
# difference +9 µs on the non-streaming mean over 12 runs.
#
# Usage: load-control.sh <tag> <output binary>
set -euo pipefail

if [ $# -ne 2 ]; then
  echo "Usage: $0 <tag> <output binary>" >&2
  exit 2
fi
tag="$1"
out="$(cd "$(dirname "$2")" && pwd)/$(basename "$2")"
repo_root="$(git rev-parse --show-toplevel)"

if ! git -C "$repo_root" rev-parse -q --verify "refs/tags/${tag}^{commit}" > /dev/null; then
  echo "load-control: tag ${tag} is not in this checkout (fetch-depth 0 and fetch-tags are needed)" >&2
  exit 1
fi

src="$(mktemp -d)"
trap 'rm -rf "$src"' EXIT
git -C "$repo_root" archive "${tag}" | tar -x -C "$src"

if [ ! -f "$src/.github/workflows/scripts/setup-go-workspace.sh" ]; then
  echo "load-control: ${tag} has no setup-go-workspace.sh; it is older than this control supports" >&2
  exit 1
fi

(
  cd "$src"
  # shellcheck source=/dev/null
  source ./.github/workflows/scripts/setup-go-workspace.sh
  mkdir -p transports/bifrost-http/ui
  echo placeholder > transports/bifrost-http/ui/.gitkeep
  cd transports/bifrost-http
  go build -o "$out" .
)
echo "load-control: built ${tag} at ${out}"
