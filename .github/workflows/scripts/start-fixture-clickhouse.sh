#!/usr/bin/env bash
# Starts the ClickHouse that harness folder 671 (ClickHouse user agent mappings) needs, for the
# fixture-folder job (TXE-6085). The gateway's ClickHouse log store speaks ClickHouse's native
# protocol, and no loopback fixture stands in for ClickHouse, so the job runs the real one:
#
#   - its image (the one tests/docker-compose.yml's clickhouse service uses, so the GHCR mirror
#     already carries it), address, network and allowed port live in fixture-containers.sh, which
#     also owns starting it: a container of the same name is replaced only when this job run made
#     it, and any other is refused and left running;
#   - published on loopback only (127.0.0.1), native protocol only;
#   - with the database, user and password the clickhouse-logstore group's gateway config names
#     (tests/e2e/api/runners/fixture-folders.mjs). They are fixture values, not credentials.
#
# It waits until ClickHouse answers inside the container, and writes FIXTURE_CLICKHOUSE_URL to
# $GITHUB_ENV for fixture-folders.mjs, whose clickhouse-logstore group reads it. It runs in
# GitHub Actions only; locally, tests/docker-compose.yml's clickhouse service is the one to use.
#
# Usage: start-fixture-clickhouse.sh [host port, default 9001]
set -euo pipefail
# shellcheck source=SCRIPTDIR/fixture-containers.sh
source "$(dirname "${BASH_SOURCE[0]}")/fixture-containers.sh"

PORT="${1:-9001}"

fixture_run CLICKHOUSE \
	-p "127.0.0.1:${PORT}:9000" \
	--ulimit nofile=262144:262144 \
	-e CLICKHOUSE_DB=bifrost \
	-e CLICKHOUSE_USER=bifrost \
	-e CLICKHOUSE_PASSWORD=bifrost_password

url="clickhouse://127.0.0.1:${PORT}"
for _ in $(seq 1 90); do
	if docker exec "$FIXTURE_CLICKHOUSE_CONTAINER" clickhouse-client --user bifrost --password bifrost_password --query "SELECT 1" >/dev/null 2>&1; then
		echo "ClickHouse ready at $url"
		fixture_export FIXTURE_CLICKHOUSE_URL "$url"
		exit 0
	fi
	sleep 2
done
echo "ClickHouse did not become ready at $url within 180s" >&2
docker logs --tail 50 "$FIXTURE_CLICKHOUSE_CONTAINER" >&2 || true
exit 1
