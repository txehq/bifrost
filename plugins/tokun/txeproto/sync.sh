#!/usr/bin/env bash
# Replaces this module's copy of the Tokun contracts with txe's generated Go
# code: every .go file under <txe checkout>/tokun/proto/gen/go/tokun/ lands
# here under tokun/ at the same relative path, byte for byte, and files txe no
# longer has are removed. txe's BUILD.bazel files are not copied.
#
#   plugins/tokun/txeproto/sync.sh <txe checkout>
set -euo pipefail

[ $# -eq 1 ] || { echo "usage: $0 <txe checkout>" >&2; exit 2; }
src="$(cd "$1" && pwd)/tokun/proto/gen/go/tokun"
[ -d "${src}" ] || { echo "sync.sh: ${src} does not exist" >&2; exit 1; }

here="$(cd "$(dirname "$0")" && pwd)"
rm -rf "${here}/tokun"
mkdir -p "${here}/tokun"
(cd "${src}" && find . -type f -name '*.go' -print0 | xargs -0 cp --parents -t "${here}/tokun" --)

echo "sync.sh: copied the .go files of ${src} from txe $(git -C "$1" rev-parse --short HEAD)"
