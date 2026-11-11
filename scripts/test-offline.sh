#!/usr/bin/env bash
# Runs a command with every credential variable removed from its environment (TXE-7053), and
# with every connection to a host that is not loopback refused (TXE-7083).
#
#   scripts/test-offline.sh go test ./...
#   scripts/test-offline.sh --list            # print the names it would remove, one per line
#
# Live provider suites skip when their key variable is absent, so a test run under this
# script makes no live vendor call however the calling shell is set up. It replaces stripping
# keys by hand: the idiom U=$(env | grep ... | sed 's/^/-u /'); env $U go test passes ONE
# argument in zsh, which does not word-split an unquoted parameter, so every key stayed set
# and the DeepSeek live suite called the vendor (TXE-7053). This script is bash whatever shell
# calls it, reads variable NAMES only (compgen -e, so a value with newlines cannot confuse it),
# and passes the removals to env as an array.
#
# Removed, matched on the upper-cased name:
#   *_API_KEY, *_API_KEY_*, API_KEY     provider keys (OPENAI_API_KEY, DEEPSEEK_API_KEY, ...)
#   *SECRET*                            client secrets, AWS_SECRET_ACCESS_KEY, ...
#   *_TOKEN, *_TOKEN_*, TOKEN           bearer and session tokens (GH_TOKEN, HF_TOKEN, ...)
#   *_CREDENTIALS                       VERTEX_CREDENTIALS, GOOGLE_APPLICATION_CREDENTIALS, ...
#   *_PRIVATE_KEY                       GITHUB_COPILOT_PRIVATE_KEY, ...
#   AWS_*, AZURE_*                      cloud credentials and their profiles/endpoints
# Every name in OTHER_PROVIDER_CREDENTIALS (.github/workflows/scripts/
# check-provider-key-environment.py), the repository's list of vendor credentials not called
# API keys, must match; the check reads that list and fails on one that does not.
#
# The child also gets BIFROST_TEST_OFFLINE=1, which the Makefile's EXPOSE_ENV honours: a make
# recipe run under this script loads neither .env nor Infisical secrets, so
# `scripts/test-offline.sh make <target>` cannot re-load the keys removed here.
# tests/test_offline_test.sh is the check: it fails when any such variable reaches the child.
#
# Egress (TXE-7083). Removing keys is not enough: a provider that ignores
# network_config.base_url (Bedrock builds its host from the region) sent two requests from a
# fixture run to the real AWS. So the child cannot reach any host but loopback:
#   - BIFROST_TEST_OFFLINE=1 turns on Bifrost's dial guard (core/network/offline.go). Every
#     dial Bifrost makes to a host that is not localhost, 127.0.0.0/8 or ::1 fails before DNS
#     with "offline run (BIFROST_TEST_OFFLINE=1): egress to <host:port> refused".
#   - The proxy variables (HTTP_PROXY, HTTPS_PROXY, ALL_PROXY, NO_PROXY, either case) are
#     removed, so a client that honours them dials directly and meets the guard instead of a
#     proxy that would carry the request out.
#   - On macOS the command runs under sandbox-exec with a profile that denies every outbound
#     connection except to loopback and unix sockets. That covers every process the command
#     starts (Go, node, newman, curl), not only Bifrost's dialers: a connection elsewhere fails
#     with "Operation not permitted". Nested runs reuse the outer sandbox. If the sandbox cannot
#     be applied, the script refuses to run rather than run unguarded.
#   - Elsewhere (Linux CI) there is no OS sandbox; the dial guard and the removed proxy
#     variables apply. BIFROST_TEST_OFFLINE_SANDBOX in the child says which applied:
#     "sandbox-exec" or "none".
# Go module downloads are egress too: run `go mod download` before a cold-cache test run.
set -euo pipefail

usage() {
	echo "usage: $0 <command> [args...]   |   $0 --list" >&2
	exit 2
}

is_credential() {
	local upper
	upper=$(printf '%s' "$1" | tr '[:lower:]' '[:upper:]')
	case "$upper" in
	API_KEY | *_API_KEY | *_API_KEY_* | *SECRET* | TOKEN | *_TOKEN | *_TOKEN_* | *_CREDENTIALS | *_PRIVATE_KEY | AWS_* | AZURE_*)
		return 0
		;;
	esac
	return 1
}

is_proxy() {
	case "$(printf '%s' "$1" | tr '[:lower:]' '[:upper:]')" in
	HTTP_PROXY | HTTPS_PROXY | ALL_PROXY | NO_PROXY) return 0 ;;
	esac
	return 1
}

removed=()
while IFS= read -r name; do
	if is_credential "$name" || is_proxy "$name"; then
		removed+=("$name")
	fi
done < <(compgen -e)

if [ "${1:-}" = "--list" ]; then
	[ "$#" -eq 1 ] || usage
	if [ "${#removed[@]}" -gt 0 ]; then
		printf '%s\n' "${removed[@]}" | sort
	fi
	exit 0
fi
[ "$#" -ge 1 ] || usage

unset_args=()
for name in ${removed[@]+"${removed[@]}"}; do
	unset_args+=(-u "$name")
done

# The macOS egress sandbox: outbound connections only to loopback and unix sockets (DNS goes
# through mDNSResponder's socket, so names still resolve and the refusal names the address).
sandbox=()
sandbox_name=none
if [ "$(uname -s)" = Darwin ]; then
	profile='(version 1)(allow default)(deny network-outbound)(allow network-outbound (remote ip "localhost:*"))(allow network-outbound (remote unix-socket))'
	if ! command -v sandbox-exec >/dev/null 2>&1; then
		echo "$0: sandbox-exec is missing; refusing to run without the egress sandbox (TXE-7083)" >&2
		exit 1
	fi
	if sandbox-exec -p '(version 1)(allow default)' /usr/bin/true 2>/dev/null; then
		sandbox=(sandbox-exec -p "$profile")
		sandbox_name=sandbox-exec
	elif [ "${BIFROST_TEST_OFFLINE_SANDBOX:-}" = sandbox-exec ]; then
		sandbox_name=sandbox-exec # nested: already inside the outer run's sandbox
	else
		echo "$0: cannot apply the egress sandbox (already sandboxed by something else?); refusing to run unguarded (TXE-7083)" >&2
		exit 1
	fi
fi
exec env ${unset_args[@]+"${unset_args[@]}"} BIFROST_TEST_OFFLINE=1 BIFROST_TEST_OFFLINE_SANDBOX="$sandbox_name" \
	${sandbox[@]+"${sandbox[@]}"} "$@"
