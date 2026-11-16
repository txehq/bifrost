#!/usr/bin/env python3
"""A live test runs only on an explicit opt-in, through one helper (TXE-7530).

A plain `go test ./...` in core/ with DEEPSEEK_API_KEY exported called api.deepseek.com until the
package timed out: live suites switched on whenever their key was in the environment. Now
core/livetest gates them: livetest.Require(t, vars...) skips unless BIFROST_LIVE_TESTS=1 and every
named variable is set, and livetest.Key(t, name) returns a credential the same way. This check
keeps every live test on it:

  1. A function in a _test.go file that reads a credential-named environment variable directly
     (os.Getenv / os.LookupEnv of *_API_KEY, *_TOKEN, *_SECRET*, *ACCESS_KEY*, *_CREDENTIALS,
     *_PRIVATE_KEY) must call livetest.Require or livetest.Key. A credential that only reaches a
     local service, never a vendor, is named in LOCAL_CREDENTIALS with the reason.
  2. A function that builds the live provider harness (llmtests.SetupTest) must call
     livetest.Require or livetest.Key: the harness reads every vendor key from the environment.
  3. llmtests.SetupTest itself refuses to build a client unless livetest.Enabled() is true, so a
     suite that slips past rule 2 fails instead of calling the vendor.

  check-live-tests-opt-in.py              check the repository; exit 1 on a violation
  check-live-tests-opt-in.py --list-live  print "<package dir>\t<TestName>" for every top-level
                                          live test, for the step that proves they all skip
  check-live-tests-opt-in.py --self-test  run the rules against built-in cases
"""
import os
import re
import subprocess
import sys

REPO = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", ".."))
SETUP_FILE = "core/internal/llmtests/setup.go"

CREDENTIAL = re.compile(r"(API_KEY|_TOKEN|SECRET|ACCESS_KEY|_CREDENTIALS|PRIVATE_KEY)$")
ENV_READ = re.compile(r'\bos\.(?:Getenv|LookupEnv)\(\s*"([A-Z0-9_]+)"\s*\)')
HARNESS = re.compile(r"\b(?:llmtests\.)?SetupTest\(\)")
GATE = re.compile(r"\blivetest\.(?:Require|Key)\(")
FUNC = re.compile(r"^func\s+(?:\([^)]*\)\s*)?([A-Za-z_][A-Za-z0-9_]*)\s*\(", re.M)

# Credentials that only ever reach a service the test starts or a local container, never a vendor.
LOCAL_CREDENTIALS = {
    "BIFROST_SETUP_TOKEN": "the gateway's own bootstrap token, for the gateway the test runs",
    "BIFROST_E2E_SETUP_TOKEN": "the same bootstrap token, under the e2e runner's name",
    "AGENTGATEWAY_ADMIN_TOKEN": "the admin token of the local Agent Gateway tests/agentgateway starts",
    "QDRANT_API_KEY": "the key of the Qdrant instance QDRANT_HOST names, a local container by default",
}


def functions(text):
    """Yields (name, body) for every top-level func in a Go source file."""
    starts = [(m.start(), m.group(1)) for m in FUNC.finditer(text)]
    for i, (pos, name) in enumerate(starts):
        end = starts[i + 1][0] if i + 1 < len(starts) else len(text)
        yield name, text[pos:end]


def violations(path, text):
    out = []
    for name, body in functions(text):
        gated = bool(GATE.search(body))
        for var in ENV_READ.findall(body):
            if CREDENTIAL.search(var) and var not in LOCAL_CREDENTIALS and not gated:
                out.append(f"{path}: {name} reads {var} without livetest.Require or livetest.Key")
        if HARNESS.search(body) and not gated and not name.startswith("SetupTest"):
            out.append(f"{path}: {name} builds the live provider harness (SetupTest) without livetest.Require")
    return out


def test_files():
    files = subprocess.run(["git", "-C", REPO, "ls-files", "*_test.go"], capture_output=True, text=True, check=True)
    return [f for f in files.stdout.split() if f]


def live_tests():
    """Top-level Test functions that call the helper: the suites that must skip with no opt-in."""
    for path in test_files():
        text = open(os.path.join(REPO, path), encoding="utf-8").read()
        for name, body in functions(text):
            if name.startswith("Test") and GATE.search(body):
                yield os.path.dirname(path), name


def check():
    failures = []
    for path in test_files():
        failures += violations(path, open(os.path.join(REPO, path), encoding="utf-8").read())
    setup = open(os.path.join(REPO, SETUP_FILE), encoding="utf-8").read()
    body = dict(functions(setup)).get("SetupTest", "")
    if "livetest.Enabled()" not in body:
        failures.append(f"{SETUP_FILE}: SetupTest no longer refuses to build a client unless livetest.Enabled()")
    for f in failures:
        print(f"FAIL {f}")
    live = sum(1 for _ in live_tests())
    print(f"{live} live tests gated on BIFROST_LIVE_TESTS, {len(failures)} failures")
    return 1 if failures else 0


def self_test():
    cases = [
        ("ungated key read", 'func TestX(t *testing.T) {\n\tif os.Getenv("DEEPSEEK_API_KEY") == "" {\n\t\tt.Skip()\n\t}\n}\n', 1),
        ("gated key read", 'func TestX(t *testing.T) {\n\tlivetest.Require(t)\n\t_ = os.Getenv("VERTEX_API_KEY")\n}\n', 0),
        ("Key helper", 'func TestX(t *testing.T) {\n\tk := livetest.Key(t, "MISTRAL_API_KEY")\n\t_ = k\n}\n', 0),
        ("helper without t", 'func getPlugin() {\n\t_ = os.Getenv("MAXIM_API_KEY")\n}\n', 1),
        ("local credential", 'func TestX(t *testing.T) {\n\t_ = os.Getenv("AGENTGATEWAY_ADMIN_TOKEN")\n}\n', 0),
        ("non-credential", 'func TestX(t *testing.T) {\n\t_ = os.Getenv("OLLAMA_BASE_URL")\n}\n', 0),
        ("ungated harness", 'func TestX(t *testing.T) {\n\tc, _, _, _ := llmtests.SetupTest()\n\t_ = c\n}\n', 1),
        ("gated harness", 'func TestX(t *testing.T) {\n\tlivetest.Require(t, "GROQ_API_KEY")\n\tllmtests.SetupTest()\n}\n', 0),
        ("gate in another func", 'func TestA(t *testing.T) {\n\tlivetest.Require(t)\n}\nfunc TestB(t *testing.T) {\n\t_ = os.Getenv("XAI_API_KEY")\n}\n', 1),
        ("LookupEnv", 'func TestX(t *testing.T) {\n\t_, _ = os.LookupEnv("AWS_SECRET_ACCESS_KEY")\n}\n', 1),
    ]
    bad = 0
    for label, src, want in cases:
        got = len(violations("case.go", src))
        if got != want:
            bad += 1
            print(f"FAIL self-test {label}: {got} violations, want {want}")
    print(f"{len(cases)} self-test cases, {bad} failures")
    return 1 if bad else 0


if __name__ == "__main__":
    if "--self-test" in sys.argv:
        sys.exit(self_test())
    if "--list-live" in sys.argv:
        for directory, name in sorted(set(live_tests())):
            print(f"{directory}\t{name}")
        sys.exit(0)
    sys.exit(check())
