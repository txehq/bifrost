#!/usr/bin/env bash
# Print the docker build arguments that send the transports Dockerfiles' `npm ci` to the
# runner's npm cache (TXE-6357), one NAME=VALUE per line, for docker-build-push-action's
# build-args:. `--flags` prints them as `--build-arg NAME=VALUE` words for `docker build`.
#
# BuildKit starts each RUN step without the job's environment, so an `npm ci` in a
# Dockerfile goes to registry.npmjs.org even on an on-prem runner whose job-started hook
# (txehq/action-runner-ops 20-npm-cache.sh) points the job's own steps at the LAN npm
# cache: through the relay, and against its quota (TXE-6354). Each Dockerfile declares
# `ARG npm_config_registry` and `ARG pnpm_config_registry` with the public default; this
# passes the runner's value when it has one and the registry answers /-/ping within 2 s.
# npm reads the first name, pnpm 11 only the second, so both are passed.
#
# It prints nothing when the runner sets no registry (GitHub-hosted runners) or the
# registry does not answer, so the build falls back to registry.npmjs.org; stderr says
# which route the build takes. ui/package-lock.json records registry.npmjs.org, which npm
# replaces with the configured registry (replace-registry-host defaults to npmjs).
#
# No argument here carries a credential. Never add one: build arguments are recorded in
# the image's provenance.
set -euo pipefail

flags=false
case "${1:-}" in
  --flags) flags=true ;;
  "") ;;
  *) echo "usage: $0 [--flags]" >&2; exit 2 ;;
esac

emit() { # name, value
  if $flags; then
    printf -- '--build-arg\n%s=%s\n' "$1" "$2"
  else
    printf '%s=%s\n' "$1" "$2"
  fi
}

registry=${npm_config_registry:-}
if [[ -z $registry ]]; then
  echo "npm routes: the runner sets no npm registry; npm ci uses registry.npmjs.org" >&2
  exit 0
fi
case $registry in
  http://* | https://*) ;;
  *) echo "npm routes: npm_config_registry is not an http(s) URL; npm ci uses registry.npmjs.org" >&2; exit 0 ;;
esac
if curl -s -o /dev/null --noproxy '*' --connect-timeout 2 --max-time 2 "${registry%/}/-/ping" 2>/dev/null; then
  emit npm_config_registry "$registry"
  emit pnpm_config_registry "$registry"
  echo "npm routes: npm ci uses $registry" >&2
else
  echo "::warning::npm routes: $registry does not answer; npm ci uses registry.npmjs.org" >&2
fi
