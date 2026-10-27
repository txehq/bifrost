#!/usr/bin/env bash
set -euo pipefail

# Verify a bifrost-http release's dependency metadata and build, without changing it.
# Usage: ./release-bifrost-http-prep.sh <version>
#
# Verify only: this never edits a tracked file, and never pulls, commits or pushes.
# main requires a reviewed codex-review check, and the release has to build exactly
# the commit its tests ran on, so a dependency bump lands through a reviewed PR first.
#
# transports/go.mod must require core, framework and every plugin that has a version
# file at that version, under the go directive below. That is compared textually with
# `go mod edit -json`: no module lookups and no `go mod tidy`. Tidy ignores go.work,
# and the fork publishes none of these module versions as tags, so it cannot resolve
# them; the release builds the checked-out modules through go.work either way.
#
# On drift the exact go.mod change is written as a patch outside the checkout (see
# release-patch-utils.sh) and the script fails before building anything: the release
# waits until that patch has merged through a reviewed PR.

# Get the absolute path of the script directory
# Use readlink if available (Linux), otherwise use cd/pwd (macOS compatible)
if command -v readlink >/dev/null 2>&1 && readlink -f "$0" >/dev/null 2>&1; then
  SCRIPT_DIR="$(dirname "$(readlink -f "$0")")"
else
  SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd -P)"
fi

source "$SCRIPT_DIR/release-patch-utils.sh"

# Validate input argument
if [ "${1:-}" = "" ]; then
  echo "Usage: $0 <version>" >&2
  exit 1
fi

VERSION="$1"
# The Go the release jobs set up; the go directive is held to it.
GO_DIRECTIVE="1.27.2"
MODULE_ROOT="$(go mod edit -json core/go.mod | jq -r '.Module.Path | sub("/core$"; "")')"

assert_tested_checkout
TRACKED_BEFORE="$(git status --porcelain=v1 --untracked-files=no)"

echo "🔍 Verifying transports/go.mod for bifrost-http v$VERSION..."

# Expected version per internal module, from the version files.
declare -A EXPECTED
EXPECTED["$MODULE_ROOT/core"]="v$(tr -d '\n\r' < core/version)"
EXPECTED["$MODULE_ROOT/framework"]="v$(tr -d '\n\r' < framework/version)"
for version_file in plugins/*/version; do
  if [ -f "$version_file" ]; then
    plugin_name="$(basename "$(dirname "$version_file")")"
    EXPECTED["$MODULE_ROOT/plugins/$plugin_name"]="v$(tr -d '\n\r' < "$version_file")"
  fi
done

GOMOD_JSON="$(go mod edit -json transports/go.mod)"
EDITS=()

current_go="$(jq -r '.Go // ""' <<<"$GOMOD_JSON")"
if [ "$current_go" != "$GO_DIRECTIVE" ]; then
  echo "   ✗ go directive is '$current_go', expected $GO_DIRECTIVE"
  EDITS+=("-go=$GO_DIRECTIVE")
fi
current_toolchain="$(jq -r '.Toolchain // ""' <<<"$GOMOD_JSON")"
if [ -n "$current_toolchain" ]; then
  echo "   ✗ toolchain line '$current_toolchain', expected none"
  EDITS+=("-toolchain=none")
fi

# A plugin the transport requires without a version file keeps its pin, as before.
# core and framework are required whether or not the transport lists them yet.
declare -A SEEN
while IFS=$'\t' read -r path version; do
  SEEN["$path"]=1
  expected="${EXPECTED[$path]:-}"
  if [ -z "$expected" ]; then
    echo "   - $path $version (no version file)"
  elif [ "$version" = "$expected" ]; then
    echo "   ✓ $path $version"
  else
    echo "   ✗ $path $version, expected $expected"
    EDITS+=("-require=$path@$expected")
  fi
done < <(jq -r --arg root "$MODULE_ROOT/" '.Require[]? | select(.Path | startswith($root)) | [.Path, .Version] | @tsv' <<<"$GOMOD_JSON")
for path in "$MODULE_ROOT/core" "$MODULE_ROOT/framework"; do
  if [ -z "${SEEN[$path]:-}" ]; then
    echo "   ✗ $path is not required, expected ${EXPECTED[$path]}"
    EDITS+=("-require=$path@${EXPECTED[$path]}")
  fi
done

if [ "${#EDITS[@]}" -gt 0 ]; then
  # The change is made on copies outside the checkout, by `go mod edit` alone.
  WORK="$(mktemp -d)"
  trap 'rm -rf "$WORK"' EXIT
  mkdir -p "$WORK/a/transports" "$WORK/b/transports"
  cp transports/go.mod "$WORK/a/transports/go.mod"
  cp transports/go.mod "$WORK/b/transports/go.mod"
  go mod edit "${EDITS[@]}" "$WORK/b/transports/go.mod"

  PATCH_DIR="$(release_patch_dir)"
  PATCH="$PATCH_DIR/$(release_patch_name release-prep "$VERSION")"
  status=0
  (cd "$WORK" && git diff --no-index --no-prefix --no-color --no-ext-diff a/transports/go.mod b/transports/go.mod) > "$PATCH" || status=$?
  if [ "$status" -ne 1 ]; then
    echo "::error::could not write the transports/go.mod patch (git diff exited $status)" >&2
    exit 1
  fi
  assert_patch_allowlist "$PATCH" transports/go.mod

  release_summary \
    "### bifrost-http v$VERSION release prep: pending reviewed PR" \
    "" \
    "transports/go.mod at $(git rev-parse HEAD) does not match the module version files, so nothing was built or published from this commit." \
    "Patch artifact: \`$(basename "$PATCH")\`. Apply it in a reviewed PR for this release's dependency ticket; the merged commit then releases."
  echo "::error::release prep pending: transports/go.mod needs a reviewed PR (patch $(basename "$PATCH"))"
  exit 1
fi

# Build what the release ships, from the checked-out modules through go.work. The
# UI and plugin outputs are gitignored; -mod=readonly keeps go from rewriting go.mod.
echo "🎨 Building UI..."
make build-ui

echo "🔨 Building hello-world plugin..."
(cd examples/plugins/hello-world && make build)

echo "🔨 Validating transport build..."
(cd transports && go build -mod=readonly ./...)

if [ "$(git status --porcelain=v1 --untracked-files=no)" != "$TRACKED_BEFORE" ]; then
  echo "::error::the prep build changed tracked files:" >&2
  git status --short --untracked-files=no >&2
  exit 1
fi

release_summary "### bifrost-http v$VERSION release prep: verified" "" "transports/go.mod matches the module version files at $(git rev-parse HEAD); no changes."
echo "success=true" >> "${GITHUB_OUTPUT:-/dev/null}"
