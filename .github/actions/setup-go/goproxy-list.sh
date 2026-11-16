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
# ⚠️ NO 'direct'. The go command's default ends in ',direct': on a 404 or 410 it fetches the
# module from its origin (git over github.com, go.googlesource.com, ...). harden-runner allows
# proxy.golang.org and github.com in the Go jobs, not every VCS host, so a direct fetch would trade
# a clear "not on the proxy" error for a blocked-host one. Every module the workspace needs is on
# proxy.golang.org, and GOSUMDB verifies what it serves.
set -euo pipefail
base="${1:-https://proxy.golang.org}"
base="${base%/}"
printf '%s|%s/|%s//\n' "$base" "$base" "$base"
