#!/usr/bin/env bash
# Sourced by detect-all-changes.sh. Decides which plugins a release may publish as Go
# modules. test-release-gates.py runs drop_unpublishable_plugins against a scratch git
# repository, so keep this file free of network calls.

# Plugin names travel the same way and by a sharper edge: they are collected into the
# changed-plugins JSON and interpolated as '${{ ... }}' inside single quotes at the
# release-all-plugins.sh call site. jq -R escapes double quotes and backslashes when
# building that JSON but leaves single quotes alone, so a name containing one closes
# the shell string. A plugin name is a directory name - it has no business containing
# anything but the characters below.
validate_plugin_name() {
  local value="$1"
  case "$value" in
    "")
      echo "::error::refusing empty plugin name" >&2
      return 1
      ;;
    *[!A-Za-z0-9._-]*)
      echo "::error::refusing plugin name with unexpected characters: '$value'" >&2
      return 1
      ;;
    *..*)
      echo "::error::refusing plugin name containing '..': '$value'" >&2
      return 1
      ;;
  esac
}

# module_published <tag> <due>: true when <tag> exists, or when <due> is "true" because
# this same run releases that module before the plugins.
module_published() {
  local tag="$1" due="$2"
  [ "$due" = "true" ] || git rev-parse --verify --quiet "refs/tags/$tag" >/dev/null
}

# drop_unpublishable_plugins: filters the PLUGIN_CHANGES array in place. Reads
# CORE_VERSION, FRAMEWORK_VERSION, CORE_NEEDS_RELEASE and FRAMEWORK_NEEDS_RELEASE, and
# runs from the repository root.
#
# A plugin release (release-single-plugin.sh) pins the plugin's go.mod to core and
# framework at their version files' versions, and every bifrost plugin it requires at
# that plugin's version file, then runs `go mod tidy` against the module proxy. Each of
# those modules has to be published: tagged already, or released by this same run
# (core-release and framework-release finish before plugins-release starts, and a
# plugin's bifrost dependencies are released in the same loop). This fork builds the
# gateway and its images from go.work and publishes no core or framework module, so a
# plugin whose changelog gained a line failed its release on an unknown revision and took
# the transport release and every image down with it (sync.13, TXE-7907). Such a plugin
# is left out with a notice; the gateway still ships its code from the workspace.
drop_unpublishable_plugins() {
  [ ${#PLUGIN_CHANGES[@]} -gt 0 ] || return 0

  local module_root plugin_name dependency dependency_version other selected missing dropped
  module_root=$(sed -n 's#^module \(.*\)/core$#\1#p' core/go.mod)
  if [ -z "$module_root" ]; then
    echo "::error::cannot read the module root from core/go.mod's module line" >&2
    return 1
  fi

  if ! module_published "core/v${CORE_VERSION}" "$CORE_NEEDS_RELEASE" \
    || ! module_published "framework/v${FRAMEWORK_VERSION}" "$FRAMEWORK_NEEDS_RELEASE"; then
    for plugin_name in "${PLUGIN_CHANGES[@]}"; do
      echo "::notice::plugin $plugin_name is not released: it needs core v${CORE_VERSION} and framework v${FRAMEWORK_VERSION} published, and at least one has no tag and no release due in this run"
    done
    PLUGIN_CHANGES=()
    return 0
  fi

  # Dropping one plugin can strand another that requires it, so repeat until nothing
  # changes.
  dropped="true"
  while [ "$dropped" = "true" ] && [ ${#PLUGIN_CHANGES[@]} -gt 0 ]; do
    dropped="false"
    local kept=()
    for plugin_name in "${PLUGIN_CHANGES[@]}"; do
      missing=""
      while IFS= read -r dependency; do
        [ -n "$dependency" ] || continue
        validate_plugin_name "$dependency" || return 1
        dependency_version=$(tr -d '\n\r' < "plugins/${dependency}/version" 2>/dev/null || true)
        selected="false"
        for other in "${PLUGIN_CHANGES[@]}"; do
          if [ "$other" = "$dependency" ]; then
            selected="true"
          fi
        done
        if [ -z "$dependency_version" ] || ! module_published "plugins/${dependency}/v${dependency_version}" "$selected"; then
          missing="${dependency} v${dependency_version:-?}"
          break
        fi
      done < <(sed -n "s#^[[:space:]]*\(require[[:space:]]\{1,\}\)\{0,1\}${module_root}/plugins/\([^[:space:]]*\)[[:space:]].*#\2#p" "plugins/${plugin_name}/go.mod" 2>/dev/null)
      if [ -n "$missing" ]; then
        echo "::notice::plugin $plugin_name is not released: it requires plugin $missing, which has no tag and no release due in this run"
        dropped="true"
      else
        kept+=("$plugin_name")
      fi
    done
    PLUGIN_CHANGES=("${kept[@]+"${kept[@]}"}")
  done
}
