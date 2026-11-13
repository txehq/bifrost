#!/usr/bin/env bash
# The API artifacts derived from the OpenAPI spec agree with it (TXE-7296).
#
# Four artifacts are generated from, or checked against, docs/openapi:
#
#   1. docs/openapi/openapi.json      bundled from openapi.yaml, paths/ and schemas/ (bundle.py)
#   2. cli/internal/operations/catalog.gen.go
#                                     the CLI's operation list, generated from openapi.json
#                                     (go generate in cli/internal/operations)
#   3. docs/docs.json                 the Mintlify API navigation (sync_mintlify_navigation.py)
#   4. the spec's own invariants      docs/openapi/spec_invariants_test.py
#
# Before this check, only the first was checked, after the merge, on main (openapi-bundle.yml),
# and the other three were never run in CI. They drifted: the catalog fell 86 operations behind,
# and the navigation sync and the invariants failed on main for months while every PR passed.
#
# Each artifact is regenerated into the working tree and compared with what is committed; the
# invariants run as they are. Every check runs, and the script fails if any did, naming each
# with the command that fixes it. It leaves the working tree as it found it.
#
# Run from the repository root. Needs python3 with PyYAML (bundle.py) and Go.
set -uo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"

failed=()
restore=()

cleanup() {
	if [ "${#restore[@]}" -gt 0 ]; then
		git checkout --quiet -- "${restore[@]}" 2>/dev/null || true
	fi
}
trap cleanup EXIT

# regenerated <path> <fix command> <description>: fails when <path> differs from what is committed.
regenerated() {
	local path="$1" fix="$2" what="$3"
	if git diff --quiet -- "$path"; then
		echo "ok - $what"
		return
	fi
	echo "FAIL - $what: $path is not what its generator produces"
	git --no-pager diff --stat -- "$path"
	echo "  fix: $fix, and commit the result"
	failed+=("$what")
	restore+=("$path")
}

for path in docs/openapi/openapi.json cli/internal/operations/catalog.gen.go docs/docs.json; do
	if ! git diff --quiet -- "$path"; then
		echo "error: $path has uncommitted changes; commit or discard them before running this check" >&2
		exit 2
	fi
done

echo "--- 1. docs/openapi/openapi.json is the bundle of the spec sources"
if (cd docs/openapi && python3 bundle.py >/dev/null); then
	regenerated docs/openapi/openapi.json "(cd docs/openapi && python3 bundle.py)" "openapi.json is bundled"
else
	echo "FAIL - bundle.py failed"
	failed+=("openapi.json bundle")
	restore+=(docs/openapi/openapi.json)
fi

echo "--- 2. the CLI operation catalog is generated from openapi.json"
if (cd cli/internal/operations && go generate ./...); then
	regenerated cli/internal/operations/catalog.gen.go "(cd cli/internal/operations && go generate ./...)" "catalog.gen.go is generated"
else
	echo "FAIL - go generate in cli/internal/operations failed"
	failed+=("catalog generation")
	restore+=(cli/internal/operations/catalog.gen.go)
fi

echo "--- 3. docs.json's API navigation lists every documented operation"
if python3 docs/openapi/sync_mintlify_navigation.py --check; then
	echo "ok - docs.json navigation"
else
	echo "  fix: python3 docs/openapi/sync_mintlify_navigation.py, and commit docs/docs.json"
	failed+=("docs.json navigation")
fi

echo "--- 4. the OpenAPI spec's invariants hold"
if python3 docs/openapi/spec_invariants_test.py; then
	echo "ok - spec invariants"
else
	echo "  fix: the failing invariants above, in docs/openapi"
	failed+=("spec invariants")
fi

if [ "${#failed[@]}" -gt 0 ]; then
	echo
	echo "API artifacts out of step with the spec: ${failed[*]}"
	exit 1
fi
echo
echo "API artifacts agree with the spec."
