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
	GOOGLE_APPLICATION_CREDENTIALS VERTEX_CREDENTIALS GITHUB_COPILOT_PRIVATE_KEY DATABRICKS_TOKEN
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

# Every vendor credential the repository names outside the *_API_KEY convention
# (OTHER_PROVIDER_CREDENTIALS in check-provider-key-environment.py) is removed. A credential
# added there that no pattern of the runner matches fails here, in the same PR.
other=$(sed -n '/^OTHER_PROVIDER_CREDENTIALS = {/,/^}/p' "$ROOT/.github/workflows/scripts/check-provider-key-environment.py" | grep -o '"[A-Z0-9_]*"' | tr -d '"')
if [ -z "$other" ]; then
	fail "OTHER_PROVIDER_CREDENTIALS could not be read from check-provider-key-environment.py"
else
	missed=()
	for name in $other; do
		if ! env "$name=x" "$RUN" --list | grep -qx "$name"; then missed+=("$name"); fi
	done
	if [ "${#missed[@]}" -gt 0 ]; then
		fail "credentials in OTHER_PROVIDER_CREDENTIALS the runner keeps: ${missed[*]}"
	else
		pass "every OTHER_PROVIDER_CREDENTIALS name is removed ($(printf '%s\n' "$other" | wc -l | tr -d ' '))"
	fi
fi

# A make recipe run under the runner does not re-load credentials: EXPOSE_ENV sees
# BIFROST_TEST_OFFLINE=1 and skips .env and Infisical. The plain run is the control: it does
# load .env, so the probe can tell the two apart.
if command -v make >/dev/null 2>&1; then
	probe=$(mktemp -d)
	printf 'PROBE_API_KEY=from-dotenv\n' >"$probe/.env"
	# $(EXPOSE_ENV) is make syntax, expanded by make, not here.
	# shellcheck disable=SC2016
	printf 'include %s/Makefile\noffline-probe:\n\t@$(EXPOSE_ENV); env | grep -c "^PROBE_API_KEY=" || true\n' "$ROOT" >"$probe/probe.mk"
	# The control must load .env even when this check itself runs under the runner.
	control=$(cd "$probe" && env -u BIFROST_TEST_OFFLINE make -s -f probe.mk -I "$ROOT" offline-probe 2>/dev/null | tail -1)
	offline=$(cd "$probe" && "$RUN" make -s -f probe.mk -I "$ROOT" offline-probe 2>/dev/null | tail -1)
	rm -rf "$probe"
	if [ "$control" != "1" ]; then
		fail "control: a plain make recipe did not load .env (got '$control'), so the probe proves nothing"
	elif [ "$offline" != "0" ]; then
		fail "a make recipe under the runner re-loaded a credential from .env (got '$offline')"
	else
		pass "a make recipe under the runner loads no .env credential (the plain run does)"
	fi
else
	echo "  skip - make recipe: make is not installed here"
fi

# Every file that loads a .env file or mints a cloud token from a CLI login (the Makefile's
# EXPOSE_ENV and its gcloud print-access-token, the integration suites' dotenv setup, any loader
# added later) honours BIFROST_TEST_OFFLINE, or a test run under the runner
# could re-load the credentials it removed. The patterns are the loaders the repository uses:
# shell `set -a; . ./.env`, dotenv's config(), python-dotenv's load_dotenv() and godotenv.
loader_pattern='set -a; \. \./\.env|from .dotenv.|load_dotenv\(|godotenv\.(Load|Overload)|require\(.dotenv.\)|dotenv/config|print-access-token'
if loaders=$(git -C "$ROOT" grep -lE "$loader_pattern" -- . ':!docs' ':!**/node_modules/**' ':!tests/test_offline_test.sh' 2>/dev/null); then
	unguarded=()
	for file in $loaders; do
		if ! grep -q 'BIFROST_TEST_OFFLINE' "$ROOT/$file"; then unguarded+=("$file"); fi
	done
	if [ "${#unguarded[@]}" -gt 0 ]; then
		fail ".env loaders that ignore BIFROST_TEST_OFFLINE: ${unguarded[*]}"
	else
		pass "every .env loader honours BIFROST_TEST_OFFLINE ($(printf '%s\n' "$loaders" | wc -l | tr -d ' ') files)"
	fi
else
	fail "no .env loader found: the loader pattern no longer matches the Makefile's EXPOSE_ENV"
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
