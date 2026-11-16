#!/usr/bin/env python3
"""A live test runs only on an explicit opt-in, through one helper (TXE-7530).

A plain `go test ./...` in core/ with DEEPSEEK_API_KEY exported called api.deepseek.com until the
package timed out: live suites switched on whenever their key was in the environment. Now
core/livetest gates them: livetest.Require(t, vars...) skips unless BIFROST_LIVE_TESTS=1 and every
named variable is set, and livetest.Key(t, name) returns a credential the same way. A function is
gated when it calls one of them, or calls a function of its own package that is gated (a helper
such as requireIntegrationEnv). This check keeps every live test on the gate:

  1. A function in a _test.go file that reads a credential-named environment variable directly
     (os.Getenv / os.LookupEnv of *_API_KEY, *_TOKEN, *SECRET*, *ACCESS_KEY*, *_CREDENTIALS,
     *_PRIVATE_KEY) must be gated. A credential that only reaches a local service, never a vendor,
     is named in LOCAL_CREDENTIALS with the reason.
  2. A function that builds the live provider harness (llmtests.SetupTest) must be gated: the
     harness reads every vendor key from the environment.
  3. llmtests.SetupTest refuses to build a client unless livetest.Enabled(), and a test account
     (GetKeysForProvider in a _test.go file) that hands out an env.<credential> key refuses unless
     livetest.Enabled(). A live test that loses its gate then fails before it reaches a vendor.
  4. The gated top-level tests are exactly the ones in live-tests.txt. A test that loses its gate
     (a helper call deleted) or a new live test that is not listed fails here, so the stay-off
     proof, which runs the listed tests, always covers every live test.
  5. A workflow step that runs a live Go runner (LIVE_RUNNERS) sets BIFROST_LIVE_TESTS. Without
     it the suites skip and the runner reports success, so a live job would pass having run nothing.

  check-live-tests-opt-in.py                    check the repository; exit 1 on a violation
  check-live-tests-opt-in.py --list-live        print "<package dir>\t<TestName>" for every live test
  check-live-tests-opt-in.py --write-inventory  rewrite live-tests.txt from the repository
  check-live-tests-opt-in.py --self-test        run the rules against built-in cases
"""
import collections
import os
import re
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.abspath(os.path.join(HERE, "..", "..", ".."))
INVENTORY = os.path.join(HERE, "live-tests.txt")
SETUP_FILE = "core/internal/llmtests/setup.go"

CREDENTIAL = re.compile(r"(API_KEY|_TOKEN|SECRET|ACCESS_KEY|_CREDENTIALS|PRIVATE_KEY)$")
ENV_READ = re.compile(r'\bos\.(?:Getenv|LookupEnv)\(\s*"([A-Z0-9_]+)"\s*\)')
ENV_KEY = re.compile(r'"env\.([A-Z0-9_]+)"')
HARNESS = re.compile(r"\b(?:llmtests\.)?SetupTest\(\)")
GATE = re.compile(r"\blivetest\.(?:Require|Key)\(")
ENABLED = "livetest.Enabled()"
FUNC = re.compile(r"^func\s+(?:\([^)]*\)\s*)?([A-Za-z_][A-Za-z0-9_]*)\s*\(", re.M)

# Credentials that only ever reach a service the test starts or a local container, never a vendor.
LOCAL_CREDENTIALS = {
    "BIFROST_SETUP_TOKEN": "the gateway's own bootstrap token, for the gateway the test runs",
    "BIFROST_E2E_SETUP_TOKEN": "the same bootstrap token, under the e2e runner's name",
    "AGENTGATEWAY_ADMIN_TOKEN": "the admin token of the local Agent Gateway tests/agentgateway starts",
    "QDRANT_API_KEY": "the key of the Qdrant instance QDRANT_HOST names, a local container by default",
}


# The commands that run the Go live suites in CI. A step running one must set the opt-in.
LIVE_RUNNERS = ("test-all-plugins.sh", "make test-core")
STEP = re.compile(r"\n(?=\s*- (?:name|uses|run):)")


def workflow_failures(workflows):
    """workflows: {path: text}. A step that runs a live runner must set BIFROST_LIVE_TESTS."""
    out = []
    for path, text in sorted(workflows.items()):
        for step in STEP.split(text):
            runner = next((r for r in LIVE_RUNNERS if r in step), None)
            if runner and "BIFROST_LIVE_TESTS" not in step:
                name = re.search(r"- name:\s*(.+)", step)
                out.append(f"{path}: step {name.group(1).strip() if name else '?'} runs {runner} without BIFROST_LIVE_TESTS")
    return out


def functions(text):
    """Yields (name, body) for every top-level func in a Go source file."""
    starts = [(m.start(), m.group(1)) for m in FUNC.finditer(text)]
    for i, (pos, name) in enumerate(starts):
        end = starts[i + 1][0] if i + 1 < len(starts) else len(text)
        yield name, text[pos:end]


def analyze(files):
    """files: {path: source} of _test.go files. Returns (failures, [(dir, TestName)] live tests)."""
    packages = collections.defaultdict(list)  # dir -> [(path, name, body)]
    for path, text in files.items():
        for name, body in functions(text):
            packages[os.path.dirname(path)].append((path, name, body))

    failures, live = [], []
    for directory, funcs in sorted(packages.items()):
        gated = {name for _, name, body in funcs if GATE.search(body)}
        changed = True
        while changed:  # a function that calls a gated function of its package is gated
            changed = False
            for _, name, body in funcs:
                if name not in gated and any(re.search(r"(?<![\w.])" + re.escape(g) + r"\(", body) for g in gated if g != name):
                    gated.add(name)
                    changed = True
        for path, name, body in funcs:
            ok = name in gated
            for var in ENV_READ.findall(body):
                if CREDENTIAL.search(var) and var not in LOCAL_CREDENTIALS and not ok:
                    failures.append(f"{path}: {name} reads {var} without livetest.Require or livetest.Key")
            if HARNESS.search(body) and not ok and not name.startswith("SetupTest"):
                failures.append(f"{path}: {name} builds the live provider harness (SetupTest) without livetest.Require")
            if name == "GetKeysForProvider" and any(CREDENTIAL.search(v) for v in ENV_KEY.findall(body)) and ENABLED not in body:
                failures.append(f"{path}: GetKeysForProvider hands out an env credential without checking {ENABLED}")
            if name.startswith("Test") and ok:
                live.append((directory, name))
    return failures, sorted(set(live))


def repository_files():
    out = subprocess.run(["git", "-C", REPO, "ls-files", "*_test.go"], capture_output=True, text=True, check=True).stdout
    return {f: open(os.path.join(REPO, f), encoding="utf-8").read() for f in out.split() if f}


def read_inventory():
    if not os.path.exists(INVENTORY):
        return set()
    with open(INVENTORY, encoding="utf-8") as fh:
        return {tuple(line.rstrip("\n").split("\t")) for line in fh if line.strip() and not line.startswith("#")}


def write_inventory(live):
    with open(INVENTORY, "w", encoding="utf-8") as fh:
        fh.write("# Every live test (TXE-7530): written by check-live-tests-opt-in.py --write-inventory and checked\n")
        fh.write("# against the repository, so a test cannot lose its livetest gate without failing the check.\n")
        for directory, name in live:
            fh.write(f"{directory}\t{name}\n")


def check():
    failures, live = analyze(repository_files())
    setup = dict(functions(open(os.path.join(REPO, SETUP_FILE), encoding="utf-8").read())).get("SetupTest", "")
    if ENABLED not in setup:
        failures.append(f"{SETUP_FILE}: SetupTest no longer refuses to build a client unless {ENABLED}")
    wf_dir = os.path.join(REPO, ".github", "workflows")
    failures += workflow_failures({f".github/workflows/{f}": open(os.path.join(wf_dir, f), encoding="utf-8").read()
                                   for f in os.listdir(wf_dir) if f.endswith((".yml", ".yaml"))})
    listed = read_inventory()
    for directory, name in sorted(listed - set(live)):
        failures.append(f"{directory}: {name} is in live-tests.txt but no longer reaches livetest.Require or livetest.Key")
    for directory, name in sorted(set(live) - listed):
        failures.append(f"{directory}: {name} is a live test missing from live-tests.txt (run --write-inventory)")
    for f in failures:
        print(f"FAIL {f}")
    print(f"{len(live)} live tests gated on BIFROST_LIVE_TESTS, {len(failures)} failures")
    return 1 if failures else 0


def self_test():
    T = "t *testing.T"
    cases = [
        ("ungated key read", {"p/a_test.go": f'func TestX({T}) {{\n\tif os.Getenv("DEEPSEEK_API_KEY") == "" {{ t.Skip() }}\n}}\n'}, 1, 0),
        ("gated key read", {"p/a_test.go": f'func TestX({T}) {{\n\tlivetest.Require(t)\n\t_ = os.Getenv("VERTEX_API_KEY")\n}}\n'}, 0, 1),
        ("Key helper", {"p/a_test.go": f'func TestX({T}) {{\n\t_ = livetest.Key(t, "MISTRAL_API_KEY")\n}}\n'}, 0, 1),
        ("local credential", {"p/a_test.go": f'func TestX({T}) {{\n\t_ = os.Getenv("AGENTGATEWAY_ADMIN_TOKEN")\n}}\n'}, 0, 0),
        ("non-credential", {"p/a_test.go": f'func TestX({T}) {{\n\t_ = os.Getenv("OLLAMA_BASE_URL")\n}}\n'}, 0, 0),
        ("ungated harness", {"p/a_test.go": f'func TestX({T}) {{\n\tllmtests.SetupTest()\n}}\n'}, 1, 0),
        ("gate in another test", {"p/a_test.go": f'func TestA({T}) {{\n\tlivetest.Require(t)\n}}\nfunc TestB({T}) {{\n\t_ = os.Getenv("XAI_API_KEY")\n}}\n'}, 1, 1),
        ("LookupEnv", {"p/a_test.go": f'func TestX({T}) {{\n\t_, _ = os.LookupEnv("AWS_SECRET_ACCESS_KEY")\n}}\n'}, 1, 0),
        ("gate through a helper in another file", {
            "p/a_test.go": f'func requireEnv({T}) {{\n\tlivetest.Require(t, "OPENAI_API_KEY")\n}}\n',
            "p/b_test.go": f'func TestX({T}) {{\n\trequireEnv(t)\n\tllmtests.SetupTest()\n}}\n'}, 0, 1),
        ("gate through two helpers", {"p/a_test.go":
            f'func inner({T}) {{\n\tlivetest.Require(t)\n}}\nfunc outer({T}) {{\n\tinner(t)\n}}\nfunc TestX({T}) {{\n\touter(t)\n}}\n'}, 0, 1),
        ("helper of another package does not gate", {
            "p/a_test.go": f'func requireEnv({T}) {{\n\tlivetest.Require(t)\n}}\n',
            "q/b_test.go": f'func TestX({T}) {{\n\trequireEnv(t)\n\tllmtests.SetupTest()\n}}\n'}, 1, 0),
        ("method of the same name does not gate", {"p/a_test.go":
            f'func requireEnv({T}) {{\n\tlivetest.Require(t)\n}}\nfunc TestX({T}) {{\n\ts.requireEnv(t)\n\tllmtests.SetupTest()\n}}\n'}, 1, 0),
        ("ungated account", {"p/a_test.go": 'func (a *A) GetKeysForProvider(ctx context.Context) {\n\treturn schemas.NewSecretVar("env.OPENAI_API_KEY")\n}\n'}, 1, 0),
        ("guarded account", {"p/a_test.go": 'func (a *A) GetKeysForProvider(ctx context.Context) {\n\tif !livetest.Enabled() { return nil }\n\treturn schemas.NewSecretVar("env.OPENAI_API_KEY")\n}\n'}, 0, 0),
    ]
    steps = [
        ("live step with the opt-in", "      - name: Run plugin tests\n        env:\n          BIFROST_LIVE_TESTS: \"1\"\n          OPENAI_API_KEY: x\n        run: ./.github/workflows/scripts/test-all-plugins.sh\n", 0),
        ("live step without the opt-in", "      - name: Run plugin tests\n        env:\n          OPENAI_API_KEY: x\n        run: ./.github/workflows/scripts/test-all-plugins.sh\n", 1),
        ("inline opt-in", "      - name: vLLM\n        run: BIFROST_LIVE_TESTS=1 make test-core PROVIDER=vllm\n", 0),
        ("opt-in in a neighbouring step does not count", "      - name: a\n        env:\n          BIFROST_LIVE_TESTS: \"1\"\n        run: echo\n      - name: b\n        run: make test-core\n", 1),
    ]
    bad = 0
    for label, text, want in steps:
        got = len(workflow_failures({"w.yml": "jobs:\n  j:\n    steps:\n" + text}))
        if got != want:
            bad += 1
            print(f"FAIL self-test {label}: {got} violations, want {want}")
    for label, files, want_failures, want_live in cases:
        failures, live = analyze(files)
        if len(failures) != want_failures or len(live) != want_live:
            bad += 1
            print(f"FAIL self-test {label}: {len(failures)} violations and {len(live)} live tests, want {want_failures} and {want_live}")
    print(f"{len(cases) + len(steps)} self-test cases, {bad} failures")
    return 1 if bad else 0


if __name__ == "__main__":
    if "--self-test" in sys.argv:
        sys.exit(self_test())
    if "--list-live" in sys.argv:
        for directory, name in analyze(repository_files())[1]:
            print(f"{directory}\t{name}")
        sys.exit(0)
    if "--write-inventory" in sys.argv:
        write_inventory(analyze(repository_files())[1])
        sys.exit(0)
    sys.exit(check())
