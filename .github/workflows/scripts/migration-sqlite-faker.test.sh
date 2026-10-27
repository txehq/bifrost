#!/usr/bin/env bash
set -uo pipefail

# Tests that run-migration-tests.sh's SQLite faker step fails closed (TXE-5283).
#
# The same faker file is applied to config.db and to logs.db, so "no such table" is
# expected for the counterpart store's tables. It must be accepted for those only: a
# statement reading a table that is in neither store dropped its rows just as silently
# as a bad column does. An exit status other than 0, or 1 with an error line, and any
# output that is neither an error line nor its SQL excerpt, must fail too, even beside an
# expected counterpart error.
#
# This runs the script's own apply_sqlite_faker_sql against throwaway databases in a
# temp directory; the exit-status and diagnostic cases use a fake sqlite3 on PATH.
#
# Usage: ./migration-sqlite-faker.test.sh
#        RUN_MIGRATION_TESTS=/path/to/other/run-migration-tests.sh ./migration-sqlite-faker.test.sh

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TARGET="${RUN_MIGRATION_TESTS:-${SCRIPT_DIR}/run-migration-tests.sh}"

if ! command -v sqlite3 >/dev/null 2>&1; then
  echo "FAIL: sqlite3 is required" >&2
  exit 1
fi

functions="$(awk '/^apply_sqlite_faker_sql\(\) \{$/,/^\}$/; /^sqlite_has_table\(\) \{$/,/^\}$/' "$TARGET")"
case "$functions" in
  *"apply_sqlite_faker_sql() {"*"sqlite_has_table() {"*) ;;
  *) echo "FAIL: apply_sqlite_faker_sql or sqlite_has_table not found in $TARGET" >&2; exit 1 ;;
esac
log_error() { echo "[ERROR] $*" >&2; }
eval "$functions"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
CONFIG_DB="$WORK/config.db"
LOGS_DB="$WORK/logs.db"
sqlite3 "$CONFIG_DB" 'CREATE TABLE governance_teams (id TEXT PRIMARY KEY, name TEXT);'
sqlite3 "$LOGS_DB" 'CREATE TABLE logs (id TEXT PRIMARY KEY);'

failures=0
fail() {
  echo "FAIL: $1" >&2
  failures=$((failures + 1))
}

# expect <pass|fail> <name> <db> <counterpart> <sql>
expect() {
  local want="$1" name="$2" db="$3" counterpart="$4" sql="$5" got=pass
  printf '%s\n' "$sql" > "$WORK/case.sql"
  apply_sqlite_faker_sql "$db" "$WORK/case.sql" "$counterpart" 2> "$WORK/case.err" || got=fail
  if [ "$got" = "$want" ]; then
    echo "ok - $name"
  else
    fail "$name: expected $want, got $got"
    sed 's/^/    /' "$WORK/case.err" >&2
  fi
}

expect pass "clean SQL applies" "$CONFIG_DB" "$LOGS_DB" \
  "INSERT INTO governance_teams (id, name) VALUES ('team-1', 'Team');"
expect pass "a counterpart-store table missing from this store is expected" "$CONFIG_DB" "$LOGS_DB" \
  "INSERT INTO logs (id) VALUES ('log-1');"
expect pass "the same holds from the logs store" "$LOGS_DB" "$CONFIG_DB" \
  "INSERT INTO governance_teams (id, name) VALUES ('team-2', 'Team');"
expect fail "a table in neither store fails, even as a SELECT source" "$CONFIG_DB" "$LOGS_DB" \
  "INSERT INTO governance_teams (id) SELECT id FROM misspelled_fixture_source;"
expect fail "a counterpart table is not excused without the counterpart database" "$CONFIG_DB" "" \
  "INSERT INTO logs (id) VALUES ('log-2');"
expect fail "an unknown column fails" "$CONFIG_DB" "$LOGS_DB" \
  "INSERT INTO governance_teams (id, no_such_column) VALUES ('team-3', 1);"
expect fail "a constraint violation fails" "$CONFIG_DB" "$LOGS_DB" \
  "INSERT INTO governance_teams (id, name) VALUES ('team-1', 'Duplicate');"

# A fake sqlite3 in front of the real one. Queries given as arguments (the fixtures,
# sqlite_has_table) go to the real CLI. A script on stdin runs through the real CLI too,
# then FAKE_EXTRA is appended to its stderr and FAKE_STATUS, when set, replaces its exit
# status: the output of a CLI that crashed, or printed something it should not.
REAL_SQLITE3="$(command -v sqlite3)"
mkdir -p "$WORK/bin"
cat > "$WORK/bin/sqlite3" <<FAKE
#!/usr/bin/env bash
[ \$# -ge 2 ] && exec "$REAL_SQLITE3" "\$@"
"$REAL_SQLITE3" "\$@"; status=\$?
[ -n "\${FAKE_EXTRA:-}" ] && printf '%s\n' "\$FAKE_EXTRA" >&2
exit "\${FAKE_STATUS:-\$status}"
FAKE
chmod +x "$WORK/bin/sqlite3"

COUNTERPART_SQL="INSERT INTO logs (id) VALUES ('log-3');"
PATH="$WORK/bin:$PATH" expect pass "the fake CLI passes an expected counterpart error through unchanged" "$CONFIG_DB" "$LOGS_DB" \
  "$COUNTERPART_SQL"
PATH="$WORK/bin:$PATH" FAKE_STATUS=1 expect fail "a non-zero sqlite3 exit with no error line fails" "$CONFIG_DB" "$LOGS_DB" \
  "INSERT INTO governance_teams (id, name) VALUES ('team-4', 'Team');"
PATH="$WORK/bin:$PATH" FAKE_STATUS=139 expect fail "an expected counterpart error does not excuse an abnormal exit" "$CONFIG_DB" "$LOGS_DB" \
  "$COUNTERPART_SQL"
PATH="$WORK/bin:$PATH" FAKE_EXTRA="Segmentation fault" expect fail "an expected counterpart error does not excuse an unclassified diagnostic" "$CONFIG_DB" "$LOGS_DB" \
  "$COUNTERPART_SQL"
PATH="$WORK/bin:$PATH" FAKE_STATUS=2 expect fail "exit status 2 fails even with no error line" "$CONFIG_DB" "$LOGS_DB" \
  "INSERT INTO governance_teams (id, name) VALUES ('team-5', 'Team');"

if [ "$failures" -ne 0 ]; then
  echo "$failures check(s) failed" >&2
  exit 1
fi
echo "OK: SQLite faker SQL fails closed on unknown missing tables, bad columns, constraints, abnormal exits and stray diagnostics"
