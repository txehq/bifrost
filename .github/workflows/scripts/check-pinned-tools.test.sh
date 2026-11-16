#!/usr/bin/env bash
# Fixture cases for check-pinned-tools.py (TXE-7592).
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
check="$here/check-pinned-tools.py"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fail=0

expect() { # expect <name> <pass|fail> <file content>
  local name="$1" want="$2" file="$tmp/$1.yml"
  printf '%s\n' "$3" > "$file"
  if python3 "$check" "$file" >/dev/null; then got=pass; else got=fail; fi
  if [ "$got" = "$want" ]; then echo "ok   $name"; else echo "FAIL $name: want $want, got $got"; fail=1; fi
}

# The fixtures split the pattern, so this file does not trip the whole-repository case.
expect npx-latest-fails fail '          npx --yes mintlify@''latest broken-links'
expect go-install-latest-fails fail '    go install github.com/tsenart/vegeta/v12@''latest'
expect npx-pinned-passes pass '          npx --yes "mintlify@${MINTLIFY_VERSION}" broken-links'
expect go-install-pinned-passes pass '    go install github.com/tsenart/vegeta/v12@v12.13.0'
expect comment-passes pass '# never npx mintlify@''latest'
expect action-sha-passes pass '      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6.0.2'
expect latest-tag-name-passes pass '    image: ghcr.io/txehq/bifrost:latest'

if python3 "$check" >/dev/null; then echo "ok   whole-repository-passes"; else echo "FAIL whole-repository-passes"; python3 "$check" | tail -5; fail=1; fi
[ "$fail" = 0 ] && echo "All pinned-tool cases passed"
exit "$fail"
