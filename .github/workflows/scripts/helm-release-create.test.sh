#!/usr/bin/env bash
set -uo pipefail

# Regression tests for helm-release-create.sh, the step of helm-release.yml
# that creates the GitHub release carrying a packaged chart.
#
# What went wrong before: the workflow inlined `gh release create` with no
# prerelease flag, so a prerelease chart version (2.1.46-sync.3, cut for an
# intermediate transport prerelease) would have been published as a regular
# release and taken the repository's "latest" release away from the published
# stable chart. The flag decision now lives in a script, so both paths are
# exercised here against a gh shim that records its arguments: a stable
# version gets no flag, a hyphenated one gets --prerelease, and a version or
# package that is not what it claims never reaches gh. One package is written
# the way `helm package` re-marshals Chart.yaml (keys sorted, maintainers[].name
# ahead of the chart's name): the first release run read that nested name and
# refused the genuine chart (TXE-5227). The last check pins the workflow to the
# script, so an inlined `gh release create` cannot come back.
#
# Nothing is created anywhere: gh is a shim on PATH. The fixture charts are
# tarballs built here with tar; the repository's own chart is packaged with the
# real helm, as the workflow does, so the Chart.yaml the script reads is the one
# helm writes.
#
# Usage: ./helm-release-create.test.sh

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TARGET="${SCRIPT_DIR}/helm-release-create.sh"
WORKFLOW="${SCRIPT_DIR}/../helm-release.yml"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

if [ ! -x "$TARGET" ]; then
  echo "FAIL: $TARGET is missing or not executable" >&2
  exit 1
fi
# The script reads the package through `helm show chart`, as the index and OCI
# scripts do, so the real helm is required (the lint job installs the same
# version the release job pins).
if ! command -v helm >/dev/null 2>&1; then
  echo "FAIL: helm is required on PATH (the script reads Chart.yaml with helm show chart)" >&2
  exit 1
fi

ROOT="$(mktemp -d "${TMPDIR:-/tmp}/helm-release-create.XXXXXX")"
trap 'rm -rf "$ROOT"' EXIT
export COPYFILE_DISABLE=1

passed=0
failed=0

ok() {
  echo "  ok - $1"
  passed=$((passed + 1))
}

fail() {
  echo "FAIL: $1" >&2
  failed=$((failed + 1))
}

check() { # description command...
  local desc="$1"
  shift
  if "$@"; then ok "$desc"; else fail "$desc"; fi
}

not() { ! "$@"; }

# make_chart NAME VERSION DIR -> packages a minimal chart as DIR/NAME-VERSION.tgz.
make_chart() {
  local name="$1" version="$2" dir="$3"
  mkdir -p "$dir/src/$name"
  printf 'apiVersion: v2\nname: %s\nversion: %s\n' "$name" "$version" > "$dir/src/$name/Chart.yaml"
  tar -czf "$dir/$name-$version.tgz" -C "$dir/src" "$name"
  echo "$dir/$name-$version.tgz"
}

# make_packaged_chart NAME VERSION DIR -> the same, with Chart.yaml written the
# way `helm package` re-marshals it: keys sorted, so maintainers[].name and a
# folded description precede the chart's own name and version, and the values
# quoted. This is what the workflow actually hands the script.
make_packaged_chart() {
  local name="$1" version="$2" dir="$3"
  mkdir -p "$dir/src/$name"
  cat > "$dir/src/$name/Chart.yaml" <<CHART
apiVersion: v2
appVersion: 1.5.12
description: A Helm chart for deploying Bifrost - AI Gateway with unified interface
  for multiple providers
maintainers:
- email: support@example.com
  name: Bifrost Team
  version: not-the-chart-version
name: "$name"
sources:
- https://github.com/example/$name
type: application
version: '$version'
CHART
  tar -czf "$dir/$name-$version.tgz" -C "$dir/src" "$name"
  echo "$dir/$name-$version.tgz"
}

# The gh shim records every invocation, one argument per line, in $ROOT/gh-args
# and counts calls in $ROOT/gh-calls.
mkdir -p "$ROOT/bin"
cat > "$ROOT/bin/gh" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$@" > "$ROOT/gh-args"
echo call >> "$ROOT/gh-calls"
EOF
chmod +x "$ROOT/bin/gh"

run() { # VERSION PACKAGE [extra env assignments...] -> runs the script, keeps rc in $RC
  local version="$1" package="$2"
  shift 2
  rm -f "$ROOT/gh-args" "$ROOT/gh-calls"
  : > "$ROOT/log"
  (env "$@" PATH="$ROOT/bin:$PATH" GH_TOKEN="${GH_TOKEN_OVERRIDE-fixture}" \
    bash "$TARGET" --chart-name bifrost --chart-version "$version" --package "$package" \
    --target 0123456789abcdef0123456789abcdef01234567 --repository txehq/bifrost) > "$ROOT/log" 2>&1
  RC=$?
}

gh_called() { [ -f "$ROOT/gh-calls" ]; }
gh_arg() { grep -qxF -- "$1" "$ROOT/gh-args"; }
gh_arg_count() { grep -cxF -- "$1" "$ROOT/gh-args" 2>/dev/null || echo 0; }

echo "stable chart version"
STABLE="$(make_chart bifrost 2.1.46 "$ROOT/stable")"
run 2.1.46 "$STABLE"
check "the script succeeds" [ "$RC" -eq 0 ]
check "gh is called once" [ "$(cat "$ROOT/gh-calls" 2>/dev/null | wc -l | tr -d ' ')" = "1" ]
check "the release is created" gh_arg release
check "creation, not another subcommand" gh_arg create
check "the tag is helm-chart-v2.1.46" gh_arg helm-chart-v2.1.46
check "the package is the release asset" gh_arg "$STABLE"
check "the target commit is passed" gh_arg 0123456789abcdef0123456789abcdef01234567
check "the repository is passed" gh_arg txehq/bifrost
check "a stable version gets no --prerelease" not gh_arg --prerelease
check "the log names the stable path" grep -q "is a stable version" "$ROOT/log"

echo "prerelease chart version"
PRE="$(make_chart bifrost 2.1.46-sync.3 "$ROOT/pre")"
run 2.1.46-sync.3 "$PRE"
check "the script succeeds" [ "$RC" -eq 0 ]
check "the tag is helm-chart-v2.1.46-sync.3" gh_arg helm-chart-v2.1.46-sync.3
check "the package is the release asset" gh_arg "$PRE"
check "a hyphenated version gets --prerelease" gh_arg --prerelease
check "--prerelease is passed exactly once" [ "$(gh_arg_count --prerelease)" = "1" ]
check "the log names the prerelease path" grep -q "is a prerelease" "$ROOT/log"

echo "a package as helm writes it: nested keys ahead of the chart's own"
PACKAGED="$(make_packaged_chart bifrost 2.1.46-sync.3 "$ROOT/packaged")"
run 2.1.46-sync.3 "$PACKAGED"
check "the script succeeds" [ "$RC" -eq 0 ]
check "the tag is helm-chart-v2.1.46-sync.3" gh_arg helm-chart-v2.1.46-sync.3
check "the package is the release asset" gh_arg "$PACKAGED"
check "a hyphenated version gets --prerelease" gh_arg --prerelease
PACKAGED_STABLE="$(make_packaged_chart bifrost 2.1.46 "$ROOT/packaged-stable")"
run 2.1.46 "$PACKAGED_STABLE"
check "the stable package is accepted too" [ "$RC" -eq 0 ]
check "and gets no --prerelease" not gh_arg --prerelease
run 2.1.47 "$PACKAGED_STABLE"
check "a packaged chart naming another version is still refused" [ "$RC" -ne 0 ]
check "gh is not called for it" not gh_called

echo "the repository's own chart, packaged by helm"
REAL_VERSION="$(awk '/^version:/ { print $2; exit }' "$REPO_ROOT/helm-charts/bifrost/Chart.yaml")"
mkdir -p "$ROOT/real"
helm package "$REPO_ROOT/helm-charts/bifrost" -d "$ROOT/real" >/dev/null 2>&1
REAL="$ROOT/real/bifrost-$REAL_VERSION.tgz"
check "helm packaged helm-charts/bifrost as version $REAL_VERSION" [ -f "$REAL" ]
run "$REAL_VERSION" "$REAL"
check "the real package is accepted" [ "$RC" -eq 0 ]
check "the tag is helm-chart-v$REAL_VERSION" gh_arg "helm-chart-v$REAL_VERSION"
check "the real package is the release asset" gh_arg "$REAL"
case "$REAL_VERSION" in
  *-*) check "the real version is a prerelease, so --prerelease" gh_arg --prerelease ;;
  *) check "the real version is stable, so no --prerelease" not gh_arg --prerelease ;;
esac
run "${REAL_VERSION}x" "$REAL"
check "the real package under another version is refused" [ "$RC" -ne 0 ]
check "gh is not called for it" not gh_called

echo "a prerelease with a build suffix is still a prerelease"
PRE_BUILD="$(make_chart bifrost 2.1.46-rc.1+fork "$ROOT/prebuild")"
run 2.1.46-rc.1+fork "$PRE_BUILD"
check "the script succeeds" [ "$RC" -eq 0 ]
check "the hyphen decides the flag" gh_arg --prerelease

echo "refusals never reach gh"
run "2.1.46-" "$STABLE"
check "an empty suffix is refused" [ "$RC" -ne 0 ]
check "gh is not called for an empty suffix" not gh_called
run '2.1.46$(touch /tmp/helm-release-create-pwned)' "$STABLE"
check "a version carrying command substitution is refused" [ "$RC" -ne 0 ]
check "gh is not called for it" not gh_called
check "nothing was executed" [ ! -e /tmp/helm-release-create-pwned ]
run 2.1.46 "$ROOT/stable/missing-2.1.46.tgz"
check "a missing package is refused" [ "$RC" -ne 0 ]
check "gh is not called for a missing package" not gh_called
run 2.1.47 "$STABLE"
check "a package named for another version is refused" [ "$RC" -ne 0 ]
check "gh is not called for it" not gh_called
MISMATCH_DIR="$ROOT/mismatch"
make_chart bifrost 2.1.46 "$MISMATCH_DIR" >/dev/null
mkdir -p "$MISMATCH_DIR/renamed"
cp "$MISMATCH_DIR/bifrost-2.1.46.tgz" "$MISMATCH_DIR/renamed/bifrost-2.1.46-sync.3.tgz"
run 2.1.46-sync.3 "$MISMATCH_DIR/renamed/bifrost-2.1.46-sync.3.tgz"
check "a package whose Chart.yaml names another version is refused" [ "$RC" -ne 0 ]
check "gh is not called for it" not gh_called
GH_TOKEN_OVERRIDE="" run 2.1.46 "$STABLE" GITHUB_TOKEN=
check "a run without a token is refused" [ "$RC" -ne 0 ]
check "gh is not called without a token" not gh_called
unset GH_TOKEN_OVERRIDE

echo "workflow wiring"
check "helm-release.yml delegates release creation to the script" grep -q 'scripts/helm-release-create.sh' "$WORKFLOW"
check "helm-release.yml passes the chart version to it" grep -q -- '--chart-version "\$VERSION"' "$WORKFLOW"
check "helm-release.yml passes the merge commit as the target" grep -q -- '--target "\$GITHUB_SHA"' "$WORKFLOW"
check "helm-release.yml no longer inlines gh release create" not grep -q 'gh release create' "$WORKFLOW"

echo
echo "$passed passed, $failed failed"
[ "$failed" -eq 0 ]
