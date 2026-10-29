#!/usr/bin/env bash
set -uo pipefail

# Tests run-migration-tests.sh's before/after comparison of matview_refresh_state (TXE-5368).
#
# The materialized-view refresher keeps one row per view it manages (mv_logs_*,
# mv_filter_*), rewrites it on every refresh and deletes it when the view is rebuilt.
# A baseline from v2.3.1-sync.3 on refreshes at boot, and so does the current binary, so
# those rows must not be held to the before/after comparison (Release Pipeline
# 40828610098 failed on them from v2.3.1). The seeded mv_migration_fixture row, which the
# refresher never writes, and any other unmanaged row must still fail the comparison when a
# migration changes or drops it.
#
# This runs the script's own compare_postgres_snapshots on synthetic snapshots in a temp
# directory; no database is needed.
#
# Usage: ./migration-matview-state.test.sh
#        RUN_MIGRATION_TESTS=/path/to/other/run-migration-tests.sh ./migration-matview-state.test.sh

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TARGET="${RUN_MIGRATION_TESTS:-${SCRIPT_DIR}/run-migration-tests.sh}"

functions="$(awk '/^compare_postgres_snapshots\(\) \{$/,/^\}$/' "$TARGET")"
case "$functions" in
  *"compare_postgres_snapshots() {"*) ;;
  *) echo "FAIL: compare_postgres_snapshots not found in $TARGET" >&2; exit 1 ;;
esac
log_info() { :; }
log_error() { echo "[ERROR] $*" >&2; }
eval "$functions"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

HEADER="# COLUMNS: view_name,last_started_at,last_success_at,last_status,last_error,last_duration_ms,last_node"
FIXTURE="mv_migration_fixture|2026-10-29 06:30:00+00|2026-10-29 06:30:00+00|success||1200|migration-node-001"
OLD_FILTER="mv_filter_models|2026-10-29 06:39:12.154901+00|2026-10-29 06:39:12.165558+00|success||10|runnervm48cm3:189548"
OLD_HOURLY="mv_logs_hourly|2026-10-29 06:39:12.100000+00|2026-10-29 06:39:12.140000+00|success||40|runnervm48cm3:189548"
NEW_FILTER="mv_filter_models|2026-10-29 06:43:40.000000+00|2026-10-29 06:43:40.011000+00|success||11|runnervm48cm3:190112"
NEW_HOURLY="mv_logs_hourly|2026-10-29 06:43:40.020000+00|2026-10-29 06:43:40.060000+00|success||38|runnervm48cm3:190112"
RUNNING_FILTER="mv_filter_aliases|2026-10-29 06:43:41.000000+00||running|||runnervm48cm3:190112"
TEAM_BEFORE="team-migration-test-1|Team One"
TEAM_AFTER="team-migration-test-1|Team Renamed"

failures=0
fail() {
  echo "FAIL: $1" >&2
  failures=$((failures + 1))
}

# write_snapshot <dir> <matview rows...>: one matview_refresh_state snapshot, plus an
# unchanged governance_teams table so a matview-only case exercises one table's handling.
write_snapshot() {
  local dir="$1"
  shift
  mkdir -p "$dir"
  printf '%s\n%s\n' matview_refresh_state governance_teams > "$dir/tables.txt"
  { echo "$HEADER"; [ "$#" -gt 0 ] && printf '%s\n' "$@"; } > "$dir/matview_refresh_state.csv"
  printf '%s\n%s\n' "# COLUMNS: id,name" "$TEAM_BEFORE" > "$dir/governance_teams.csv"
}

# expect <pass|fail> <name>: compare $WORK/<name>/before with $WORK/<name>/after.
expect() {
  local want="$1" name="$2" got=pass
  compare_postgres_snapshots "$WORK/$name/before" "$WORK/$name/after" 2> "$WORK/$name.err" || got=fail
  if [ "$got" = "$want" ]; then
    echo "ok - $name"
  else
    fail "$name: expected $want, got $got"
    sed 's/^/    /' "$WORK/$name.err" >&2
  fi
}

# The release failure: both binaries refreshed the views they manage.
write_snapshot "$WORK/refreshed-rows-rewritten/before" "$FIXTURE" "$OLD_FILTER" "$OLD_HOURLY"
write_snapshot "$WORK/refreshed-rows-rewritten/after" "$FIXTURE" "$NEW_FILTER" "$NEW_HOURLY"
expect pass refreshed-rows-rewritten

# A rebuilt view's row is deleted, and the binary stopped before refreshing it again.
write_snapshot "$WORK/rebuilt-view-row-gone/before" "$FIXTURE" "$OLD_FILTER" "$OLD_HOURLY"
write_snapshot "$WORK/rebuilt-view-row-gone/after" "$FIXTURE" "$NEW_FILTER"
expect pass rebuilt-view-row-gone

# A baseline before sync.3 never refreshed; the current binary adds rows, one still running.
write_snapshot "$WORK/rows-added-by-current-binary/before" "$FIXTURE"
write_snapshot "$WORK/rows-added-by-current-binary/after" "$FIXTURE" "$NEW_FILTER" "$NEW_HOURLY" "$RUNNING_FILTER"
expect pass rows-added-by-current-binary

# Nothing refreshed on either side.
write_snapshot "$WORK/fixture-only-unchanged/before" "$FIXTURE"
write_snapshot "$WORK/fixture-only-unchanged/after" "$FIXTURE"
expect pass fixture-only-unchanged

# The seeded row is data the migration must keep: changing it fails.
write_snapshot "$WORK/fixture-changed/before" "$FIXTURE" "$OLD_FILTER"
write_snapshot "$WORK/fixture-changed/after" "${FIXTURE/|1200|/|1300|}" "$NEW_FILTER"
expect fail fixture-changed

# So does a status the refresher would never write for it.
write_snapshot "$WORK/fixture-status-changed/before" "$FIXTURE" "$OLD_FILTER"
write_snapshot "$WORK/fixture-status-changed/after" "${FIXTURE/|success|/|failed|}" "$NEW_FILTER"
expect fail fixture-status-changed

# Dropping it fails, even though managed rows keep the total row count the same.
write_snapshot "$WORK/fixture-dropped/before" "$FIXTURE" "$OLD_FILTER"
write_snapshot "$WORK/fixture-dropped/after" "$NEW_FILTER" "$NEW_HOURLY"
expect fail fixture-dropped

# Another unmanaged row is held like the fixture: a mv_ prefix alone does not exempt it.
CUSTOM="mv_custom_rollup|2026-10-29 06:30:00+00|2026-10-29 06:30:00+00|success||5|migration-node-002"
write_snapshot "$WORK/unmanaged-row-changed/before" "$FIXTURE" "$CUSTOM"
write_snapshot "$WORK/unmanaged-row-changed/after" "$FIXTURE" "${CUSTOM/|5|/|6|}"
expect fail unmanaged-row-changed

# The exemption is matview_refresh_state's alone: other tables are still compared.
write_snapshot "$WORK/other-table-changed/before" "$FIXTURE" "$OLD_FILTER"
write_snapshot "$WORK/other-table-changed/after" "$FIXTURE" "$NEW_FILTER"
printf '%s\n%s\n' "# COLUMNS: id,name" "$TEAM_AFTER" > "$WORK/other-table-changed/after/governance_teams.csv"
expect fail other-table-changed

if [ "$failures" -ne 0 ]; then
  echo "$failures case(s) failed" >&2
  exit 1
fi
echo "All matview_refresh_state comparison cases passed"
