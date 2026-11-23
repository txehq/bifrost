# shellcheck shell=bash disable=SC2034 # the FIXTURE_* values are read by name, ${!var}, and by sourcing scripts
# The one home of the fixture-folder job's containers (TXE-6085): the network they share, each
# fixture's address, the ports the job's egress allowlist opens to it, its digest-pinned image and
# the compose service that image must match, and the only code that starts or replaces them.
# start-fixture-*.sh source this file and call fixture_run; they name no address, image, network
# or `docker rm` of their own.
#
# Ownership. Every container and the network carry the label $FIXTURE_OWNER_LABEL, whose value is
# this job run ($GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT-$GITHUB_JOB). fixture_run removes a container of
# the same name only when it carries this run's label, so a rerun replaces its own container and
# nothing else. A same-name container with any other label, or none, is refused and left running,
# as is a same-name network that is not this run's or has another subnet. Outside GitHub Actions
# the scripts refuse to start: a local run uses tests/docker-compose.yml under a project name of
# its own, which never shares these names, addresses or ports.
#
# ⚠️ ENFORCED. check-fixture-containers.py (API collection offline suites) fails when:
#   - a fixture's image is not the image of the compose service it names;
#   - a job that runs a start-fixture script lacks an allowlist entry for a port listed here, or
#     allows an address on this subnet that no fixture it starts owns;
#   - that job's ci-images step does not pull this file;
#   - a start-fixture script runs docker itself, names an address, or does not call fixture_run;
#   - two fixtures share an address or a container name, or an address is outside the subnet.
# fixture-containers.test.sh drives fixture_run against a stub docker: own container replaced,
# foreign container and foreign network refused, and no start outside GitHub Actions.

FIXTURE_NETWORK="fixture-folders"
FIXTURE_SUBNET="172.39.0.0/24"
FIXTURE_OWNER_LABEL="org.txehq.bifrost.fixture-owner"

# Harness folder 276 (warp-preservation, TXE-7322). Ports: HTTP 8080, the gossip and data ports
# 7946/7947 it binds, and 8301, which release-pipeline.yml allows for the same image.
FIXTURE_WEAVIATE_CONTAINER="fixture-weaviate"
FIXTURE_WEAVIATE_ADDRESS="172.39.0.12"
FIXTURE_WEAVIATE_PORTS="7946 7947 8080 8301"
FIXTURE_WEAVIATE_COMPOSE=".github/workflows/configs/docker-compose.yml weaviate"
FIXTURE_WEAVIATE_IMAGE="${BIFROST_CI_REGISTRY:-docker.io}/semitechnologies/weaviate:1.32.4@sha256:920463cf9a9b2b8d38e5b925d63f6928fa703277c93df11d5a75fc75e1fff758"

# Harness folder 671 (ClickHouse user agent mappings, TXE-6085). Native protocol only.
FIXTURE_CLICKHOUSE_CONTAINER="fixture-clickhouse"
FIXTURE_CLICKHOUSE_ADDRESS="172.39.0.13"
FIXTURE_CLICKHOUSE_PORTS="9000"
FIXTURE_CLICKHOUSE_COMPOSE="tests/docker-compose.yml clickhouse"
FIXTURE_CLICKHOUSE_IMAGE="${BIFROST_CI_REGISTRY:-docker.io}/clickhouse/clickhouse-server:24.8-alpine@sha256:b002e56ed5c16e224c312527f6fcba7e77216fec5d7a88a7828f59efc614feb5"

# Prints this job run's owner value, or refuses outside GitHub Actions.
fixture_owner() {
	if [ "${GITHUB_ACTIONS:-}" != "true" ] || [ -z "${GITHUB_RUN_ID:-}" ]; then
		echo "fixture-containers: refusing to start outside GitHub Actions; locally use tests/docker-compose.yml with a project name of your own" >&2
		return 1
	fi
	printf '%s-%s-%s\n' "$GITHUB_RUN_ID" "${GITHUB_RUN_ATTEMPT:-1}" "${GITHUB_JOB:-job}"
}

# fixture_run KEY [docker run options...] [-- image arguments...]
# Starts fixture KEY (WEAVIATE, CLICKHOUSE) detached on the fixture network at its address, after
# replacing a container of its name only when this run owns it. Prints nothing on success.
fixture_run() {
	local key="$1"
	shift
	local name_var="FIXTURE_${key}_CONTAINER" address_var="FIXTURE_${key}_ADDRESS" image_var="FIXTURE_${key}_IMAGE"
	local name="${!name_var:-}" address="${!address_var:-}" image="${!image_var:-}"
	if [ -z "$name" ] || [ -z "$address" ] || [ -z "$image" ]; then
		echo "fixture-containers: unknown fixture '$key'" >&2
		return 1
	fi
	local owner
	owner="$(fixture_owner)" || return 1

	if docker network inspect "$FIXTURE_NETWORK" >/dev/null 2>&1; then
		local net_owner net_subnet
		net_owner="$(docker network inspect -f "{{ index .Labels \"$FIXTURE_OWNER_LABEL\" }}" "$FIXTURE_NETWORK")"
		net_subnet="$(docker network inspect -f '{{ range .IPAM.Config }}{{ .Subnet }} {{ end }}' "$FIXTURE_NETWORK")"
		if [ "$net_owner" != "$owner" ] || [ "${net_subnet% }" != "$FIXTURE_SUBNET" ]; then
			echo "fixture-containers: network $FIXTURE_NETWORK exists but is not this job's (owner '${net_owner}', subnet '${net_subnet% }'); leaving it alone" >&2
			return 1
		fi
	else
		docker network create --subnet "$FIXTURE_SUBNET" --label "$FIXTURE_OWNER_LABEL=$owner" "$FIXTURE_NETWORK" >/dev/null
	fi

	if docker container inspect "$name" >/dev/null 2>&1; then
		local c_owner
		c_owner="$(docker container inspect -f "{{ index .Config.Labels \"$FIXTURE_OWNER_LABEL\" }}" "$name")"
		if [ "$c_owner" != "$owner" ]; then
			echo "fixture-containers: container $name exists but is not this job's (owner '${c_owner}'); leaving it alone" >&2
			return 1
		fi
		docker rm -f "$name" >/dev/null
	fi

	local opts=() args=()
	while [ $# -gt 0 ]; do
		if [ "$1" = "--" ]; then
			shift
			args=("$@")
			break
		fi
		opts+=("$1")
		shift
	done
	docker run -d --name "$name" --label "$FIXTURE_OWNER_LABEL=$owner" \
		--network "$FIXTURE_NETWORK" --ip "$address" \
		${opts[@]+"${opts[@]}"} "$image" ${args[@]+"${args[@]}"} >/dev/null
}

# fixture_export VAR VALUE: hands a fixture's URL to the next steps.
fixture_export() {
	echo "$1=$2" >>"$GITHUB_ENV"
}
