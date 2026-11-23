#!/usr/bin/env python3
"""Fails when the fixture-folder containers drift from their one home (TXE-6085).

fixture-containers.sh holds, for each fixture the fixture-folder job runs as a container, its
container name, its address on the shared fixture network, the ports the job's egress allowlist
opens to it, its digest-pinned image and the compose service that image must match. It also holds
the only code that starts one (fixture_run, which replaces a container only when this job run
owns it). This check fails when:

  1. a fixture lacks one of CONTAINER, ADDRESS, PORTS, COMPOSE or IMAGE; two fixtures share a
     container name or an address; an address is outside FIXTURE_SUBNET or is its network or
     gateway address;
  2. a fixture's image is not the image of the compose service it names (so the GHCR mirror,
     which mirrors the compose files' images, carries it, and the two cannot drift apart);
  3. a start-fixture-*.sh does not source fixture-containers.sh, does not call fixture_run with a
     known fixture, runs docker run/rm/network itself, or names an address on its own;
  4. a workflow job that runs a start-fixture script blocks egress without allowing every
     address:port of each fixture it starts, or allows an address on the fixture subnet that no
     fixture it starts owns (a stale entry);
  5. that job's ci-images step does not pull fixture-containers.sh, where the images now live.

Usage: check-fixture-containers.py [--self-test]
"""
import ipaddress
import re
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parents[3]
SCRIPTS = ".github/workflows/scripts"
HOME = f"{SCRIPTS}/fixture-containers.sh"
FIELDS = ("CONTAINER", "ADDRESS", "PORTS", "COMPOSE", "IMAGE")
REGISTRY_PREFIX = re.compile(r"^\$\{BIFROST_CI_REGISTRY:-docker\.io\}/")


def parse_home(text):
    """(network, subnet, {KEY: {FIELD: value}}) from fixture-containers.sh's assignments."""
    values = dict(re.findall(r'^(FIXTURE_[A-Z_]+)="([^"]*)"\s*(?:#.*)?$', text, re.MULTILINE))
    fixtures = {}
    for name, value in values.items():
        m = re.match(r"FIXTURE_([A-Z0-9]+)_(" + "|".join(FIELDS) + r")$", name)
        if m:
            fixtures.setdefault(m.group(1), {})[m.group(2)] = value
    return values.get("FIXTURE_NETWORK"), values.get("FIXTURE_SUBNET"), fixtures


def compose_image(text, service):
    """The image of a compose service, read by indentation (no YAML dependency on PRs)."""
    lines = text.splitlines()
    for i, line in enumerate(lines):
        m = re.match(r"^(\s+)" + re.escape(service) + r":\s*$", line)
        if not m:
            continue
        indent = len(m.group(1))
        for inner in lines[i + 1:]:
            if inner.strip() and len(inner) - len(inner.lstrip()) <= indent:
                break
            im = re.match(r"^\s+image:\s*[\"']?([^\"'\s#]+)", inner)
            if im:
                return im.group(1)
    return None


def strip_registry(ref):
    return REGISTRY_PREFIX.sub("", ref)


def job_blocks(text):
    """(job name, job text) for each job under a workflow's top-level jobs: map."""
    m = re.search(r"^jobs:\s*$", text, re.MULTILINE)
    if not m:
        return []
    body = text[m.end():]
    starts = list(re.finditer(r"^  ([A-Za-z0-9_-]+):\s*$", body, re.MULTILINE))
    return [(s.group(1), body[s.start():(starts[i + 1].start() if i + 1 < len(starts) else len(body))])
            for i, s in enumerate(starts)]


def allowed_endpoints(job):
    m = re.search(r"allowed-endpoints:\s*>-?\s*\n((?:\s+\S+\s*\n)+)", job)
    return set(m.group(1).split()) if m else None


def check(files):
    """files maps a repo path to its text. Returns problems."""
    problems = []
    home = files.get(HOME)
    if home is None:
        return [f"{HOME} is missing"]
    network, subnet, fixtures = parse_home(home)
    if not network or not subnet:
        problems.append(f"{HOME}: FIXTURE_NETWORK and FIXTURE_SUBNET must both be set")
        return problems
    net = ipaddress.ip_network(subnet)
    seen_names, seen_addresses = {}, {}
    for key, f in sorted(fixtures.items()):
        missing = [x for x in FIELDS if not f.get(x)]
        if missing:
            problems.append(f"{HOME}: fixture {key} lacks FIXTURE_{key}_{', FIXTURE_{key}_'.join(missing)}")
            fixtures.pop(key)
            continue
        for kind, value, seen in (("container name", f["CONTAINER"], seen_names), ("address", f["ADDRESS"], seen_addresses)):
            if value in seen:
                problems.append(f"{HOME}: fixtures {seen[value]} and {key} share the {kind} {value}")
            seen[value] = key
        addr = ipaddress.ip_address(f["ADDRESS"])
        if addr not in net or addr in (net.network_address, net.network_address + 1, net.broadcast_address):
            problems.append(f"{HOME}: fixture {key}'s address {addr} is not a host address on {subnet} (the gateway is .1)")
        compose_path, _, service = f["COMPOSE"].partition(" ")
        compose = files.get(compose_path)
        image = compose_image(compose, service) if compose is not None else None
        if image is None:
            problems.append(f"{HOME}: fixture {key} names compose service '{service}' in {compose_path}, which has no image")
        elif strip_registry(image) != strip_registry(f["IMAGE"]):
            problems.append(f"{HOME}: fixture {key}'s image {strip_registry(f['IMAGE'])} is not {compose_path}'s {service} image {strip_registry(image)}")

    started_by = {}
    for path, text in sorted(files.items()):
        name = path.rsplit("/", 1)[-1]
        if not (path.startswith(SCRIPTS + "/") and re.match(r"start-fixture-.*\.sh$", name)):
            continue
        code = "\n".join(l for l in text.splitlines() if not l.lstrip().startswith("#"))
        if not re.search(r'^source "\$\(dirname "\$\{BASH_SOURCE\[0\]\}"\)/fixture-containers\.sh"$', code, re.MULTILINE):
            problems.append(f"{path}: must source fixture-containers.sh")
        keys = re.findall(r"^fixture_run ([A-Z0-9]+)\b", code, re.MULTILINE)
        if len(keys) != 1:
            problems.append(f"{path}: must call fixture_run exactly once (found {len(keys)})")
        for k in keys:
            if k not in fixtures:
                problems.append(f"{path}: fixture_run names {k}, which {HOME} does not define")
            started_by[name] = k
        for bad in re.findall(r"\bdocker\s+(?:run|rm|create|network)\b", code):
            problems.append(f"{path}: runs `{bad}` itself; only fixture_run starts or removes a fixture")
        if re.search(r"\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b", code.replace("127.0.0.1", "").replace("0.0.0.0", "")):
            problems.append(f"{path}: names an address; use FIXTURE_<KEY>_ADDRESS from {HOME}")

    for path, text in sorted(files.items()):
        if not (path.startswith(".github/workflows/") and path.count("/") == 2 and path.endswith((".yml", ".yaml"))):
            continue
        for job, block in job_blocks(text):
            run = [n for n in started_by if re.search(r"^\s+(?:-\s+)?run:.*\b" + re.escape(n) + r"\b", block, re.MULTILINE)]
            if not run:
                continue
            where = f"{path} :: job '{job}'"
            pull = re.search(r"uses: \./\.github/actions/ci-images\s*\n\s+with:\s*\n\s+pull:\s*(.+)", block)
            if not pull or HOME not in pull.group(1).split():
                problems.append(f"{where} runs {', '.join(run)} but its ci-images step does not pull {HOME}")
            allowed = allowed_endpoints(block)
            if allowed is None:
                continue  # egress not blocked by an allowlist: nothing to drift
            owned = {fixtures[started_by[n]]["ADDRESS"]: started_by[n] for n in run if started_by[n] in fixtures}
            for n in run:
                k = started_by[n]
                if k not in fixtures:
                    continue
                for port in fixtures[k]["PORTS"].split():
                    ep = f"{fixtures[k]['ADDRESS']}:{port}"
                    if ep not in allowed:
                        problems.append(f"{where} starts {k} but its egress allowlist lacks {ep}")
            for ep in sorted(allowed):
                host, _, port = ep.rpartition(":")
                try:
                    a = ipaddress.ip_address(host)
                except ValueError:
                    continue
                if a in net:
                    k = owned.get(host)
                    if k is None:
                        problems.append(f"{where} allows {ep} on the fixture subnet, which no fixture it starts owns")
                    elif port not in fixtures[k]["PORTS"].split():
                        problems.append(f"{where} allows {ep}, a port {HOME} does not list for {k}")
    return problems


def load_files():
    import subprocess
    out = subprocess.run(["git", "-C", str(REPO), "ls-files"], check=True, capture_output=True, text=True).stdout
    files = {}
    for path in out.splitlines():
        if path == HOME or path.startswith(".github/workflows/") or re.search(r"(^|/)docker-compose[\w.-]*\.ya?ml$", path):
            p = REPO / path
            if p.is_file():
                files[path] = p.read_text(encoding="utf-8")
    return files


def self_test():
    home = f'''FIXTURE_NETWORK="n"
FIXTURE_SUBNET="172.39.0.0/24"
FIXTURE_DB_CONTAINER="fixture-db"
FIXTURE_DB_ADDRESS="172.39.0.13"
FIXTURE_DB_PORTS="9000"
FIXTURE_DB_COMPOSE="tests/docker-compose.yml db"
FIXTURE_DB_IMAGE="${{BIFROST_CI_REGISTRY:-docker.io}}/x/db:1@sha256:aa"
'''
    compose = "services:\n    db:\n        image: ${BIFROST_CI_REGISTRY:-docker.io}/x/db:1@sha256:aa\n"
    script = '#!/usr/bin/env bash\nsource "$(dirname "${BASH_SOURCE[0]}")/fixture-containers.sh"\nfixture_run DB -p "127.0.0.1:9001:9000"\n'
    workflow = f"""jobs:
  fx:
    steps:
      - uses: step-security/harden-runner@x
        with:
          egress-policy: block
          allowed-endpoints: >
            172.39.0.13:9000
            ghcr.io:443

      - uses: ./.github/actions/ci-images
        with:
          pull: {HOME}
      - run: bash {SCRIPTS}/start-fixture-db.sh
"""
    good = {HOME: home, "tests/docker-compose.yml": compose, f"{SCRIPTS}/start-fixture-db.sh": script,
            ".github/workflows/w.yml": workflow}
    cases = [
        ("clean", {}, None),
        ("image drift", {"tests/docker-compose.yml": compose.replace("sha256:aa", "sha256:bb")}, "is not tests/docker-compose.yml's db image"),
        ("missing allowlist port", {".github/workflows/w.yml": workflow.replace("172.39.0.13:9000\n", "")}, "lacks 172.39.0.13:9000"),
        ("stale allowlist address", {".github/workflows/w.yml": workflow.replace("ghcr.io:443", "172.39.0.14:9000")}, "no fixture it starts owns"),
        ("unlisted port", {".github/workflows/w.yml": workflow.replace("ghcr.io:443", "172.39.0.13:8123")}, "does not list for DB"),
        ("no pull", {".github/workflows/w.yml": workflow.replace(f"pull: {HOME}", "pull: none")}, "does not pull"),
        ("raw docker rm", {f"{SCRIPTS}/start-fixture-db.sh": script + "docker rm -f fixture-db\n"}, "runs `docker rm`"),
        ("own address", {f"{SCRIPTS}/start-fixture-db.sh": script + 'A="172.39.0.13"\n'}, "names an address"),
        ("no source", {f"{SCRIPTS}/start-fixture-db.sh": script.replace("source", "# source")}, "must source"),
        ("unknown key", {f"{SCRIPTS}/start-fixture-db.sh": script.replace("fixture_run DB", "fixture_run NOPE")}, "does not define"),
        ("gateway address", {HOME: home.replace("172.39.0.13", "172.39.0.1")}, "not a host address"),
        ("missing field", {HOME: home.replace('FIXTURE_DB_PORTS="9000"\n', "")}, "lacks FIXTURE_DB_PORTS"),
    ]
    failed = 0
    for name, change, want in cases:
        got = check({**good, **change})
        ok = (not got) if want is None else any(want in p for p in got)
        if not ok:
            failed += 1
            print(f"self-test FAIL: {name}: expected {want or 'no problem'}, got {got}", file=sys.stderr)
    if failed:
        sys.exit(1)
    print(f"check-fixture-containers.py self-test: {len(cases)} cases passed")


def main():
    if sys.argv[1:] == ["--self-test"]:
        self_test()
        return
    problems = check(load_files())
    if problems:
        for p in problems:
            print(f"FAIL: {p}", file=sys.stderr)
        sys.exit(1)
    _, _, fixtures = parse_home((REPO / HOME).read_text(encoding="utf-8"))
    print(f"Fixture containers: {len(fixtures)} fixtures in {HOME}; images match their compose services; "
          "every job that starts one allows exactly its ports and pulls its image.")


if __name__ == "__main__":
    main()
