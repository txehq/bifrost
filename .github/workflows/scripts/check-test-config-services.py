#!/usr/bin/env python3
"""Fails when a CI job boots a test config that names a service the job does not start (TXE-8090).

The sync.15 Release Pipeline's test-core-integrations died at startup: upstream had given
tests/integrations/python/config.json a Weaviate vector store, the gateway treats an unreachable
vector store as fatal, and no job that boots that config starts Weaviate. test-cli-harness had
already met the same failure and dropped the block from its own copy, so the fix stayed in one
job and the next one broke.

The rule. A job boots a config when the config's path, or the directory that holds it, appears
in the job's run steps or in a file they call: a script (.sh, .py, .mjs, .js) or a Makefile
target, followed transitively. A file is followed where it is run (in command position), not
where it is only named, and the repository's checkers and their fixtures are not followed; a
config counts wherever a followed file names it outside a comment, so the reader errs towards
registering too much. Only configs under CONFIG_ROOTS count. A config names a service
when some object in it has a "type" in SERVICE_TYPES and is not "enabled": false: a vector
store (weaviate, redis, qdrant, ...) or a postgres config or logs store.

Every (config, job) pair found that way is in exactly one of:

  REGISTRY    config -> job -> {service: text}. For each service the config names, the text
              is what the job runs to start that service, or to take it out of the copy it
              boots; it must appear in the job's run text (the steps, the files they call, and
              the job's services: block). A service the config no longer names fails as stale.
  NOT_BOOTED  pairs where the job reads the config but starts no gateway on it, with the reason.

A pair in neither fails, and so does an entry for a pair that is no longer found, so neither
list can go stale. The check reads the configs as committed: a script that merges a store into
a copy at run time is outside it, and so is a config written inline by a script.

Usage: check-test-config-services.py [--root DIR] [--self-test]
"""
import json
import os
import re
import sys

import yaml

ROOT = os.path.normpath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", ".."))

# Where the test configs that CI boots live.
CONFIG_ROOTS = ("tests/", ".github/workflows/configs/", "examples/configs/")

# An object with one of these as its "type" is a service the gateway dials at boot.
SERVICE_TYPES = {"weaviate", "redis", "valkey", "qdrant", "pinecone", "postgres", "mysql", "clickhouse", "kafka", "nats"}

# config -> "workflow:job" -> {service type: text the job runs that starts or removes it}.
# An empty map: the config names no service, so the job needs nothing started.
REGISTRY = {
    # The provider harness (harness-gateway.sh), the CLI harness, the core integrations suite
    # and its offline startup check. Named no Weaviate before TXE-8090; sync.15's
    # test-core-integrations died dialling the one it named.
    "tests/integrations/python/config.json": {
        "go-unit-tests.yml:core-integrations": {},
        "release-pipeline.yml:test-core": {},
        "release-pipeline.yml:test-core-integrations": {},
        "release-pipeline.yml:test-cli-harness": {},
        "run-core-tests.yml:run-core-tests": {},
        "pr-tests.yml:run-tests": {},
    },
    # test-api-integrations.sh starts the compose stack (Postgres among it) before the gateway.
    "tests/config.json": {
        "go-unit-tests.yml:api-integrations": {"postgres": 'docker compose -f "$COMPOSE_FILE" up -d'},
        "release-pipeline.yml:test-api-integrations": {"postgres": 'docker compose -f "$COMPOSE_FILE" up -d'},
    },
    "tests/live/config.json": {
        "go-unit-tests.yml:live-fake": {},
        "live-tests.yml:live-fake": {},
        "live-tests.yml:live-long": {},
        "release-pipeline.yml:test-live": {},
    },
    "tests/governance/config.json": {
        "pr-tests.yml:run-tests": {},
    },
    # fixture-folders.mjs boots one gateway per fixture group, each on its own config.
    "tests/e2e/api/provider_config/a2a-push.config.json": {"go-unit-tests.yml:fixture-folders": {}},
    "tests/e2e/api/provider_config/azure-stream-preamble.config.json": {"go-unit-tests.yml:fixture-folders": {}},
    "tests/e2e/api/provider_config/databricks-oauth.config.json": {"go-unit-tests.yml:fixture-folders": {}},
    "tests/e2e/api/provider_config/provider-error-fixture.config.json": {"go-unit-tests.yml:fixture-folders": {}},
    "tests/e2e/api/provider_config/routing-error-fixture.config.json": {"go-unit-tests.yml:fixture-folders": {}},
    "tests/e2e/api/provider_config/stream-retry.config.json": {"go-unit-tests.yml:fixture-folders": {}},
    "tests/e2e/api/provider_config/video-settlement.config.json": {"go-unit-tests.yml:fixture-folders": {}},
    "tests/e2e/api/provider_config/warp-wire.config.json": {
        "go-unit-tests.yml:fixture-folders": {"weaviate": "start-fixture-weaviate.sh"},
    },
}

# (config, "workflow:job") -> why the job reads the config without booting a gateway on it.
NOT_BOOTED = {
    ("tests/integrations/typescript/config.json", "go-unit-tests.yml:core-integrations"):
        "test-integrations.sh runs the TypeScript suite from that directory, against the gateway it booted on the python config",
    ("tests/integrations/typescript/config.json", "release-pipeline.yml:test-core-integrations"):
        "test-integrations.sh runs the TypeScript suite from that directory, against the gateway it booted on the python config",
    ("tests/integrations/typescript/config.json", "run-core-tests.yml:run-core-tests"):
        "test-integrations.sh runs the TypeScript suite from that directory, against the gateway it booted on the python config",
    ("examples/configs/withlocalpricingfiles/config.json", "go-unit-tests.yml:api-integrations"):
        "test-api-integrations.sh serves that directory's pricing snapshots; its gateway boots tests/config.json",
    ("examples/configs/withlocalpricingfiles/config.json", "release-pipeline.yml:test-api-integrations"):
        "test-api-integrations.sh serves that directory's pricing snapshots; its gateway boots tests/config.json",
}

CALLABLE = (".sh", ".py", ".mjs", ".js")
PATH_TOKEN = re.compile(r"(?<![\w.-])([\w.-]+(?:/[\w.*-]+)+)")
# A script in command position: at the start of a line or after ; & | ( $( then do else, or as
# the argument of bash, sh, source, ., exec, python, node or a wrapper that runs its arguments
# (scripts/test-offline.sh, env, timeout). A file named anywhere else, an echo or an error
# message, is not followed.
INVOKE = re.compile(
    r"""(?:^\s*|[;&|(]\s*|\$\(\s*|\b(?:then|do|else|bash|sh|source|exec|python3?|node|timeout\s+\S+|test-offline\.sh)\s+|^\s*\.\s+)"""
    r"""(?:[A-Za-z_]\w*=\S*\s+)*(?:env\s+(?:\S+=\S*\s+|"\$\{[^}]+\}"\s+)*)?(?:node\s+|bash\s+|python3?\s+)?"""
    r"""["']?([^\s"';|&()]+\.(?:sh|py|mjs|js))(?=["'\s;|&)]|$)""",
    re.M,
)
# Not followed: the repository's checkers and their tests. They start no gateway, and their
# fixtures hold workflow text whose run steps would otherwise read as this job's.
NOT_FOLLOWED = re.compile(r"\.py$|\.test\.|_test\.|^test_|^check-")
MAKE_CALL = re.compile(r"\bmake\s+(?:-[A-Za-z]\s+\S+\s+|-\S+\s+|[A-Z_]+=\S*\s+)*([a-z][\w-]*)")


def strip_comments(text):
    """Drops whole-line comments (#, //) so a path named only in prose does not count."""
    keep = []
    for line in text.splitlines():
        stripped = line.lstrip()
        if stripped.startswith("#") and not stripped.startswith("#!"):
            continue
        if stripped.startswith("//"):
            continue
        keep.append(line)
    return "\n".join(keep)


class Tree:
    """Reads files under one repository root, cached."""

    def __init__(self, root):
        self.root = root
        self._text = {}
        self._makefile = None

    def exists(self, rel):
        return os.path.exists(os.path.join(self.root, rel))

    def isdir(self, rel):
        return os.path.isdir(os.path.join(self.root, rel))

    def text(self, rel):
        if rel not in self._text:
            try:
                with open(os.path.join(self.root, rel), encoding="utf-8") as f:
                    self._text[rel] = strip_comments(f.read())
            except (OSError, UnicodeDecodeError):
                self._text[rel] = ""
        return self._text[rel]

    def make_recipe(self, target):
        if self._makefile is None:
            self._makefile = {}
            current = None
            for line in self.text("Makefile").splitlines():
                head = re.match(r"^([a-z][\w-]*)\s*:(?!=)", line)
                if head:
                    current = head.group(1)
                    self._makefile.setdefault(current, [])
                elif line.startswith("\t") and current:
                    self._makefile[current].append(line)
                elif line.strip() and not line.startswith((" ", "\t")):
                    current = None
        return "\n".join(self._makefile.get(target, []))


def path_candidates(token, base_dir):
    """Repository-relative paths a token in a file under base_dir may name."""
    while token.startswith("./"):
        token = token[2:]
    out = [token]
    for marker in CONFIG_ROOTS + (".github/", "scripts/", "examples/", "tests/", "transports/"):
        i = token.find(marker)
        if i > 0:
            out.append(token[i:])
    out.append(os.path.normpath(os.path.join(base_dir, token)))
    # "$SCRIPT_DIR/x.sh", "$(dirname "$0")/x.sh": a file next to the caller.
    out.append(os.path.normpath(os.path.join(base_dir, os.path.basename(token))))
    return out


def job_closure(tree, text, base_dir="."):
    """The text of every file the given run text calls, transitively, joined with it."""
    seen = set()
    parts = [text]
    queue = [(text, base_dir)]
    while queue:
        body, where = queue.pop()
        for target in MAKE_CALL.findall(body):
            key = "make:" + target
            if key not in seen:
                seen.add(key)
                recipe = tree.make_recipe(target)
                parts.append(recipe)
                queue.append((recipe, "."))
        for token in INVOKE.findall(body):
            token = re.sub(r"^\$(?:\{\w+\}|\w+|\([^)]*\))/", "", token)
            if NOT_FOLLOWED.search(os.path.basename(token)):
                continue
            for cand in path_candidates(token, where):
                if cand not in seen and tree.exists(cand) and not tree.isdir(cand):
                    seen.add(cand)
                    called = tree.text(cand)
                    parts.append(called)
                    queue.append((called, os.path.dirname(cand)))
                    break
    return "\n".join(parts)


def configs_named(tree, text):
    """Configs under CONFIG_ROOTS that the text names, by file or by the directory holding one."""
    found = set()
    for token in PATH_TOKEN.findall(text):
        for cand in path_candidates(token, "."):
            if not cand.startswith(CONFIG_ROOTS):
                continue
            if cand.endswith(".json") and "config" in os.path.basename(cand) and tree.exists(cand):
                found.add(cand)
                break
            if tree.isdir(cand) and tree.exists(os.path.join(cand, "config.json")):
                found.add(os.path.join(cand, "config.json"))
                break
    return found


def services_named(config):
    """{service type: [JSON path, ...]} for every enabled object whose type is a service."""
    out = {}

    def walk(node, path):
        if isinstance(node, dict):
            if node.get("enabled") is False:
                return
            kind = node.get("type")
            if isinstance(kind, str) and kind.lower() in SERVICE_TYPES:
                out.setdefault(kind.lower(), []).append(path or "/")
            for key, value in node.items():
                walk(value, f"{path}/{key}")
        elif isinstance(node, list):
            for i, value in enumerate(node):
                walk(value, f"{path}[{i}]")

    walk(config, "")
    return out


def jobs_of(tree):
    """'workflow:job' -> (run text with everything it calls, the job's services: block)."""
    out = {}
    wf_dir = os.path.join(tree.root, ".github", "workflows")
    for name in sorted(os.listdir(wf_dir)):
        if not name.endswith((".yml", ".yaml")):
            continue
        with open(os.path.join(wf_dir, name), encoding="utf-8") as f:
            workflow = yaml.safe_load(f) or {}
        for job_id, job in (workflow.get("jobs") or {}).items():
            if not isinstance(job, dict) or not job.get("steps"):
                continue
            runs = "\n".join(strip_comments(str(s.get("run") or "")) for s in job["steps"] if isinstance(s, dict))
            text = job_closure(tree, runs)
            services = yaml.safe_dump(job.get("services")) if job.get("services") else ""
            out[f"{name}:{job_id}"] = (text, services)
    return out


def problems(found, configs, job_text, registry, not_booted):
    """found: {(config, job)}. configs: config -> parsed JSON. job_text: job -> text.
    Returns one line per defect; empty passes."""
    out = []
    registered = {(c, j) for c, jobs in registry.items() for j in jobs}
    for pair in sorted(found):
        config, job = pair
        if pair in registered and pair in not_booted:
            out.append(f"{job} -> {config}: in both REGISTRY and NOT_BOOTED")
        elif pair not in registered and pair not in not_booted:
            named = sorted(services_named(configs.get(config, {})))
            out.append(f"{job} boots {config} (services: {', '.join(named) or 'none'}), which REGISTRY does not list: "
                       "register the pair with what the job runs to start each service, or list it in NOT_BOOTED with the reason")
    for pair in sorted((registered | set(not_booted)) - set(found)):
        config, job = pair
        table = "REGISTRY" if pair in registered else "NOT_BOOTED"
        out.append(f"{table} lists {job} -> {config}, but the job no longer names that config: remove the entry")
    for config, jobs in sorted(registry.items()):
        named = services_named(configs.get(config, {}))
        for job, handled in sorted(jobs.items()):
            if (config, job) not in found:
                continue
            text = job_text.get(job, "")
            for service, where in sorted(named.items()):
                if service not in handled:
                    out.append(f"{job} boots {config}, which names a {service} service at {', '.join(where)}, and the job "
                               f"does not start it: start it in the job (a services: container or a step), or take it out "
                               f"of the config the job boots, and record that in REGISTRY")
                elif handled[service] not in text:
                    out.append(f"REGISTRY[{config}][{job}]: the job no longer runs '{handled[service]}' for {service}; update the entry")
            for service in sorted(set(handled) - set(named)):
                out.append(f"REGISTRY[{config}][{job}] handles {service}, which the config no longer names: remove it")
    return out


def scan(root):
    tree = Tree(root)
    jobs = jobs_of(tree)
    found = set()
    job_text = {}
    for job, (text, services) in jobs.items():
        job_text[job] = text + "\n" + services
        for config in configs_named(tree, text):
            found.add((config, job))
    configs = {}
    for config, _ in found:
        try:
            with open(os.path.join(root, config), encoding="utf-8") as f:
                configs[config] = json.load(f)
        except (OSError, ValueError) as err:
            configs[config] = {}
            print(f"warning: {config} is not readable JSON ({err})", file=sys.stderr)
    return found, configs, job_text


def self_test():
    """Each rule on a small tree and on hand-built inputs."""
    import tempfile

    failures = []

    def expect(name, got, want):
        if got != want:
            failures.append(f"{name}: got {got!r}, want {want!r}")

    weaviate = {"vector_store": {"enabled": True, "type": "weaviate", "config": {"host": "localhost:9000"}}}
    expect("an enabled vector store is a service", services_named(weaviate), {"weaviate": ["/vector_store"]})
    expect("a disabled one is not", services_named({"vector_store": {"enabled": False, "type": "weaviate"}}), {})
    expect("sqlite stores are not",
           services_named({"config_store": {"type": "sqlite"}, "logs_store": {"type": "sqlite"}}), {})
    expect("postgres stores are", services_named({"logs_store": {"enabled": True, "type": "postgres"}}),
           {"postgres": ["/logs_store"]})

    with tempfile.TemporaryDirectory() as root:
        def write(rel, body):
            os.makedirs(os.path.dirname(os.path.join(root, rel)), exist_ok=True)
            with open(os.path.join(root, rel), "w", encoding="utf-8") as f:
                f.write(body)

        write("tests/app/config.json", json.dumps(weaviate))
        write("tests/other/config.json", "{}")
        write(".github/workflows/scripts/boot.sh",
              '#!/bin/sh\n# tests/other is named only in this comment\nbifrost-http -app-dir "$REPO_ROOT/tests/app"\n')
        write(".github/workflows/scripts/other.sh", 'bifrost-http -app-dir tests/other\n')
        write(".github/workflows/scripts/helper.sh",
              'source "$SCRIPT_DIR/boot.sh"\necho "see .github/workflows/scripts/other.sh"\n')
        write("Makefile", "boot-target:\n\tbash .github/workflows/scripts/helper.sh\n")
        write(".github/workflows/ci.yml", yaml.safe_dump({
            "on": {"pull_request": None},
            "jobs": {
                "direct": {"runs-on": "x", "steps": [{"run": "./.github/workflows/scripts/boot.sh"}]},
                "via-make": {"runs-on": "x", "steps": [{"run": "make LOCAL=1 boot-target"}]},
                "started": {"runs-on": "x", "steps": [{"run": "docker run weaviate\n./.github/workflows/scripts/boot.sh"}]},
                "named-only": {"runs-on": "x", "steps": [{"run": "echo .github/workflows/scripts/boot.sh"}]},
            },
        }))
        found, configs, job_text = scan(root)
        expect("discovery follows a script, a sourced script and a make target; not an echo or a comment",
               sorted(found), [("tests/app/config.json", "ci.yml:direct"), ("tests/app/config.json", "ci.yml:started"),
                               ("tests/app/config.json", "ci.yml:via-make")])

        app = "tests/app/config.json"
        ok = {app: {"ci.yml:direct": {"weaviate": "boot.sh"}, "ci.yml:started": {"weaviate": "docker run weaviate"},
                    "ci.yml:via-make": {"weaviate": "boot.sh"}}}
        expect("every service started: passes", problems(found, configs, job_text, ok, {}), [])
        unregistered = {app: {k: v for k, v in ok[app].items() if k != "ci.yml:direct"}}
        out = problems(found, configs, job_text, unregistered, {})
        expect("an unregistered pair fails", len(out) == 1 and "REGISTRY does not list" in out[0], True)
        missing = {app: dict(ok[app], **{"ci.yml:started": {}})}
        out = problems(found, configs, job_text, missing, {})
        expect("a named service the job does not start fails",
               len(out) == 1 and "names a weaviate service at /vector_store" in out[0], True)
        stale_text = {app: dict(ok[app], **{"ci.yml:started": {"weaviate": "docker compose up weaviate"}})}
        out = problems(found, configs, job_text, stale_text, {})
        expect("a handler the job no longer runs fails", len(out) == 1 and "no longer runs" in out[0], True)
        extra = {app: dict(ok[app], **{"ci.yml:started": {"weaviate": "docker run weaviate", "redis": "docker run"}})}
        out = problems(found, configs, job_text, extra, {})
        expect("a service the config no longer names fails", len(out) == 1 and "no longer names: remove it" in out[0], True)
        gone = dict(ok, **{"tests/other/config.json": {"ci.yml:direct": {}}})
        out = problems(found, configs, job_text, gone, {})
        expect("an entry for a pair no longer found fails", len(out) == 1 and "remove the entry" in out[0], True)
        both = problems(found, configs, job_text, ok, {(app, "ci.yml:direct"): "reads it"})
        expect("a pair in both tables fails", len(both) == 1 and "in both" in both[0], True)

    for line in failures:
        print(f"FAIL {line}")
    print(f"check-test-config-services self-test: {12 - len(failures)} of 12 cases passed")
    return 1 if failures else 0


def main(argv):
    if "--self-test" in argv:
        return self_test()
    root = ROOT
    if "--root" in argv:
        root = os.path.abspath(argv[argv.index("--root") + 1])
    found, configs, job_text = scan(root)
    if "--list" in argv:
        for config, job in sorted(found):
            print(f"{config}\t{job}\t{','.join(sorted(services_named(configs[config])))}")
        return 0
    defects = problems(found, configs, job_text, REGISTRY, NOT_BOOTED)
    for line in defects:
        print(f"FAIL {line}")
    if defects:
        return 1
    print(f"Test config services: {len(found)} (config, job) pairs, {len(REGISTRY)} configs registered, "
          f"{len(NOT_BOOTED)} pairs that boot nothing; every service a booted config names is started by its job.")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
