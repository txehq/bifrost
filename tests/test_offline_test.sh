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
	# The control must load the probe's .env even when this check runs under the runner or the
	# caller has USE_INFISICAL set, so it clears the flag and forces the .env branch.
	control=$(cd "$probe" && env -u BIFROST_TEST_OFFLINE USE_INFISICAL=0 make -s -f probe.mk -I "$ROOT" offline-probe 2>/dev/null | tail -1)
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

# --- Egress (TXE-7083): a run under the runner reaches loopback only. ---

# Proxy variables are removed, so no client can hand a request to a proxy that carries it out.
child=$(HTTP_PROXY=http://proxy.invalid:3128 https_proxy=http://proxy.invalid:3128 ALL_PROXY=socks5://proxy.invalid:1080 NO_PROXY=example.com \
	"$RUN" env -0 | tr '\0' '\n')
if printf '%s\n' "$child" | grep -qiE '^(http|https|all|no)_proxy='; then
	fail "proxy variables reached the child: $(printf '%s\n' "$child" | grep -iE '^(http|https|all|no)_proxy=' | cut -d= -f1 | tr '\n' ' ')"
else
	pass "proxy variables are removed"
fi
if printf '%s\n' "$child" | grep -qx 'BIFROST_TEST_OFFLINE=1'; then
	pass "the child runs with BIFROST_TEST_OFFLINE=1"
else
	fail "BIFROST_TEST_OFFLINE=1 is missing in the child"
fi

# Bifrost's dial guard is on from the environment alone: a dial to a non-loopback address
# (TEST-NET-3, never routed) fails before connecting and names it. The control run without the runner skips the test, so the runner is
# what turns the guard on.
if command -v go >/dev/null 2>&1; then
	guard_test='^TestOfflineRunTurnsOnTheGuard$'
	guarded=$(cd "$ROOT/core" && "$RUN" go test ./network/ -run "$guard_test" -count=1 -v 2>&1 || true)
	unguarded=$(cd "$ROOT/core" && env -u BIFROST_TEST_OFFLINE go test ./network/ -run "$guard_test" -count=1 -v 2>&1 || true)
	if ! printf '%s\n' "$unguarded" | grep -q -- '--- SKIP: TestOfflineRunTurnsOnTheGuard'; then
		fail "control: without the runner the guard test did not skip: $(printf '%s\n' "$unguarded" | tail -3)"
	elif printf '%s\n' "$guarded" | grep -q -- '--- PASS: TestOfflineRunTurnsOnTheGuard' &&
		printf '%s\n' "$guarded" | grep -q 'egress to 203.0.113.10:443 refused'; then
		pass "under the runner Bifrost refuses a non-loopback address before connecting, naming it (the control run skips)"
	else
		fail "under the runner the Go dial guard did not refuse loudly: $(printf '%s\n' "$guarded" | tail -5)"
	fi
else
	echo "  skip - Go dial guard: go is not installed here"
fi

# On macOS every process the runner starts is sandboxed, not only Bifrost: a raw socket to
# TEST-NET-1 (192.0.2.1, never routed, so the control cannot reach anything either) gets EPERM
# under the runner, while a loopback listener stays reachable.
if [ "$(uname -s)" = Darwin ] && command -v python3 >/dev/null 2>&1; then
	socket_probe='
import errno, socket
srv = socket.socket(); srv.bind(("127.0.0.1", 0)); srv.listen(1)
loop = socket.create_connection(srv.getsockname(), timeout=3); loop.close()
s = socket.socket(); s.settimeout(3)
try:
    s.connect(("192.0.2.1", 443)); print("outbound=connected")
except OSError as e:
    print("outbound=" + ("EPERM" if e.errno == errno.EPERM else "errno%s" % (e.errno,)))
'
	sandboxed=$("$RUN" python3 -I -c "$socket_probe" 2>&1 | tail -1)
	plain=$(env -u BIFROST_TEST_OFFLINE_SANDBOX python3 -I -c "$socket_probe" 2>&1 | tail -1)
	if [ "$sandboxed" != "outbound=EPERM" ]; then
		fail "macOS sandbox: a non-loopback connection under the runner gave '$sandboxed', want outbound=EPERM"
	elif [ "$plain" = "outbound=EPERM" ] && [ "${BIFROST_TEST_OFFLINE_SANDBOX:-}" != sandbox-exec ]; then
		fail "control: the probe got EPERM without the runner too, so it proves nothing"
	else
		pass "macOS sandbox: a non-loopback connection fails with EPERM, loopback works (control: $plain)"
	fi
else
	echo "  skip - OS egress sandbox: macOS only (this run relies on the dial guard)"
fi

if [ "$failures" -gt 0 ]; then
	echo "$failures failed" >&2
	exit 1
fi
echo "all passed"
