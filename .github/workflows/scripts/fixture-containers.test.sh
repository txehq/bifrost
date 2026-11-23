#!/usr/bin/env bash
# shellcheck disable=SC2016 # the SEED strings and bash -c script expand in the child, on purpose
# Tests fixture-containers.sh's fixture_run against a stub docker (TXE-6085): no real container,
# network or image is touched. The stub keeps its state in a temporary directory and logs every
# call, so each case checks what fixture_run asked docker to do.
#
#   1. Outside GitHub Actions it refuses, and calls docker for nothing.
#   2. A same-name container with another owner's label is refused and never removed.
#   3. A same-name container with no label is refused and never removed.
#   4. This run's own container is replaced (rm -f, then run with the owner label at the address).
#   5. A same-name network with another owner is refused; nothing is created or run.
#   6. A same-name network of this run with another subnet is refused.
#   7. With nothing present, the network is created with the label and subnet, then the container.
#   8. An unknown fixture key is refused.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$WORK/bin"
cat >"$WORK/bin/docker" <<'STUB'
#!/usr/bin/env bash
# Stub docker. State: $STUB_STATE/net.<name> holds "owner subnet"; $STUB_STATE/ctr.<name> holds
# "owner" ("-" for no label). Every call is appended to $STUB_STATE/calls.
echo "$*" >>"$STUB_STATE/calls"
label_value() { # the owner from a format string's query, from a state file
	local v; v="$(cut -d' ' -f1 <"$1")"; [ "$v" = "-" ] && v=""; printf '%s\n' "$v"
}
case "$1 $2" in
"network inspect")
	if [ "$3" = "-f" ]; then
		f="$STUB_STATE/net.$5"
		case "$4" in *Labels*) label_value "$f" ;; *IPAM*) printf '%s \n' "$(cut -d' ' -f2 <"$f")" ;; esac
		exit 0
	fi
	[ -f "$STUB_STATE/net.$3" ] ;;
"network create") exit 0 ;;
"container inspect")
	if [ "$3" = "-f" ]; then label_value "$STUB_STATE/ctr.$5"; exit 0; fi
	[ -f "$STUB_STATE/ctr.$3" ] ;;
"rm -f") rm -f "$STUB_STATE/ctr.$3" ;;
"run -d") exit 0 ;;
*) echo "stub docker: unexpected call: $*" >&2; exit 2 ;;
esac
STUB
chmod +x "$WORK/bin/docker"

failures=0
fail() { echo "  FAIL: $*" >&2; failures=$((failures + 1)); }

# Runs fixture_run in a fresh shell with the stub first on PATH. $1 = case name, $2 = "ci" or
# "local", rest = fixture_run arguments. Leaves the exit status in $status and the calls in $calls.
run_case() {
	local name="$1" where="$2"
	shift 2
	echo "case: $name"
	export STUB_STATE="$WORK/state.$name"
	mkdir -p "$STUB_STATE"
	: >"$STUB_STATE/calls"
	[ -n "${SEED:-}" ] && eval "$SEED"
	local env=(PATH="$WORK/bin:$PATH" STUB_STATE="$STUB_STATE" GITHUB_ENV="$WORK/env" HOME="$WORK")
	if [ "$where" = ci ]; then
		env+=(GITHUB_ACTIONS=true GITHUB_RUN_ID=42 GITHUB_RUN_ATTEMPT=1 GITHUB_JOB=fixture-folders)
	fi
	set +e
	env -i "${env[@]}" bash -c 'set -euo pipefail; source "$1"; shift; fixture_run "$@"' _ "$HERE/fixture-containers.sh" "$@" 2>"$STUB_STATE/stderr"
	status=$?
	set -e
	calls="$(cat "$STUB_STATE/calls")"
	SEED=""
}
OWN="42-1-fixture-folders"
SUB="172.39.0.0/24"

run_case outside-actions local CLICKHOUSE
[ "$status" -ne 0 ] || fail "started outside GitHub Actions"
[ -z "$calls" ] || fail "called docker outside GitHub Actions: $calls"

SEED='echo "$OWN $SUB" >"$STUB_STATE/net.fixture-folders"; echo "someone-else" >"$STUB_STATE/ctr.fixture-clickhouse"'
run_case foreign-container ci CLICKHOUSE
[ "$status" -ne 0 ] || fail "a foreign container was not refused"
grep -q '^rm ' <<<"$calls" && fail "a foreign container was removed: $calls"
grep -q '^run ' <<<"$calls" && fail "ran beside a foreign container: $calls"

SEED='echo "$OWN $SUB" >"$STUB_STATE/net.fixture-folders"; echo "-" >"$STUB_STATE/ctr.fixture-clickhouse"'
run_case unlabelled-container ci CLICKHOUSE
[ "$status" -ne 0 ] || fail "an unlabelled container was not refused"
grep -q '^rm ' <<<"$calls" && fail "an unlabelled container was removed: $calls"

SEED='echo "$OWN $SUB" >"$STUB_STATE/net.fixture-folders"; echo "$OWN" >"$STUB_STATE/ctr.fixture-clickhouse"'
run_case own-container ci CLICKHOUSE -p 127.0.0.1:9001:9000
[ "$status" -eq 0 ] || fail "this run's own container was not replaced (status $status)"
grep -qx 'rm -f fixture-clickhouse' <<<"$calls" || fail "own container not removed: $calls"
grep -q "^run -d --name fixture-clickhouse --label org.txehq.bifrost.fixture-owner=$OWN --network fixture-folders --ip 172.39.0.13 -p 127.0.0.1:9001:9000 .*clickhouse-server:24.8-alpine@sha256:" <<<"$calls" ||
	fail "own container not started with its label, address and options: $calls"

SEED='echo "someone-else $SUB" >"$STUB_STATE/net.fixture-folders"'
run_case foreign-network ci WEAVIATE
[ "$status" -ne 0 ] || fail "a foreign network was not refused"
grep -qE '^(run|network create|rm) ' <<<"$calls" && fail "acted on a foreign network: $calls"

SEED='echo "$OWN 10.0.0.0/24" >"$STUB_STATE/net.fixture-folders"'
run_case other-subnet ci WEAVIATE
[ "$status" -ne 0 ] || fail "a network with another subnet was not refused"
grep -q '^run ' <<<"$calls" && fail "ran on a network with another subnet: $calls"

run_case fresh ci WEAVIATE -p 127.0.0.1:8080:8080 -- --host 0.0.0.0
[ "$status" -eq 0 ] || fail "a fresh start failed (status $status): $(cat "$STUB_STATE/stderr")"
grep -qx "network create --subnet $SUB --label org.txehq.bifrost.fixture-owner=$OWN fixture-folders" <<<"$calls" ||
	fail "network not created with its subnet and label: $calls"
grep -q "^run -d --name fixture-weaviate --label org.txehq.bifrost.fixture-owner=$OWN --network fixture-folders --ip 172.39.0.12 -p 127.0.0.1:8080:8080 .*weaviate:1.32.4@sha256:[0-9a-f]* --host 0.0.0.0$" <<<"$calls" ||
	fail "container not started with its image arguments after the image: $calls"

run_case unknown-key ci NOPE
[ "$status" -ne 0 ] || fail "an unknown fixture key was accepted"
[ -z "$calls" ] || fail "called docker for an unknown key: $calls"

if [ "$failures" -ne 0 ]; then
	echo "fixture-containers.test.sh: $failures failure(s)" >&2
	exit 1
fi
echo "fixture-containers.test.sh: 8 cases passed"
