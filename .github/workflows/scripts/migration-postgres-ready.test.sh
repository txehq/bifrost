#!/usr/bin/env bash
set -uo pipefail

# Tests that run-migration-tests.sh waits for the real PostgreSQL server (TXE-5184).
#
# On first start the postgres image runs a temporary server that listens only on the Unix
# socket, initialises the cluster, stops, and starts the real server. A readiness probe over
# the socket can pass during that window; the first DROP DATABASE then finds no socket, which
# failed test-migrations twice on release run 40282744249.
#
# This runs the script's own ensure_postgres_running against a fake docker that plays that
# start-up: the first probe finds nothing, the next two find only the init server's socket,
# and the real server answers both socket and TCP from the fourth probe on. The wait must not
# return before the real server is up. Fake docker, pg_isready and sleep only: nothing starts.
#
# Usage: ./migration-postgres-ready.test.sh
#        RUN_MIGRATION_TESTS=/path/to/other/run-migration-tests.sh ./migration-postgres-ready.test.sh

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TARGET="${RUN_MIGRATION_TESTS:-${SCRIPT_DIR}/run-migration-tests.sh}"
REPO_ROOT="$(git -C "$SCRIPT_DIR" rev-parse --show-toplevel)"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
FAKE_BIN="$WORK/bin"
mkdir -p "$FAKE_BIN"
PROBES="$WORK/probes"
: > "$PROBES"

failures=0
fail() {
  echo "FAIL: $1" >&2
  failures=$((failures + 1))
}

# docker compose ... exec -T postgres pg_isready [args]: each probe advances the start-up by
# one step and records "<step> <phase> <socket|tcp> <answered>".
cat > "$FAKE_BIN/docker" <<EOF
#!/usr/bin/env bash
case " \$* " in
  *" up -d postgres "*) exit 0 ;;
  *" exec -T postgres pg_isready "*)
    step=\$(( \$(wc -l < "$PROBES") + 1 ))
    if [ "\$step" -eq 1 ]; then phase=none
    elif [ "\$step" -le 3 ]; then phase=init
    else phase=real
    fi
    case " \$* " in *" -h "*) via=tcp ;; *) via=socket ;; esac
    answered=no
    [ "\$phase" = real ] && answered=yes
    [ "\$phase" = init ] && [ "\$via" = socket ] && answered=yes
    echo "\$step \$phase \$via \$answered" >> "$PROBES"
    [ "\$answered" = yes ] ;;
  *) exit 1 ;;
esac
EOF
# The host-side connectivity check that follows the wait; it plays no part in the race.
printf '#!/usr/bin/env bash\nexit 0\n' > "$FAKE_BIN/pg_isready"
printf '#!/usr/bin/env bash\nexit 0\n' > "$FAKE_BIN/sleep"
chmod +x "$FAKE_BIN/docker" "$FAKE_BIN/pg_isready" "$FAKE_BIN/sleep"

functions="$(awk '/^ensure_postgres_running\(\) \{$/,/^\}$/' "$TARGET")"
[ -n "$functions" ] || { echo "FAIL: ensure_postgres_running not found in $TARGET" >&2; exit 1; }

PATH="$FAKE_BIN:$PATH" bash -c "
  log_info() { :; }
  log_warn() { :; }
  log_error() { echo \"\$1\" >&2; }
  REPO_ROOT='$REPO_ROOT' POSTGRES_USER=bifrost POSTGRES_HOST=localhost POSTGRES_PORT=5432
  $functions
  ensure_postgres_running
" || fail "ensure_postgres_running failed"

last="$(tail -n 1 "$PROBES")"
case "$last" in
  *" real "*" yes") ;;
  *) fail "the wait returned on probe '$last' before the real server was up (probes: $(tr '\n' ';' < "$PROBES"))" ;;
esac
grep -q " init socket yes$" "$PROBES" && fail "a socket probe was used, which the init server answers (probes: $(tr '\n' ';' < "$PROBES"))"

if [ "$failures" -gt 0 ]; then
  echo "$failures check(s) failed" >&2
  exit 1
fi
echo "ok - ensure_postgres_running waits for the real server, not the init-time socket"
