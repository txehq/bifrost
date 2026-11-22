#!/usr/bin/env bash
# Starts the ClickHouse that harness folder 671 (ClickHouse user agent mappings) needs, for the
# fixture-folder job (TXE-6085). The gateway's ClickHouse log store speaks ClickHouse's native
# protocol, and no loopback fixture stands in for ClickHouse, so the job runs the real one, the
# same way start-fixture-weaviate.sh runs Weaviate:
#
#   - pinned by image digest, the one tests/docker-compose.yml's clickhouse service uses (the Go
#     unit shards' ClickHouse), so the GHCR mirror already carries it;
#   - on the fixture network with a fixed address, because Harden Runner filters on the post-NAT
#     destination, so the job's allowlist names that address and its native port;
#   - published on loopback only (127.0.0.1), native protocol only;
#   - with the database, user and password the clickhouse-logstore group's gateway config names
#     (tests/e2e/api/runners/fixture-folders.mjs). They are fixture values, not credentials.
#
# It waits until ClickHouse answers inside the container, and writes FIXTURE_CLICKHOUSE_URL to
# $GITHUB_ENV (or prints it) for fixture-folders.mjs, whose clickhouse-logstore group reads it.
#
# Usage: start-fixture-clickhouse.sh [host port, default 9001]
set -euo pipefail

IMAGE="${BIFROST_CI_REGISTRY:-docker.io}/clickhouse/clickhouse-server:24.8-alpine@sha256:b002e56ed5c16e224c312527f6fcba7e77216fec5d7a88a7828f59efc614feb5"
NETWORK="fixture-folders"
SUBNET="172.39.0.0/24"
ADDRESS="172.39.0.13"   # also in go-unit-tests.yml's fixture-folders allowlist
NAME="${FIXTURE_CLICKHOUSE_CONTAINER:-fixture-clickhouse}"
PORT="${1:-9001}"

docker network inspect "$NETWORK" >/dev/null 2>&1 || docker network create --subnet "$SUBNET" "$NETWORK" >/dev/null
docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" --network "$NETWORK" --ip "$ADDRESS" \
	-p "127.0.0.1:${PORT}:9000" \
	--ulimit nofile=262144:262144 \
	-e CLICKHOUSE_DB=bifrost \
	-e CLICKHOUSE_USER=bifrost \
	-e CLICKHOUSE_PASSWORD=bifrost_password \
	"$IMAGE" >/dev/null

url="clickhouse://127.0.0.1:${PORT}"
for _ in $(seq 1 90); do
	if docker exec "$NAME" clickhouse-client --user bifrost --password bifrost_password --query "SELECT 1" >/dev/null 2>&1; then
		echo "ClickHouse ready at $url"
		if [ -n "${GITHUB_ENV:-}" ]; then
			echo "FIXTURE_CLICKHOUSE_URL=$url" >>"$GITHUB_ENV"
		else
			echo "FIXTURE_CLICKHOUSE_URL=$url"
		fi
		exit 0
	fi
	sleep 2
done
echo "ClickHouse did not become ready at $url within 180s" >&2
docker logs --tail 50 "$NAME" >&2 || true
exit 1
