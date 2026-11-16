#!/usr/bin/env bash
# Decides whether this Release Pipeline run may release anything (TXE-7511).
#
# A push releases only when the push itself changes a version file: core/version,
# framework/version, transports/version or plugins/<name>/version. Every other push
# releases nothing, so its run reaches no live-providers job and asks nobody for an
# approval. A workflow_dispatch always may release: it is how a person releases on
# purpose, and how a release that failed is retried (docs/release.md).
#
# Before this, detect-all-changes.sh released whenever a version's tag was missing, so
# once a version bump merged, every later merge re-attempted that release from a commit
# nobody chose, and each attempt waited on Connor's live-providers approval (sync.11).
#
# A push whose range cannot be read (a new branch, a force push whose old head is gone)
# fails closed: it releases nothing and says so. Dispatch the release instead.
#
# Usage: release-trigger.sh <event-name> <before-sha> <after-sha>
# Prints may-release=true|false, and appends it to $GITHUB_OUTPUT when that is set.
set -euo pipefail

event="${1:?event name}"
before="${2:-}"
after="${3:?after sha}"

decide() {
  echo "may-release=$1"
  if [ -n "${GITHUB_OUTPUT:-}" ]; then
    echo "may-release=$1" >> "$GITHUB_OUTPUT"
  fi
  exit 0
}

case "$event" in
  workflow_dispatch)
    echo "A dispatch may release: it names the commit on purpose."
    decide true
    ;;
  push) ;;
  *)
    echo "::warning::Release Pipeline started by '$event', which never releases; dispatch the workflow to release."
    decide false
    ;;
esac

if [[ ! "$before" =~ ^[0-9a-f]{40}$ ]] || [[ "$before" =~ ^0+$ ]]; then
  echo "::warning::This push has no previous head ('$before'), so it releases nothing. To release, dispatch the Release Pipeline (docs/release.md)."
  decide false
fi
if ! changed=$(git diff --name-only "$before" "$after" 2>/dev/null); then
  echo "::warning::Could not diff $before..$after (a force push?), so this push releases nothing. To release, dispatch the Release Pipeline (docs/release.md)."
  decide false
fi

versions=$(grep -E '^(core|framework|transports)/version$|^plugins/[^/]+/version$' <<<"$changed" || true)
if [ -n "$versions" ]; then
  echo "This push changes a version file, so it may release:"
  while IFS= read -r f; do echo "  $f"; done <<<"$versions"
  decide true
fi
echo "This push changes no version file, so it releases nothing and asks for no live-providers approval."
echo "A release that failed is retried with: gh run rerun <run-id> --failed -R txehq/bifrost (docs/release.md)."
decide false
