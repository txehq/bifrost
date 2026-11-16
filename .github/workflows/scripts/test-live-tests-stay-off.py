#!/usr/bin/env python3
"""Proves that every vendor key set, and no opt-in, runs no live test (TXE-7530).

A plain `go test ./...` with DEEPSEEK_API_KEY exported used to call api.deepseek.com. This sets a
fake value for every credential a live test or the live provider harness reads, leaves
BIFROST_LIVE_TESTS unset, points HTTP_PROXY and HTTPS_PROXY at a closed loopback port so a request
that did go out fails at once, and runs every live test that check-live-tests-opt-in.py lists. Each
one must skip; one that passes or fails ran its body with a key it should never have used. In CI
the job also blocks egress, so a vendor call could not leave the runner either way.

Usage: test-live-tests-stay-off.py   (from the repository root, after the Go workspace is set up)
"""
import collections
import json
import os
import re
import subprocess
import sys

REPO = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", ".."))
SCAN = os.path.join(os.path.dirname(os.path.abspath(__file__)), "check-live-tests-opt-in.py")
OPT_IN = "BIFROST_LIVE_TESTS"

LITERAL = re.compile(r'"([A-Z][A-Z0-9_]{2,})"')
ENV_REF = re.compile(r'"env\.([A-Z][A-Z0-9_]+)"')
GATE_CALL = re.compile(r"livetest\.(?:Require|Key)\(t(?:,\s*([^)]*))?\)")
ENV_READ = re.compile(r'\bos\.(?:Getenv|LookupEnv)\(\s*"([A-Z0-9_]+)"\s*\)')


def module_root(directory):
    d = os.path.join(REPO, directory)
    while d != REPO and not os.path.exists(os.path.join(d, "go.mod")):
        d = os.path.dirname(d)
    return d


def module_path(root):
    with open(os.path.join(root, "go.mod"), encoding="utf-8") as fh:
        return fh.readline().split()[1]


def credentials(dirs):
    """Every variable a live test names to the helper or reads, and every env.X the harness uses."""
    names = set()
    paths = [os.path.join(REPO, "core/internal/llmtests", f) for f in os.listdir(os.path.join(REPO, "core/internal/llmtests")) if f.endswith(".go")]
    for directory in dirs:
        base = os.path.join(REPO, directory)
        paths += [os.path.join(base, f) for f in os.listdir(base) if f.endswith("_test.go")]
    for path in paths:
        text = open(path, encoding="utf-8").read()
        names.update(ENV_REF.findall(text))
        names.update(ENV_READ.findall(text))
        for args in GATE_CALL.findall(text):
            names.update(LITERAL.findall(args))
    names.discard(OPT_IN)
    return sorted(names)


def main():
    listed = subprocess.run([sys.executable, SCAN, "--list-live"], capture_output=True, text=True, check=True).stdout
    live = [tuple(line.split("\t")) for line in listed.splitlines() if line]
    if not live:
        print("FAIL check-live-tests-opt-in.py --list-live listed no live tests")
        return 1

    env = dict(os.environ)
    env.pop(OPT_IN, None)
    names = credentials(sorted({d for d, _ in live}))
    for name in names:
        env[name] = "live-tests-off-fake-" + name.lower()
    env["HTTP_PROXY"] = env["HTTPS_PROXY"] = env["http_proxy"] = env["https_proxy"] = "http://127.0.0.1:9"
    env["NO_PROXY"] = env["no_proxy"] = ""
    print(f"{len(names)} credentials set to fake values, {OPT_IN} unset, proxy at a closed loopback port")

    by_module = collections.defaultdict(lambda: collections.defaultdict(set))
    for directory, name in live:
        root = module_root(directory)
        by_module[root][os.path.relpath(os.path.join(REPO, directory), root)].add(name)

    # The build needs the module proxy, and the dead proxy below would refuse it on a runner whose
    # module cache is empty. So each module's test binaries are built first, with the network but
    # without the fake keys; -exec true builds them and runs none, so no test code executes (not
    # even a TestMain). The test run then has every module it needs, and GOPROXY=off makes a module
    # that is still missing fail at once instead of reaching for the network.
    build_env = dict(os.environ)
    build_env.pop(OPT_IN, None)
    for name in names:
        build_env.pop(name, None)
    for root, packages in sorted(by_module.items()):
        # A download the module proxy drops mid-transfer fails the build; it is retried, since
        # that is the network and not the change under test.
        build = ["go", "test", "-count=1", "-exec", "true"] + ["./" + p for p in sorted(packages)]
        proc = subprocess.run(build, cwd=root, env=build_env, capture_output=True, text=True)
        for _ in range(2):
            if proc.returncode == 0:
                break
            proc = subprocess.run(build, cwd=root, env=build_env, capture_output=True, text=True)
        if proc.returncode != 0:
            print(f"FAIL building the live tests in {os.path.relpath(root, REPO)}: {(proc.stdout + proc.stderr)[-1500:]}")
            return 1
    env["GOPROXY"] = "off"

    outcome = {}
    failures = []
    for root, packages in sorted(by_module.items()):
        mod = module_path(root)
        names_re = "^(" + "|".join(sorted({n for ns in packages.values() for n in ns})) + ")$"
        args = ["go", "test", "-json", "-count=1", "-timeout=10m", "-run", names_re] + ["./" + p for p in sorted(packages)]
        proc = subprocess.run(args, cwd=root, env=env, capture_output=True, text=True)
        for line in proc.stdout.splitlines():
            try:
                ev = json.loads(line)
            except ValueError:
                continue
            if ev.get("Test") and ev.get("Action") in ("pass", "fail", "skip"):
                outcome[(ev["Package"], ev["Test"])] = ev["Action"]
        if proc.returncode not in (0, 1):
            failures.append(f"go test in {os.path.relpath(root, REPO)} exited {proc.returncode}: {proc.stderr[-800:]}")
        for rel, tests in packages.items():
            pkg = mod if rel == "." else mod + "/" + rel.replace(os.sep, "/")
            for name in sorted(tests):
                got = outcome.get((pkg, name))
                # A test that gates inside t.Run reports pass when every subtest skipped: that is a
                # skip too, as long as no subtest under it passed or failed.
                subs = [v for (p, n), v in outcome.items() if p == pkg and n.startswith(name + "/")]
                if got == "pass" and subs and all(v == "skip" for v in subs):
                    got = "skip"
                if got != "skip":
                    ran = [n for (p, n), v in outcome.items() if p == pkg and n.startswith(name + "/") and v != "skip"]
                    detail = f" (subtests that ran: {', '.join(sorted(ran)[:5])})" if ran else ""
                    failures.append(f"{pkg} {name}: {got or 'did not run'}{detail}, want skip with every key set and no {OPT_IN}")
        if proc.returncode == 1 and not failures:
            failures.append(f"go test in {os.path.relpath(root, REPO)} failed: {proc.stdout[-1500:]}")

    for f in failures:
        print(f"FAIL {f}")
    print(f"{len(live)} live tests, {len(live) - sum(1 for f in failures if ': ' in f and 'want skip' in f)} skipped, {len(failures)} failures")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
