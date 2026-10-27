#!/usr/bin/env bash
set -uo pipefail

# Tests that run-migration-tests.sh's SQLite faker step fails closed (TXE-5283).
#
# The same faker file is applied to config.db and to logs.db, so "no such table" is
# expected for the counterpart store's tables. It must be accepted for those only: a
# statement reading a table that is in neither store dropped its rows just as silently
# as a bad column does. A non-zero sqlite3 exit that prints no error line must fail too.
#
# This runs the script's own apply_sqlite_faker_sql against throwaway databases in a
# temp directory; the silent-exit case uses a fake sqlite3 on PATH.
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

# A fake sqlite3 that fails without printing anything. sqlite_has_table also calls
# sqlite3, so the fake answers its query the way a real store without the table would.
mkdir -p "$WORK/bin"
cat > "$WORK/bin/sqlite3" <<'EOF'
#!/usr/bin/env bash
[ $# -ge 2 ] && { echo 0; exit 0; }
cat > /dev/null
exit 1
EOF
chmod +x "$WORK/bin/sqlite3"
PATH="$WORK/bin:$PATH" expect fail "a non-zero sqlite3 exit with no error line fails" "$CONFIG_DB" "$LOGS_DB" \
  "INSERT INTO governance_teams (id, name) VALUES ('team-4', 'Team');"

if [ "$failures" -ne 0 ]; then
  echo "$failures check(s) failed" >&2
  exit 1
fi
echo "OK: SQLite faker SQL fails closed on unknown missing tables, bad columns, constraints and silent exits"
