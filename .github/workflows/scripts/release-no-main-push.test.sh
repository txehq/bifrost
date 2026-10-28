#!/usr/bin/env bash
set -uo pipefail

# Tests that the release scripts which used to push to main now leave their changes as
# patches for a reviewed PR: release-bifrost-http-prep.sh, push-mintlify-changelog.sh
# and push-cli-mintlify-changelog.sh.
#
# main requires a reviewed codex-review check, so a release job cannot push to it. Each
# scenario runs a real script in a clone of a local bare origin whose pre-receive hook
# rejects refs/heads/main and logs every ref update. The origin's main has moved on
# since the clone, so a pull would show up as a moved HEAD. Shims first on PATH record
# and refuse `git pull/fetch/commit/push`, refuse `go mod tidy/get/download`, and stand
# in for `go build` and `make`; `go mod edit`, node and every other git command are
# real. Nothing leaves the machine, and everything lives under one temporary directory.
#
# Usage: ./release-no-main-push.test.sh
#        RELEASE_SCRIPTS_DIR=/path/to/other/scripts ./release-no-main-push.test.sh

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPTS="${RELEASE_SCRIPTS_DIR:-$SCRIPT_DIR}"

for tool in git go jq node; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "FAIL: $tool is required" >&2
    exit 1
  fi
done
REAL_GIT="$(command -v git)"
REAL_GO="$(command -v go)"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
FAKE_BIN="$WORK/bin"
LOG="$WORK/calls.log"
mkdir -p "$FAKE_BIN"

# Sentinels standing in for secrets in the job environment: none may reach a patch,
# the summary, the step outputs or the script output.
SECRET_KEY="sk-fixture-SENTINEL0123456789"
SECRET_TOKEN="ghs_SENTINELtoken0123456789"

cat > "$FAKE_BIN/git" <<FAKE
#!/usr/bin/env bash
args=("\$@")
i=0
while [ "\$i" -lt "\${#args[@]}" ]; do
  case "\${args[\$i]}" in
    -C|-c) i=\$((i + 2)) ;;
    -*) i=\$((i + 1)) ;;
    *) break ;;
  esac
done
case "\${args[\$i]:-}" in
  pull|fetch|commit|push)
    echo "git \${args[\$i]} \$*" >> "$LOG"
    echo "fake git: \${args[\$i]} is not allowed in a release script" >&2
    exit 97
    ;;
esac
exec "$REAL_GIT" "\$@"
FAKE
cat > "$FAKE_BIN/go" <<FAKE
#!/usr/bin/env bash
case "\$1 \${2:-}" in
  "mod edit") GOTOOLCHAIN=local GOFLAGS= GOPROXY=off exec "$REAL_GO" "\$@" ;;
  "mod tidy"|"mod download"|"get "*)
    echo "go \$*" >> "$LOG"
    echo "fake go: \$* resolves modules from the network" >&2
    exit 97
    ;;
esac
echo "go \$* (in \$PWD)" >> "$LOG"
FAKE
cat > "$FAKE_BIN/make" <<FAKE
#!/usr/bin/env bash
echo "make \$* (in \$PWD)" >> "$LOG"
FAKE
chmod +x "$FAKE_BIN/git" "$FAKE_BIN/go" "$FAKE_BIN/make"

FAILURES=0
fail() {
  echo "FAIL: $*" >&2
  FAILURES=$((FAILURES + 1))
}

# The commit date the docs pages must carry: far from today, so a wall-clock date shows.
COMMIT_DATE="2001-02-03T04:05:06Z"
MODULE="github.com/capsohq/bifrost"

# write_tree <dir> <governance version>: a release tree whose transports/go.mod requires
# governance v1.7.2, so any other governance version file is drift.
write_tree() {
  local d="$1"
  mkdir -p "$d/core" "$d/framework" "$d/plugins/governance" "$d/plugins/litellmcompat" \
    "$d/transports" "$d/cli" "$d/docs/changelogs" "$d/examples/plugins/hello-world"
  printf 'module %s/core\n\ngo 1.27.2\n' "$MODULE" > "$d/core/go.mod"
  printf 'build:\n' > "$d/examples/plugins/hello-world/Makefile"
  echo "1.8.6" > "$d/core/version"
  echo "1.6.2" > "$d/framework/version"
  echo "$2" > "$d/plugins/governance/version"
  echo "2.1.2" > "$d/transports/version"
  echo "0.10.6" > "$d/cli/version"
  : > "$d/core/changelog.md"
  : > "$d/framework/changelog.md"
  printf -- '- feat: governance budgets\n' > "$d/plugins/governance/changelog.md"
  printf -- '- fix: transport retries\n' > "$d/transports/changelog.md"
  printf -- '- feat: cli login\n' > "$d/cli/changelog.md"
  cat > "$d/transports/go.mod" <<GOMOD
module $MODULE/transports

go 1.27.2

require (
	$MODULE/core v1.8.6
	$MODULE/framework v1.6.2
	$MODULE/plugins/governance v1.7.2
	$MODULE/plugins/litellmcompat v0.0.1
	github.com/google/uuid v1.6.0
)
GOMOD
  printf -- '---\ntitle: "v2.1.1"\ndescription: "v2.1.1 changelog - 2001-02-01"\n---\n' > "$d/docs/changelogs/v2.1.1.mdx"
  printf -- '---\ntitle: "v0.10.5"\ndescription: "v0.10.5 changelog - 2001-02-01"\n---\n' > "$d/docs/changelogs/cli-v0.10.5.mdx"
  cat > "$d/docs/docs.json" <<'JSON'
{
  "navigation": {
    "tabs": [
      {
        "tab": "Changelogs",
        "menu": [
          { "item": "Open Source", "pages": ["changelogs/v2.1.1"] },
          { "item": "Bifrost CLI", "pages": ["changelogs/cli-v0.10.5"] }
        ]
      }
    ]
  }
}
JSON
}

# make_origin <name> <governance version> [go.mod line]: a bare origin with main at the
# release tree, then one more commit, then the protection hook. Prints the release
# commit; the clone at $WORK/<name> is checked out there, one commit behind origin's main.
make_origin() {
  local name="$1" seed="$WORK/$1-seed" origin="$WORK/$1.git" release
  write_tree "$seed" "$2"
  if [ -n "${3:-}" ]; then
    printf '\n%s\n' "$3" >> "$seed/transports/go.mod"
  fi
  (
    cd "$seed" || exit 1
    "$REAL_GIT" init -q -b main
    "$REAL_GIT" add -A
    GIT_AUTHOR_DATE="$COMMIT_DATE" GIT_COMMITTER_DATE="$COMMIT_DATE" \
      "$REAL_GIT" -c user.name=fixture -c user.email=fixture@example.invalid commit -q -m release
    "$REAL_GIT" init -q --bare "$origin"
    "$REAL_GIT" push -q "$origin" main
  ) || return 1
  release="$("$REAL_GIT" -C "$seed" rev-parse HEAD)"
  "$REAL_GIT" clone -q "$origin" "$WORK/$name" || return 1
  (
    cd "$seed" || exit 1
    echo "later" > later.txt
    "$REAL_GIT" add later.txt
    "$REAL_GIT" -c user.name=fixture -c user.email=fixture@example.invalid commit -q -m later
    "$REAL_GIT" push -q "$origin" main
  ) || return 1
  cat > "$origin/hooks/pre-receive" <<HOOK
#!/usr/bin/env bash
while read -r old new ref; do
  echo "\$ref \$old \$new" >> "$WORK/ref-updates.log"
  if [ "\$ref" = refs/heads/main ]; then
    echo "protected branch hook declined: main requires a reviewed check" >&2
    exit 1
  fi
done
HOOK
  chmod +x "$origin/hooks/pre-receive"
  "$REAL_GIT" -C "$WORK/$name" checkout -q --detach "$release" || return 1
  echo "$release"
}

# run_script <checkout> <script> <args...>: runs it as the release job would, with the
# job's environment and the sentinels. Sets STATUS, OUT and SUMMARY.
run_script() {
  local checkout="$1" script="$2"
  shift 2
  RUN="$WORK/run-$RANDOM$RANDOM"
  mkdir -p "$RUN/runner-temp"
  : > "$LOG"
  OUT="$RUN/out"
  SUMMARY="$RUN/summary.md"
  STATE_BEFORE="$("$REAL_GIT" -C "$checkout" rev-parse HEAD) $("$REAL_GIT" -C "$checkout" for-each-ref | "$REAL_GIT" hash-object --stdin)"
  (
    cd "$checkout" || exit 1
    env PATH="$FAKE_BIN:$PATH" GITHUB_SHA="${FIXTURE_SHA:-$("$REAL_GIT" rev-parse HEAD)}" \
      RUNNER_TEMP="$RUN/runner-temp" GITHUB_STEP_SUMMARY="$SUMMARY" GITHUB_OUTPUT="$RUN/output" \
      GITHUB_REF_NAME=main OPENAI_API_KEY="$SECRET_KEY" GH_TOKEN="$SECRET_TOKEN" \
      bash "$SCRIPTS/$script" "$@"
  ) > "$OUT" 2>&1
  STATUS=$?
  touch "$SUMMARY" "$RUN/output"
}

# Checks shared by every scenario: no ref moved anywhere, no forbidden command ran, and
# no sentinel leaked.
assert_no_writes() {
  local label="$1" checkout="$2"
  local state_after
  state_after="$("$REAL_GIT" -C "$checkout" rev-parse HEAD) $("$REAL_GIT" -C "$checkout" for-each-ref | "$REAL_GIT" hash-object --stdin)"
  [ "$state_after" = "$STATE_BEFORE" ] || fail "$label: HEAD or a ref in the checkout moved"
  [ ! -s "$WORK/ref-updates.log" ] || fail "$label: origin received a ref update: $(cat "$WORK/ref-updates.log")"
  if grep -E '^(git (pull|fetch|commit|push)|go (mod tidy|mod download|get))' "$LOG" >/dev/null; then
    fail "$label: forbidden command: $(grep -E '^(git|go) ' "$LOG" | head -3)"
  fi
  if grep -rl -e "$SECRET_KEY" -e "$SECRET_TOKEN" "$RUN" >/dev/null 2>&1; then
    fail "$label: a sentinel secret reached $(grep -rl -e "$SECRET_KEY" -e "$SECRET_TOKEN" "$RUN" | head -1)"
  fi
}

# patch_paths <patch>: the paths it touches, sorted. Run outside any repository: inside
# one, git apply ignores paths outside the current directory.
patch_paths() {
  (cd "$WORK" && "$REAL_GIT" apply --numstat "$1") | cut -f3 | sort | tr '\n' ' '
}

# fresh_clone <origin name> <commit>: a clean clone at the commit, for applying a patch.
fresh_clone() {
  local dir="$WORK/apply-$RANDOM$RANDOM"
  "$REAL_GIT" clone -q "$WORK/$1.git" "$dir" && "$REAL_GIT" -C "$dir" checkout -q --detach "$2" && echo "$dir"
}

SHA="$(make_origin clean 1.7.2)" || { echo "FAIL: could not build the clean fixture" >&2; exit 1; }
SHA12="${SHA:0:12}"

# --- prep, no drift: verified, built from the checkout, nothing changed ----------------
run_script "$WORK/clean" release-bifrost-http-prep.sh 2.1.2
label="prep without drift"
[ "$STATUS" -eq 0 ] || fail "$label: exit $STATUS: $(tail -5 "$OUT")"
grep -qx 'success=true' "$RUN/output" || fail "$label: no success=true output"
[ -z "$(find "$RUN/runner-temp" -name '*.patch')" ] || fail "$label: wrote a patch"
grep -q '^make build-ui ' "$LOG" || fail "$label: did not build the UI"
grep -q '^make build (in .*/examples/plugins/hello-world)' "$LOG" || fail "$label: did not build the hello-world plugin"
grep -q '^go build -mod=readonly ./\.\.\. (in .*/transports)' "$LOG" || fail "$label: did not build the transport read-only"
[ -z "$("$REAL_GIT" -C "$WORK/clean" status --porcelain)" ] || fail "$label: the checkout is dirty"
grep -q 'release prep: verified' "$SUMMARY" || fail "$label: summary does not say verified"
assert_no_writes "$label" "$WORK/clean"

# --- prep against a checkout that is not the tested commit -----------------------------
FIXTURE_SHA="0123456789abcdef0123456789abcdef01234567" run_script "$WORK/clean" release-bifrost-http-prep.sh 2.1.2
label="prep on an untested checkout"
[ "$STATUS" -ne 0 ] || fail "$label: exit 0"
grep -q 'is not\|but this run tested' "$OUT" || fail "$label: no checkout mismatch error"
grep -q '^make ' "$LOG" && fail "$label: built anyway"
assert_no_writes "$label" "$WORK/clean"

# --- prep with drift: a pending patch for go.mod alone, and nothing built --------------
# Governance's version file moved on, and a toolchain line crept in.
DRIFT_SHA="$(make_origin drift 1.7.3 'toolchain go1.27.3')" || { echo "FAIL: could not build the drift fixture" >&2; exit 1; }
run_script "$WORK/drift" release-bifrost-http-prep.sh 2.1.2
label="prep with drift"
PATCH="$RUN/runner-temp/release-patches/release-prep-v2.1.2-${DRIFT_SHA:0:12}.patch"
[ "$STATUS" -ne 0 ] || fail "$label: exit 0"
grep -q '::error::release prep pending' "$OUT" || fail "$label: no pending error"
grep -q 'pending reviewed PR' "$SUMMARY" || fail "$label: summary does not say pending"
grep -q 'success=true' "$RUN/output" && fail "$label: reported success"
grep -q '^make ' "$LOG" && fail "$label: built before the change was reviewed"
grep -q '^go build' "$LOG" && fail "$label: built before the change was reviewed"
if [ -f "$PATCH" ]; then
  [ "$(patch_paths "$PATCH")" = "transports/go.mod " ] || fail "$label: patch touches $(patch_paths "$PATCH")"
  APPLIED="$(fresh_clone drift "$DRIFT_SHA")"
  if "$REAL_GIT" -C "$APPLIED" apply "$PATCH"; then
    grep -q "$MODULE/plugins/governance v1.7.3" "$APPLIED/transports/go.mod" || fail "$label: patch does not bump governance"
    grep -q '^toolchain' "$APPLIED/transports/go.mod" && fail "$label: patch keeps the toolchain line"
    grep -q "$MODULE/plugins/litellmcompat v0.0.1" "$APPLIED/transports/go.mod" || fail "$label: patch moved a module without a version file"
    grep -q 'github.com/google/uuid v1.6.0' "$APPLIED/transports/go.mod" || fail "$label: patch moved an external module"
  else
    fail "$label: patch does not apply to the tested commit"
  fi
else
  fail "$label: no patch at $PATCH"
fi
[ -z "$("$REAL_GIT" -C "$WORK/drift" status --porcelain)" ] || fail "$label: the checkout was changed"
assert_no_writes "$label" "$WORK/drift"

# --- prep refuses a patch directory inside the checkout ---------------------------------
RELEASE_PATCH_DIR="$WORK/drift/patches" run_script "$WORK/drift" release-bifrost-http-prep.sh 2.1.2
label="prep patch directory in the checkout"
[ "$STATUS" -ne 0 ] || fail "$label: exit 0"
grep -q 'inside the checkout' "$OUT" || fail "$label: no inside-the-checkout error"
[ -z "$(find "$WORK/drift/patches" -name '*.patch' 2>/dev/null)" ] || fail "$label: wrote a patch into the checkout"
rm -rf "$WORK/drift/patches"

# --- docs changelog: a pending patch inside the allowlist, dated from the commit --------
run_script "$WORK/clean" push-mintlify-changelog.sh 2.1.2
label="docs changelog"
PATCH="$RUN/runner-temp/release-patches/docs-changelog-v2.1.2-$SHA12.patch"
[ "$STATUS" -eq 0 ] || fail "$label: exit $STATUS: $(tail -5 "$OUT")"
grep -q 'pending, not published' "$SUMMARY" || fail "$label: summary does not say pending, not published"
grep -q '::warning::docs changelog v2.1.2 is pending' "$OUT" || fail "$label: no pending warning"
FIRST_PATCH="$WORK/first-docs.patch"
if [ -f "$PATCH" ]; then
  cp "$PATCH" "$FIRST_PATCH"
  expected="docs/changelogs/v2.1.2.mdx docs/docs.json plugins/governance/changelog.md transports/changelog.md "
  [ "$(patch_paths "$PATCH")" = "$expected" ] || fail "$label: patch touches $(patch_paths "$PATCH")"
  APPLIED="$(fresh_clone clean "$SHA")"
  if "$REAL_GIT" -C "$APPLIED" apply "$PATCH"; then
    grep -q 'v2.1.2 changelog - 2001-02-03' "$APPLIED/docs/changelogs/v2.1.2.mdx" || fail "$label: page is not dated from the commit"
    grep -q 'transport retries' "$APPLIED/docs/changelogs/v2.1.2.mdx" || fail "$label: page lacks the transport notes"
    [ ! -s "$APPLIED/transports/changelog.md" ] || fail "$label: transports/changelog.md is not cleared"
    jq -e '.navigation.tabs[0].menu[0].pages[0] == "changelogs/v2.1.2"' "$APPLIED/docs/docs.json" >/dev/null || fail "$label: docs.json does not list the page"
  else
    fail "$label: patch does not apply to the tested commit"
  fi
else
  fail "$label: no patch at $PATCH"
fi
assert_no_writes "$label" "$WORK/clean"

# --- docs changelog rerun for the same commit and version: the same bytes ---------------
"$REAL_GIT" -C "$WORK/clean" checkout -q -- . && "$REAL_GIT" -C "$WORK/clean" clean -qfd
run_script "$WORK/clean" push-mintlify-changelog.sh 2.1.2
label="docs changelog rerun"
PATCH="$RUN/runner-temp/release-patches/docs-changelog-v2.1.2-$SHA12.patch"
[ "$STATUS" -eq 0 ] || fail "$label: exit $STATUS"
cmp -s "$FIRST_PATCH" "$PATCH" || fail "$label: the patch differs from the first run"
assert_no_writes "$label" "$WORK/clean"
"$REAL_GIT" -C "$WORK/clean" checkout -q -- . && "$REAL_GIT" -C "$WORK/clean" clean -qfd

# --- docs changelog refuses a checkout dirty outside, or inside, the allowlist ----------
for dirty in stray.txt transports/go.mod docs/docs.json; do
  echo "dirty" >> "$WORK/clean/$dirty"
  run_script "$WORK/clean" push-mintlify-changelog.sh 2.1.2
  label="docs changelog with $dirty dirty"
  [ "$STATUS" -ne 0 ] || fail "$label: exit 0"
  [ -z "$(find "$RUN/runner-temp" -name '*.patch')" ] || fail "$label: wrote a patch"
  assert_no_writes "$label" "$WORK/clean"
  "$REAL_GIT" -C "$WORK/clean" checkout -q -- . && "$REAL_GIT" -C "$WORK/clean" clean -qfd
done

# --- CLI docs changelog: the same contract --------------------------------------------
run_script "$WORK/clean" push-cli-mintlify-changelog.sh 0.10.6
label="CLI docs changelog"
PATCH="$RUN/runner-temp/release-patches/cli-docs-changelog-v0.10.6-$SHA12.patch"
[ "$STATUS" -eq 0 ] || fail "$label: exit $STATUS: $(tail -5 "$OUT")"
grep -q 'pending, not published' "$SUMMARY" || fail "$label: summary does not say pending, not published"
if [ -f "$PATCH" ]; then
  expected="cli/changelog.md docs/changelogs/cli-v0.10.6.mdx docs/docs.json "
  [ "$(patch_paths "$PATCH")" = "$expected" ] || fail "$label: patch touches $(patch_paths "$PATCH")"
  APPLIED="$(fresh_clone clean "$SHA")"
  if "$REAL_GIT" -C "$APPLIED" apply "$PATCH"; then
    grep -q 'v0.10.6 changelog - 2001-02-03' "$APPLIED/docs/changelogs/cli-v0.10.6.mdx" || fail "$label: page is not dated from the commit"
  else
    fail "$label: patch does not apply to the tested commit"
  fi
else
  fail "$label: no patch at $PATCH"
fi
assert_no_writes "$label" "$WORK/clean"

if [ "$FAILURES" -gt 0 ]; then
  echo "$FAILURES failure(s)" >&2
  exit 1
fi
echo "PASS: release scripts leave main to reviewed PRs"
