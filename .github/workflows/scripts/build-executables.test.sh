#!/usr/bin/env bash
set -uo pipefail

# Tests that build-executables.sh builds bifrost-http through the checked-in go.work.
#
# transports/go.mod pins core, framework and plugin versions whose module tags the fork never
# cuts (core/v1.8.6 and so on). A GOWORK=off build cannot resolve them, so every
# build-binaries job failed and the release stopped short of its tag (TXE-4957), while the
# Docker build, which copies go.work, published an image. This test makes the binary build
# use the same workspace as the image.
#
# The real script runs from a copy placed in a scratch repository layout, with a fake go
# first on PATH that records GOWORK, its working directory and its arguments, so nothing is
# compiled or downloaded.
#
# Usage: ./build-executables.test.sh
#        BUILD_EXECUTABLES=/path/to/other/build-executables.sh ./build-executables.test.sh

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TARGET="${BUILD_EXECUTABLES:-${SCRIPT_DIR}/build-executables.sh}"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
FAKE_BIN="$WORK/bin"
mkdir -p "$FAKE_BIN"
CALLS="$WORK/calls"

cat > "$FAKE_BIN/go" <<EOF
#!/usr/bin/env bash
echo "GOWORK=\${GOWORK-<unset>} PWD=\$PWD ARGS=\$*" >> "$CALLS"
out=""
while [ \$# -gt 0 ]; do
  if [ "\$1" = "-o" ]; then out="\$2"; shift; fi
  shift
done
[ -n "\$out" ] && : > "\$out"
exit 0
EOF
chmod +x "$FAKE_BIN/go"

FAILURES=0
pass() { echo "ok - $1"; }
fail() { echo "not ok - $1"; FAILURES=$((FAILURES + 1)); }

# make_repo <dir> <with-go.work: yes|no>
make_repo() {
  local repo="$1"
  mkdir -p "$repo/.github/workflows/scripts" "$repo/transports/bifrost-http"
  cp "$TARGET" "$repo/.github/workflows/scripts/build-executables.sh"
  if [ "$2" = yes ]; then
    printf 'go 1.27.0\n\nuse ./transports\n' > "$repo/go.work"
  fi
}

# run_build <repo> <platforms>; output in $WORK/out, exit status in $RC
run_build() {
  : > "$CALLS"
  (cd "$1" && PATH="$FAKE_BIN:$PATH" GOWORK=off \
    bash ./.github/workflows/scripts/build-executables.sh 9.9.9 "$2") > "$WORK/out" 2>&1
  RC=$?
}

# Scenario 1: every platform builds through the repository's go.work, even when the caller's
# environment says GOWORK=off.
REPO="$WORK/with-workspace"
make_repo "$REPO" yes
REPO="$(cd "$REPO" && pwd -P)"
ALL="darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64"
run_build "$REPO" "$ALL"
if [ "$RC" -eq 0 ]; then pass "build succeeds with go.work present"; else fail "build failed (rc=$RC): $(cat "$WORK/out")"; fi

builds=$(grep -c ' ARGS=build ' "$CALLS")
if [ "$builds" -eq 5 ]; then pass "one go build per platform"; else fail "expected 5 go build calls, got $builds"; fi

if grep ' ARGS=build ' "$CALLS" | grep -v "^GOWORK=$REPO/go.work " >/dev/null; then
  fail "a go build ran without GOWORK=$REPO/go.work: $(grep -v "^GOWORK=$REPO/go.work " "$CALLS")"
else
  pass "every go build uses the repository go.work"
fi

if grep ' ARGS=build ' "$CALLS" | grep -v " PWD=$REPO/transports/bifrost-http " >/dev/null; then
  fail "a go build ran outside transports/bifrost-http"
else
  pass "every go build runs in transports/bifrost-http"
fi

if grep ' ARGS=build ' "$CALLS" | grep -v -- '-X main.Version=v9.9.9' >/dev/null; then
  fail "a go build is missing the version ldflag"
else
  pass "every go build stamps the version"
fi

for p in darwin/amd64/bifrost-http darwin/arm64/bifrost-http linux/amd64/bifrost-http \
         linux/arm64/bifrost-http windows/amd64/bifrost-http.exe; do
  if [ -f "$REPO/dist/$p" ]; then pass "dist/$p written"; else fail "dist/$p missing"; fi
done

# Scenario 2: without go.work the script refuses before compiling anything, rather than
# falling back to resolving the unpublished module tags.
REPO="$WORK/without-workspace"
make_repo "$REPO" no
run_build "$REPO" "linux/amd64"
if [ "$RC" -ne 0 ]; then pass "build refuses without go.work"; else fail "build succeeded without go.work"; fi
if grep -q ' ARGS=build ' "$CALLS"; then fail "go build ran without go.work"; else pass "no go build without go.work"; fi
if grep -q 'go.work not found' "$WORK/out"; then pass "refusal names go.work"; else fail "refusal message missing: $(cat "$WORK/out")"; fi

# Scenario 3: the script itself never opts out of the workspace.
if grep -nE '^[^#]*GOWORK=off' "$TARGET"; then fail "build-executables.sh still sets GOWORK=off"; else pass "no GOWORK=off in build-executables.sh"; fi

if [ "$FAILURES" -gt 0 ]; then
  echo "$FAILURES check(s) failed"
  exit 1
fi
echo "all checks passed"
