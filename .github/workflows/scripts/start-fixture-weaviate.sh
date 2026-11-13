#!/usr/bin/env bash
# Starts the Weaviate that harness folder 276 (warp-preservation) needs, for the fixture-folder
# job (TXE-7322). Warp keeps its namespaces in a vector store, and no loopback fixture stands in
# for Weaviate, so the job runs the real one:
#
#   - pinned by image digest, the one .github/workflows/configs/docker-compose.yml uses;
#   - on a network of its own with a fixed address, because Harden Runner filters on the
#     post-NAT destination, so the job's allowlist names that address and its ports;
#   - published on loopback only (127.0.0.1), so nothing off the runner can reach it;
#   - anonymous, with no vectorizer module: the gateway sends its own vectors.
#
# It waits until Weaviate is ready, and writes FIXTURE_WEAVIATE_URL to $GITHUB_ENV (or prints it)
# for fixture-folders.mjs, whose warp-wire group reads it.
#
# Usage: start-fixture-weaviate.sh [host port, default 8080]
set -euo pipefail

IMAGE="cr.weaviate.io/semitechnologies/weaviate:1.32.4@sha256:920463cf9a9b2b8d38e5b925d63f6928fa703277c93df11d5a75fc75e1fff758"
NETWORK="fixture-folders"
SUBNET="172.39.0.0/24"
ADDRESS="172.39.0.12"   # also in go-unit-tests.yml's fixture-folders allowlist
NAME="fixture-weaviate"
PORT="${1:-8080}"

docker network inspect "$NETWORK" >/dev/null 2>&1 || docker network create --subnet "$SUBNET" "$NETWORK" >/dev/null
docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" --network "$NETWORK" --ip "$ADDRESS" \
	-p "127.0.0.1:${PORT}:8080" \
	-e CLUSTER_HOSTNAME=weaviate \
	-e CLUSTER_ADVERTISE_ADDR="$ADDRESS" \
	-e CLUSTER_GOSSIP_BIND_PORT=7946 \
	-e CLUSTER_DATA_BIND_PORT=7947 \
	-e DISABLE_TELEMETRY=true \
	-e PERSISTENCE_DATA_PATH=/var/lib/weaviate \
	-e DEFAULT_VECTORIZER_MODULE=none \
	-e ENABLE_MODULES= \
	-e AUTHENTICATION_ANONYMOUS_ACCESS_ENABLED=true \
	-e LOG_LEVEL=warning \
	"$IMAGE" --host 0.0.0.0 --port 8080 --scheme http >/dev/null

url="http://127.0.0.1:${PORT}"
for _ in $(seq 1 90); do
	if curl -fsS -o /dev/null "$url/v1/.well-known/ready"; then
		echo "Weaviate ready at $url"
		if [ -n "${GITHUB_ENV:-}" ]; then
			echo "FIXTURE_WEAVIATE_URL=$url" >>"$GITHUB_ENV"
		else
			echo "FIXTURE_WEAVIATE_URL=$url"
		fi
		exit 0
	fi
	sleep 2
done
echo "Weaviate did not become ready at $url within 180s" >&2
docker logs --tail 50 "$NAME" >&2 || true
exit 1
