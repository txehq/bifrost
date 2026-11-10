#!/usr/bin/env bash
# Runs a command with every credential variable removed from its environment (TXE-7053).
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
#   AWS_*, AZURE_*                      cloud credentials and their profiles/endpoints
#   GOOGLE_APPLICATION_CREDENTIALS, GOOGLE_API_KEY, GEMINI_API_KEY (covered by *_API_KEY)
# tests/test_offline_test.sh is the check: it fails when any such variable reaches the child.
set -euo pipefail

usage() {
	echo "usage: $0 <command> [args...]   |   $0 --list" >&2
	exit 2
}

is_credential() {
	local upper
	upper=$(printf '%s' "$1" | tr '[:lower:]' '[:upper:]')
	case "$upper" in
	API_KEY | *_API_KEY | *_API_KEY_* | *SECRET* | TOKEN | *_TOKEN | *_TOKEN_* | AWS_* | AZURE_* | GOOGLE_APPLICATION_CREDENTIALS)
		return 0
		;;
	esac
	return 1
}

removed=()
while IFS= read -r name; do
	if is_credential "$name"; then
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
exec env ${unset_args[@]+"${unset_args[@]}"} "$@"
