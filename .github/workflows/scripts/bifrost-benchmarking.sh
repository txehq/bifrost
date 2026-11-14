#!/usr/bin/env bash
# Sourced, not run: fetches maximhq/bifrost-benchmarking, whose mocker and hitter the cost
# accuracy and load tests build and run, at one pinned commit (TXE-7452).
#
# That repository belongs to another organisation. The release jobs checked out its default
# branch at whatever commit it had, and every pull request now runs the cost accuracy test, so
# its code comes from this reviewed commit only. Move the pin in a reviewed change.
#
#   source .github/workflows/scripts/bifrost-benchmarking.sh
#   fetch_bifrost_benchmarking "$BENCHMARK_DIR"
#
# A directory that already exists is used only when its HEAD is the pinned commit.

BIFROST_BENCHMARKING_REPO="https://github.com/maximhq/bifrost-benchmarking.git"
BIFROST_BENCHMARKING_SHA="3cb72a95cacaee6f73f9db5636a1a651c7dcb1e9"

fetch_bifrost_benchmarking() {
  local dir="$1" head
  if [ ! -d "$dir" ]; then
    echo "fetching bifrost-benchmarking at ${BIFROST_BENCHMARKING_SHA}"
    mkdir -p "$(dirname "$dir")"
    git init -q "$dir"
    git -C "$dir" fetch -q --depth 1 "$BIFROST_BENCHMARKING_REPO" "$BIFROST_BENCHMARKING_SHA"
    git -C "$dir" -c advice.detachedHead=false checkout -q FETCH_HEAD
  fi
  head="$(git -C "$dir" rev-parse HEAD 2>/dev/null || true)"
  if [ "$head" != "$BIFROST_BENCHMARKING_SHA" ]; then
    echo "❌ $dir is not bifrost-benchmarking at ${BIFROST_BENCHMARKING_SHA} (HEAD ${head:-none}); remove it or set BENCHMARK_DIR to a new path" >&2
    return 1
  fi
}
