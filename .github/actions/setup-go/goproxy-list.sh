#!/usr/bin/env bash
# Prints the GOPROXY value that gives every module fetch three attempts at one proxy (TXE-7621).
#
# Usage: goproxy-list.sh [base-url]     (default https://proxy.golang.org)
#
# ⚠️ WHY THREE SPELLINGS. Entries separated by '|' make the go command try the next entry on ANY
# error, a stream reset in the middle of a .zip included (cmd/go truncates the partial file and
# tries again). But the go command caches a fetch, error and all, under the proxy URL as written,
# so the SAME URL listed twice fails twice without a second request. `base`, `base/` and `base//`
# are three cache keys for one host and one request path: the go command trims the slashes when
# it builds the request. Measured on 16 Nov 2026 with go1.27.3 against a proxy that fails each
# path's first request: `base|base` fails, `base|base/` succeeds after one failure per path.
#
# The trailing ',direct' keeps the go command's default: fetch from the origin only when the
# proxy answers 404 or 410 (a module it does not serve), not on an error.
set -euo pipefail
base="${1:-https://proxy.golang.org}"
base="${base%/}"
printf '%s|%s/|%s//,direct\n' "$base" "$base" "$base"
