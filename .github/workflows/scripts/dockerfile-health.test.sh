#!/usr/bin/env bash
set -uo pipefail

# Checks that the command each production Dockerfile uses for its HEALTHCHECK exists in
# that Dockerfile's pinned final base image. `transports/Dockerfile.redhat` ran
# `curl-minimal`, the package and not its command, from the day the UBI9 image was added,
# so every UBI9 image reported unhealthy while it served requests. Nothing builds these
# Dockerfiles in a pull request, and the release's image test builds Dockerfile.local.
# Each base runs with no network, and only `command -v`.
#
# Usage: ./dockerfile-health.test.sh
# DOCKERFILE_HEALTH_ROOT points at another tree holding transports/Dockerfile*.

ROOT="${DOCKERFILE_HEALTH_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)}"
failed=0

check() { # Dockerfile, relative to the root
  local file="$ROOT/$1" base command
  base="$(awk 'toupper($1) == "FROM" { ref = $2 } END { print ref }' "$file")"
  command="$(sed -E ':a; /\\$/ { N; s/\\\n/ /; ba }' "$file" | grep -m1 -E '^[[:space:]]*HEALTHCHECK' | sed -E 's/.*[[:space:]]CMD[[:space:]]+//' | awk '{ print $1 }')"
  if [ -z "$base" ] || [ -z "$command" ]; then
    echo "FAIL: $1: cannot read its final base image and HEALTHCHECK command" >&2
    failed=$((failed + 1))
    return
  fi
  if docker run --rm --network none --entrypoint sh "$base" -c "command -v -- '$command'" >/dev/null; then
    echo "ok - $1: $command exists in $base"
  else
    echo "FAIL: $1: $command does not exist in $base" >&2
    failed=$((failed + 1))
  fi
}

check transports/Dockerfile
check transports/Dockerfile.redhat

if [ "$failed" -gt 0 ]; then
  exit 1
fi
echo "all checks passed"
