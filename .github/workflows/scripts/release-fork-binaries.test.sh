#!/usr/bin/env bash
set -uo pipefail

# Fixture tests for release-fork-binaries.sh. A local bare repository stands in for
# origin, stubs stand in for gh and go, and state files record what gh was asked to do.
# Nothing here reaches the network.
#
# Usage: ./release-fork-binaries.test.sh

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
ROOT="$(mktemp -d)"
trap 'rm -rf "$ROOT"' EXIT
export GIT_CONFIG_GLOBAL="$ROOT/gitconfig" GIT_CONFIG_NOSYSTEM=1
git config --global user.name fixture
git config --global user.email fixture@example.invalid
git config --global init.defaultBranch main

passed=0
failed=0
ok() {
  echo "  ok - $1"
  passed=$((passed + 1))
}
bad() {
  echo "  FAIL - $1" >&2
  failed=$((failed + 1))
}

mkdir -p "$ROOT/bin"
cat >"$ROOT/bin/go" <<'STUB'
#!/usr/bin/env bash
# `go build ... -o <path> .`: writes a binary that prints what the ldflags gave it.
while [ $# -gt 0 ]; do
  case "$1" in
    -ldflags) flags="$2"; shift ;;
    -o) out="$2"; shift ;;
  esac
  shift
done
version="$(sed -n 's/.*main.version=\([^ ]*\).*/\1/p' <<<"$flags")"
commit="$(sed -n 's/.*main.commit=\([^ ]*\).*/\1/p' <<<"$flags")"
channel="$(sed -n 's/.*releaseChannel=\([^ ]*\).*/\1/p' <<<"$flags")"
cat >"$out" <<BIN
#!/usr/bin/env bash
case "\$1" in
  version) echo "bifrost $version ($commit)" ;;
  update) [ "$channel" = txehq ] && { echo "see https://github.com/txehq/bifrost/releases" >&2; exit 1; }; exit 0 ;;
esac
BIN
chmod +x "$out"
STUB
cat >"$ROOT/bin/gh" <<'STUB'
#!/usr/bin/env bash
# State: $GH_STATE/release (id draft target tag), $GH_STATE/assets/, $GH_STATE/log.
echo "gh $*" >>"$GH_STATE/log"
case "$1 $2" in
  "api repos/txehq/bifrost/releases")
    [ -f "$GH_STATE/release" ] && read -r id draft target tag <"$GH_STATE/release" && [ "$tag" = "$FIXTURE_TAG" ] && echo "$id $draft $target"
    exit 0 ;;
  "api repos/txehq/bifrost/releases/latest") cat "$GH_STATE/latest"; exit 0 ;;
  "api -X")
    [ "${GH_FAIL:-}" = delete ] && exit 1
    rm -f "$GH_STATE/release"; rm -rf "$GH_STATE/assets"; exit 0 ;;
  "release create")
    tag="$3"; shift 3
    mkdir -p "$GH_STATE/assets"
    while [ $# -gt 0 ]; do
      case "$1" in
        --target) target="$2"; shift ;;
        --repo|--title|--notes-file) shift ;;
        --*) ;;
        *) cp "$1" "$GH_STATE/assets/" ;;
      esac
      shift
    done
    [ "${GH_FAIL:-}" = upload ] && { rm "$GH_STATE/assets/SHA256SUMS"; }
    echo "101 true $target $tag" >"$GH_STATE/release"
    exit 0 ;;
  "release download")
    dir=""; while [ $# -gt 0 ]; do [ "$1" = --dir ] && dir="$2"; shift; done
    cp "$GH_STATE/assets/"* "$dir/"
    [ "${GH_FAIL:-}" = corrupt ] && echo x >>"$dir/SHA256SUMS"
    exit 0 ;;
  "release edit")
    [ "${GH_FAIL:-}" = latest ] && echo stolen >"$GH_STATE/latest"
    read -r id draft target tag <"$GH_STATE/release"
    echo "$id false $target $tag" >"$GH_STATE/release"
    git -C "$FIXTURE_ORIGIN" tag "$tag" "$target"
    exit 0 ;;
esac
echo "unexpected gh $*" >&2
exit 1
STUB
chmod +x "$ROOT/bin/go" "$ROOT/bin/gh"

# Builds origin and a checkout holding the real scripts, at the given version.
setup() {
  rm -rf "$ROOT/origin.git" "$ROOT/work" "$ROOT/state"
  mkdir -p "$ROOT/state"
  git init -q --bare "$ROOT/origin.git"
  git init -q "$ROOT/work"
  local w="$ROOT/work" s=".github/workflows/scripts"
  mkdir -p "$w/$s" "$w/cli" "$w/scripts/bifrost-migration-cli"
  cp "$SCRIPT_DIR/release-fork-binaries.sh" "$SCRIPT_DIR/build-cli-executables.sh" \
    "$SCRIPT_DIR/build-bifrost-migration-cli-executables.sh" "$w/$s/"
  echo "1.0.1-sync.1" >"$w/cli/version"
  echo "- fix: something" >"$w/cli/changelog.md"
  echo "0.1.1-sync.1" >"$w/scripts/bifrost-migration-cli/version"
  echo "- feat: something" >"$w/scripts/bifrost-migration-cli/changelog.md"
  echo dist >"$w/.gitignore"
  git -C "$w" add -A
  git -C "$w" commit -qm base
  git -C "$w" remote add origin "$ROOT/origin.git"
  git -C "$w" push -q origin HEAD:main
  SHA="$(git -C "$w" rev-parse HEAD)"
  echo "transports/v2.3.2" >"$ROOT/state/latest"
}

# run <action> [component] [version]: runs the real script under the fixture.
run() {
  local component="${2:-cli}" version="${3:-1.0.1-sync.1}" prefix="cli"
  [ "$component" = migration ] && prefix="bifrost-migration-cli"
  (
    cd "$ROOT/work" &&
      env PATH="$ROOT/bin:$PATH" GH_STATE="$ROOT/state" FIXTURE_ORIGIN="$ROOT/origin.git" \
        FIXTURE_TAG="$prefix/v$version" GITHUB_REPOSITORY="${REPO:-txehq/bifrost}" GITHUB_SHA="${RUN_SHA:-$SHA}" \
        GH_TOKEN=fixture GITHUB_OUTPUT="$ROOT/state/output" \
        bash "$ROOT/work/.github/workflows/scripts/release-fork-binaries.sh" "$1" "$component" "$version"
  ) >"$ROOT/out" 2>&1
}

expect_refused() {
  local label="$1" text="$2"
  shift 2
  if run "$@"; then
    bad "$label: succeeded"
  elif ! grep -qF "$text" "$ROOT/out"; then
    bad "$label: wrong refusal: $(tail -1 "$ROOT/out")"
  elif git -C "$ROOT/origin.git" tag | grep -q .; then
    bad "$label: left a tag"
  else
    ok "$label"
  fi
}

echo "publish then verify, both components"
for component in cli migration; do
  setup
  version="1.0.1-sync.1"
  [ "$component" = migration ] && version="0.1.1-sync.1"
  if run publish "$component" "$version" && run verify "$component" "$version"; then
    ok "$component: published and verified"
  else
    bad "$component: $(tail -3 "$ROOT/out")"
  fi
  tag="$(git -C "$ROOT/origin.git" tag)"
  [ "$(git -C "$ROOT/origin.git" rev-parse "$tag^{commit}")" = "$SHA" ] && ok "$component: tag at the pushed commit" || bad "$component: tag $tag"
  [ "$(cat "$ROOT/state/latest")" = "transports/v2.3.2" ] && ok "$component: GH latest unchanged" || bad "$component: latest moved"
  grep -q 'release create.*--draft.*--latest=false.*--prerelease' "$ROOT/state/log" && ok "$component: draft, not latest, prerelease" || bad "$component: flags"
  [ "$(ls "$ROOT/state/assets" | wc -l | tr -d ' ')" = 6 ] && ok "$component: 5 binaries and SHA256SUMS" || bad "$component: assets"
done

echo "a stable version is still not latest"
setup
echo "1.0.1" >"$ROOT/work/cli/version"
git -C "$ROOT/work" commit -qam stable
git -C "$ROOT/work" push -q origin HEAD:main
SHA="$(git -C "$ROOT/work" rev-parse HEAD)"
run publish cli 1.0.1 && grep -q 'release create.*--latest=false' "$ROOT/state/log" && ! grep -q 'prerelease' "$ROOT/state/log" && ok "stable: --latest=false, not prerelease" || bad "stable: $(tail -1 "$ROOT/out")"

echo "refusals"
setup
REPO=maximhq/bifrost expect_refused "upstream repository" "only txehq/bifrost" publish
REPO=someone/bifrost expect_refused "other fork" "only txehq/bifrost" publish
setup
RUN_SHA=0000000000000000000000000000000000000000 expect_refused "checkout is not the pushed commit" "is not the pushed commit" publish
setup
echo "1.0.2" >"$ROOT/work/cli/version"
expect_refused "local changes" "local changes" publish
setup
git -C "$ROOT/work" commit -q --allow-empty -m unmerged
SHA="$(git -C "$ROOT/work" rev-parse HEAD)"
expect_refused "commit not on main" "is not on main" publish
setup
expect_refused "version argument differs from the file" "cli/version says" publish cli 1.0.2-sync.1
setup
: >"$ROOT/work/cli/changelog.md"
git -C "$ROOT/work" commit -qam empty
git -C "$ROOT/work" push -q origin HEAD:main
SHA="$(git -C "$ROOT/work" rev-parse HEAD)"
expect_refused "empty changelog" "is empty" publish
setup
expect_refused "bad version" "invalid version" publish cli '1.0.1;id'

echo "immutability"
for at in same other; do
  setup
  target="$SHA"
  if [ "$at" = other ]; then
    git -C "$ROOT/work" commit -q --allow-empty -m next
    git -C "$ROOT/work" push -q origin HEAD:main
    target="$SHA"
    SHA="$(git -C "$ROOT/work" rev-parse HEAD)"
  fi
  git -C "$ROOT/origin.git" tag cli/v1.0.1-sync.1 "$target"
  if run publish; then bad "existing tag ($at commit): succeeded"; elif grep -qF "versions are immutable" "$ROOT/out"; then ok "existing tag ($at commit)"; else bad "existing tag ($at): $(tail -1 "$ROOT/out")"; fi
  [ "$(git -C "$ROOT/origin.git" rev-parse "cli/v1.0.1-sync.1^{commit}")" = "$target" ] || bad "tag moved"
  ! grep -q 'release create' "$ROOT/state/log" 2>/dev/null || bad "created over an existing tag"
done
setup
echo "9 false $SHA cli/v1.0.1-sync.1" >"$ROOT/state/release"
expect_refused "published release, no tag" "already published" publish
setup
echo "9 true deadbeef cli/v1.0.1-sync.1" >"$ROOT/state/release"
expect_refused "another commit's draft" "draft cli/v1.0.1-sync.1 targets deadbeef" publish
setup
echo "9 true $SHA cli/v1.0.1-sync.1" >"$ROOT/state/release"
run publish && grep -q 'api -X DELETE repos/txehq/bifrost/releases/9' "$ROOT/state/log" && ok "this commit's draft is replaced" || bad "own draft: $(tail -1 "$ROOT/out")"

echo "a failed publish leaves no tag and an unpublished draft"
setup
GH_FAIL=corrupt expect_refused "read-back differs" "differ from the build" publish
grep -q 'release edit' "$ROOT/state/log" && bad "published after a bad read-back" || ok "not published after a bad read-back"
setup
GH_FAIL=upload expect_refused "missing asset" "does not hold 6 assets" publish
setup
if GH_FAIL=latest run publish; then bad "moved latest: succeeded"; elif grep -qF "GH latest moved" "$ROOT/out"; then ok "moved latest is reported"; else bad "moved latest: $(tail -1 "$ROOT/out")"; fi

echo "verify refuses what it should"
setup
expect_refused "no release" "no release" verify
setup
run publish
sed -i.bak "s/.*/101 true $SHA cli\/v1.0.1-sync.1/" "$ROOT/state/release"
if run verify; then bad "draft verified"; elif grep -qF "still a draft" "$ROOT/out"; then ok "draft refused"; else bad "draft: $(tail -1 "$ROOT/out")"; fi
setup
run publish
: >"$ROOT/state/sentinel"
f="$(ls "$ROOT/state/assets/"*linux-*64 | head -1)"
echo tampered >>"$f"
if run verify; then bad "tampered asset verified"; else ok "tampered asset refused"; fi
setup
run publish
echo "cli/v1.0.1-sync.1" >"$ROOT/state/latest"
if run verify; then bad "verify passed while holding latest"; elif grep -qF "took GH latest" "$ROOT/out"; then ok "holding GH latest refused"; else bad "latest: $(tail -1 "$ROOT/out")"; fi

echo "a build without the fork channel is never published"
setup
sed -i.bak 's/CLI_RELEASE_CHANNEL=txehq //' "$ROOT/work/.github/workflows/scripts/release-fork-binaries.sh"
rm "$ROOT/work/.github/workflows/scripts/release-fork-binaries.sh.bak"
git -C "$ROOT/work" commit -qam nochannel
git -C "$ROOT/work" push -q origin HEAD:main
SHA="$(git -C "$ROOT/work" rev-parse HEAD)"
expect_refused "missing channel at publish" "update command succeeded" publish
grep -q 'release create' "$ROOT/state/log" && bad "uploaded a non-fork build" || ok "no draft for a non-fork build"

echo "verify checks the published binary"
replace_asset() { # body
  local f
  f="$(ls "$ROOT/state/assets/"*linux-*64 | grep "$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')")"
  printf '%s\n' '#!/usr/bin/env bash' "$1" >"$f"
  (cd "$ROOT/state/assets" && rm SHA256SUMS && sha256sum -- * >/tmp/sums.$$ && mv /tmp/sums.$$ SHA256SUMS)
}
setup
run publish
replace_asset "case \"\$1\" in version) echo \"bifrost v1.0.1-sync.1 ($SHA)\" ;; update) exit 0 ;; esac"
if run verify; then bad "self-updating binary verified"; elif grep -qF "update command succeeded" "$ROOT/out"; then ok "self-updating binary refused"; else bad "self-update: $(tail -1 "$ROOT/out")"; fi
setup
run publish
replace_asset "case \"\$1\" in version) echo \"bifrost v1.0.1-sync.1 ($SHA)\" ;; update) echo no route to host >&2; exit 1 ;; esac"
if run verify; then bad "upstream binary failing offline verified"; elif grep -qF "not a fork build" "$ROOT/out"; then ok "upstream binary failing offline refused"; else bad "offline: $(tail -1 "$ROOT/out")"; fi
setup
run publish
replace_asset "echo \"bifrost v9.9.9 ($SHA)\""
if run verify; then bad "wrong version verified"; elif grep -qF "does not report" "$ROOT/out"; then ok "wrong version refused"; else bad "version: $(tail -1 "$ROOT/out")"; fi
setup
run publish
replace_asset "echo \"bifrost v1.0.1-sync.1 (0000)\""
if run verify; then bad "wrong commit verified"; elif grep -qF "does not report" "$ROOT/out"; then ok "wrong commit refused"; else bad "commit: $(tail -1 "$ROOT/out")"; fi
setup
run publish
if RUN_SHA=1111111111111111111111111111111111111111 run verify; then bad "another commit verified"; elif grep -qF "targets $SHA" "$ROOT/out"; then ok "verify for another commit refused"; else bad "other commit: $(tail -1 "$ROOT/out")"; fi

echo "the fork release script never touches npm or R2"
if grep -nE '\bnpm\b|\baws\b|R2_' "$SCRIPT_DIR/release-fork-binaries.sh" | grep -v '^[0-9]*:#' | grep -v 'npm packages'; then bad "npm or R2 use"; else ok "no npm or R2 use"; fi

echo ""
echo "$passed passed, $failed failed"
[ "$failed" -eq 0 ]
