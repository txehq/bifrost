#!/usr/bin/env bash
# Tests check-api-artifacts.sh (TXE-7296): on a clean tree it passes, and each kind of drift it
# exists for fails it, naming the artifact, and leaves the tree as it found it.
#
# Each case runs in a throwaway clone of HEAD, so the working tree is never touched, and only
# the step it is about (--only): the invariants take most of the check's time, and the full check
# already runs on the real tree in the same job. Needs python3 with PyYAML and Go, as the check does.
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
check=".github/workflows/scripts/check-api-artifacts.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
failures=0

# run_case <name> <steps> <expected exit> <expected text> <mutation, run in the clone>
run_case() {
	local name="$1" steps="$2" want_exit="$3" want_text="$4" mutate="$5"
	local dir="$tmp/$name"
	git clone --quiet --shared --no-checkout "$root" "$dir"
	git -C "$dir" checkout --quiet "$(git -C "$root" rev-parse HEAD)"
	# The check under test is the working copy's, so an edit to it is tested before commit.
	cp "$root/$check" "$dir/$check"
	(cd "$dir" && eval "$mutate" && git add -A && git -c user.name=test -c user.email=test@example.invalid commit --quiet --allow-empty -m "$name")
	local out status
	out="$(cd "$dir" && bash "$check" --only "$steps" 2>&1)" && status=0 || status=$?
	local dirty
	dirty="$(git -C "$dir" status --porcelain)"
	if [ "$status" != "$want_exit" ] || ! grep -qF -- "$want_text" <<<"$out" || [ -n "$dirty" ]; then
		echo "not ok - $name: exit $status (want $want_exit), want text '$want_text', tree left: '${dirty}'"
		echo "$out" | tail -15 | sed 's/^/    /'
		failures=$((failures + 1))
	else
		echo "ok - $name"
	fi
}

run_case clean 1,2,3 0 "API artifacts agree with the spec." ":"

run_case stale-bundle 1 1 "openapi.json is bundled" \
	"python3 - <<'EOF'
import json
p='docs/openapi/openapi.json'
d=json.load(open(p))
d['info']['title'] += ' (stale)'
open(p,'w').write(json.dumps(d, indent=2) + '\n')
EOF"

run_case stale-catalog 2 1 "catalog.gen.go is generated" \
	"sed -i.bak '/ID: \"listStorageFolders\"/d' cli/internal/operations/catalog.gen.go && rm cli/internal/operations/catalog.gen.go.bak"

run_case stale-navigation 3 1 "docs.json navigation" \
	"python3 - <<'EOF'
p='docs/docs.json'
s=open(p).read()
s=s.replace('\"GET /api/governance/storage-folders\",\n', '', 1)
open(p,'w').write(s)
EOF"

run_case broken-invariant 4 1 "spec invariants" \
	"python3 - <<'EOF'
p='docs/openapi/paths/management/governance.yaml'
s=open(p).read()
s=s.replace('operationId: listStorageFolders', 'operationId: listRateLimits', 1)
open(p,'w').write(s)
EOF
(cd docs/openapi && python3 bundle.py >/dev/null) && (cd cli/internal/operations && go generate ./...) && python3 docs/openapi/sync_mintlify_navigation.py >/dev/null || true"

# An unknown argument is a usage error before anything is regenerated.
if out="$(cd "$root" && bash "$check" --bogus 2>&1)"; then status=0; else status=$?; fi
if [ "$status" = 2 ] && grep -qF -- "usage:" <<<"$out"; then
	echo "ok - bad-usage"
else
	echo "not ok - bad-usage: exit $status (want 2): $out"
	failures=$((failures + 1))
fi

if [ "$failures" -gt 0 ]; then
	echo "$failures case(s) failed"
	exit 1
fi
echo "all cases passed"
