#!/usr/bin/env bash
# Starts the Weaviate that harness folder 276 (warp-preservation) needs, for the fixture-folder
# job (TXE-7322). Warp keeps its namespaces in a vector store, and no loopback fixture stands in
# for Weaviate, so the job runs the real one:
#
#   - its image, address, network and allowed ports live in fixture-containers.sh, which also
#     owns starting it: a container of the same name is replaced only when this job run made it;
#   - published on loopback only (127.0.0.1), so nothing off the runner can reach it;
#   - anonymous, with no vectorizer module: the gateway sends its own vectors.
#
# It waits until Weaviate is ready, and writes FIXTURE_WEAVIATE_URL to $GITHUB_ENV for
# fixture-folders.mjs, whose warp-wire group reads it. It runs in GitHub Actions only.
#
# Usage: start-fixture-weaviate.sh [host port, default 8080]
set -euo pipefail
# shellcheck source=SCRIPTDIR/fixture-containers.sh
source "$(dirname "${BASH_SOURCE[0]}")/fixture-containers.sh"

PORT="${1:-8080}"

fixture_run WEAVIATE \
	-p "127.0.0.1:${PORT}:8080" \
	-e CLUSTER_HOSTNAME=weaviate \
	-e CLUSTER_ADVERTISE_ADDR="$FIXTURE_WEAVIATE_ADDRESS" \
	-e CLUSTER_GOSSIP_BIND_PORT=7946 \
	-e CLUSTER_DATA_BIND_PORT=7947 \
	-e DISABLE_TELEMETRY=true \
	-e PERSISTENCE_DATA_PATH=/var/lib/weaviate \
	-e DEFAULT_VECTORIZER_MODULE=none \
	-e ENABLE_MODULES= \
	-e AUTHENTICATION_ANONYMOUS_ACCESS_ENABLED=true \
	-e LOG_LEVEL=warning \
	-- --host 0.0.0.0 --port 8080 --scheme http

url="http://127.0.0.1:${PORT}"
for _ in $(seq 1 90); do
	if curl -fsS -o /dev/null "$url/v1/.well-known/ready"; then
		echo "Weaviate ready at $url"
		fixture_export FIXTURE_WEAVIATE_URL "$url"
		exit 0
	fi
	sleep 2
done
echo "Weaviate did not become ready at $url within 180s" >&2
docker logs --tail 50 "$FIXTURE_WEAVIATE_CONTAINER" >&2 || true
exit 1
