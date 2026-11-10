#!/usr/bin/env bash
# The check for scripts/test-offline.sh (TXE-7053): no credential variable reaches the command
# it runs, whichever shell calls it, and nothing else is removed. Runs in the offline API
# suites (.github/workflows/scripts/test-api-collections-offline.sh), so a pattern dropped from
# the script fails the PR.
set -Eeuo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
RUN="$ROOT/scripts/test-offline.sh"
failures=0
fail() {
	echo "  FAIL - $1" >&2
	failures=$((failures + 1))
}
pass() { echo "  ok - $1"; }

# Credentials the script must remove, by the patterns it documents, and look-alikes it must
# keep. A value with a newline and an "=" checks that names are read, not parsed out of env.
credentials=(
	OPENAI_API_KEY DEEPSEEK_API_KEY lower_case_api_key API_KEY OPENAI_API_KEY_2
	AZURE_CLIENT_SECRET MY_CLIENT_SECRET SECRET_SAUCE
	GH_TOKEN HF_TOKEN TOKEN SLACK_TOKEN_PROD
	AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_PROFILE AZURE_OPENAI_ENDPOINT
	GOOGLE_APPLICATION_CREDENTIALS
)
keep=(KEEP_ME MAX_TOKENS TOKENIZER_PATH KEYBOARD_LAYOUT)
for name in "${credentials[@]}"; do export "$name=value-of-$name"; done
for name in "${keep[@]}"; do export "$name=kept"; done
export MULTILINE_API_KEY=$'line one\nFAKE_INJECTED=1'

# check_child <label> <command...>: the command prints the child's environment as NUL-separated
# NAME=value entries; no credential may be in it, every keep variable must be.
check_child() {
	local label=$1
	shift
	local child
	child=$("$@" | tr '\0' '\n' | sed -n 's/^\([A-Za-z_][A-Za-z0-9_]*\)=.*/\1/p')
	local leaked=()
	for name in "${credentials[@]}" MULTILINE_API_KEY; do
		if printf '%s\n' "$child" | grep -qx "$name"; then leaked+=("$name"); fi
	done
	if [ "${#leaked[@]}" -gt 0 ]; then
		fail "$label: credentials reached the child: ${leaked[*]}"
	else
		pass "$label: no credential reached the child"
	fi
	local lost=()
	for name in "${keep[@]}" PATH HOME; do
		if ! printf '%s\n' "$child" | grep -qx "$name"; then lost+=("$name"); fi
	done
	if [ "${#lost[@]}" -gt 0 ]; then
		fail "$label: non-credential variables were removed: ${lost[*]}"
	else
		pass "$label: non-credential variables are kept"
	fi
}

check_child "called directly" "$RUN" env -0
# $0 is the runner's path, expanded by the child shell, not here.
# shellcheck disable=SC2016
check_child "called from bash" bash -c '"$0" env -0' "$RUN"
# shellcheck disable=SC2016
check_child "called from sh" sh -c '"$0" env -0' "$RUN"
if command -v zsh >/dev/null 2>&1; then
	# shellcheck disable=SC2016
	check_child "called from zsh" zsh -c '"$0" env -0' "$RUN"
else
	echo "  skip - called from zsh: zsh is not installed here"
fi

# The idiom the script replaces really does leak in zsh, so this check can tell the two apart.
if command -v zsh >/dev/null 2>&1; then
	if zsh -c 'U=$(env | grep -o "^[A-Z_]*_API_KEY" | sed "s/^/-u /"); env $U env' 2>/dev/null | grep -q '^OPENAI_API_KEY='; then
		pass "the hand-built zsh idiom leaks (what the script replaces)"
	else
		pass "the hand-built zsh idiom did not leak here"
	fi
fi

# --list prints names only, never a value.
listed=$("$RUN" --list)
if printf '%s\n' "$listed" | grep -q 'value-of-'; then
	fail "--list printed a value"
elif ! printf '%s\n' "$listed" | grep -qx 'DEEPSEEK_API_KEY'; then
	fail "--list did not name DEEPSEEK_API_KEY: $listed"
else
	pass "--list names the credentials and prints no value"
fi

# The command's exit status and arguments pass through unchanged.
set +e
"$RUN" sh -c 'exit 7'
status=$?
set -e
if [ "$status" -eq 7 ]; then pass "the command's exit status passes through"; else fail "exit status $status, want 7"; fi
if [ "$("$RUN" printf '%s|' 'a b' '' 'c')" = 'a b||c|' ]; then pass "arguments pass through unchanged"; else fail "arguments changed"; fi

# No command is a usage error.
set +e
"$RUN" 2>/dev/null
status=$?
set -e
if [ "$status" -eq 2 ]; then pass "no command is a usage error"; else fail "no command exited $status, want 2"; fi

if [ "$failures" -gt 0 ]; then
	echo "$failures failed" >&2
	exit 1
fi
echo "all passed"
