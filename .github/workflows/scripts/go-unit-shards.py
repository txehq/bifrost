#!/usr/bin/env python3
"""Plans and runs one shard of the offline Go unit tests on a pull request (TXE-7078).

The fork's required PR checks ran no Go test, and the release pipeline runs the Go suites only
after a merge, so a PR that broke a cross-provider Go test merged green and main went red in
core (TestExtraParamsNeverReplaceTheModelOrInputOnAnyProvider for google-cloud-tts, after
PR #144). go-unit-tests.yml runs this script on every pull request, in SHARDS parallel jobs.

  go-unit-shards.py plan --shards N            print every shard's units (no tests run)
  go-unit-shards.py run --shard I --shards N   run shard I (1-based)
  go-unit-shards.py compile                    build every test binary, run none (cache warm-up)

What it runs. Every package with test files in core, framework, transports and every
plugins/*/go.mod module, found with `go list` on each run, so a new package or plugin is
tested without a CI edit. The few exclusions are EXCLUDED below, each with its reason. A
package heavier than a shard's share is split into parts by its top-level test names
(`go test -list`), round-robin over the sorted names, so every test lands in exactly one part.
Each run re-plans every shard and fails unless every package and every listed test of a split
package is assigned exactly once.

No live calls. go-unit-tests.yml runs this script through scripts/test-offline.sh, which
removes every credential variable from the environment; the script refuses to run when any
*_API_KEY variable is still set, and prints how many it saw (zero).

Load-sensitive tests. A test known to fail on a loaded host is named in LOAD_SENSITIVE_TESTS
(go-unit-tests.yml), one `<ticket> <import path> <test name>` per line. It is skipped in its
package's main run and then run alone, after the rest of the shard has finished, with -p 1,
so it is tested and its ticket is printed, never silently dropped.
"""
import argparse
import concurrent.futures
import math
import os
import re
import subprocess
import sys
import time

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))

# Module directories, relative to ROOT. plugins/*/go.mod is globbed in modules().
MODULES = ("core", "framework", "transports")

# Packages not run here, with the reason. Matched on the import path's suffix after the module.
EXCLUDED = {
    # The shared live-provider suites. They hold no unit test of their own; each provider
    # package calls them, and they skip there with no key (TXE-7053).
    "core/internal/llmtests": "live-provider suite library, exercised through the provider packages",
    # Live gateway and provider tests (test-bifrost-http.sh excludes the same subtree).
    "transports/bifrost-http/tests": "live gateway tests, run by the release pipeline",
    # Its cache tests embed through OpenAI (text-embedding-3-small) and synthesize with tts-1, so
    # without a key 13 of them fail on "no keys found that support model". test-all-plugins.sh
    # skips this plugin for the same reason when OPENAI_API_KEY is unset.
    "plugins/semanticcache": "needs a live OpenAI key for embeddings and speech",
}

# Extra `go test` flags per module directory. transports runs -short like test-bifrost-http.sh.
MODULE_FLAGS = {"transports": ["-short"]}

# Seconds each package's tests take on a 4-vCPU ubuntu-24.04 runner without -race, measured
# from this job's own logs (the TIMING lines it prints). A package not listed weighs DEFAULT_WEIGHT.
# Only the plan's balance depends on these numbers; a wrong one makes a shard slower, never
# skips a test.
WEIGHTS = {
    # PR #207's first run (cold cache), test execution only.
    "framework/logstore": 351,
    "framework/configstore": 284,
    "core/internal/mcptests": 234,
    "framework/queue": 211,
    "transports/bifrost-http/handlers": 124,
    "transports/bifrost-http/lib": 121,
    "core/providers/utils": 97,
    "core": 84,
    "framework/plugins": 62,
    "plugins/logging": 46,
    "core/network": 36,
    "plugins/governance": 27,
    "core/mcp": 27,
    "framework/vectorstore": 23,
    "core/providers/anthropic": 21,
    "transports/bifrost-http/integrations": 20,
    "transports/bifrost-http/server": 20,
    "framework/postgresconn": 16,
    "core/agent": 12,
    "framework/openapimcp": 8,
    "framework/configstore/tables": 7,
}
DEFAULT_WEIGHT = 5

API_KEY = re.compile(r"(^API_KEY$|_API_KEY$|_API_KEY_)", re.I)


def go(args, cwd=ROOT, check=True):
    out = subprocess.run(["go", *args], cwd=cwd, text=True, capture_output=True)
    if check and out.returncode != 0:
        sys.stderr.write(out.stdout + out.stderr)
        raise SystemExit(f"go {' '.join(args)} failed in {cwd}")
    return out.stdout


def modules():
    mods: list[str] = list(MODULES)
    for name in sorted(os.listdir(os.path.join(ROOT, "plugins"))):
        if os.path.isfile(os.path.join(ROOT, "plugins", name, "go.mod")):
            mods.append(f"plugins/{name}")
    return mods


def packages():
    """[(module dir, import path, repo-relative dir)] for every package with test files."""
    found = []
    for mod in modules():
        listing = go(["list", "-f", "{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}\t{{.Dir}}{{end}}", "./..."],
                     cwd=os.path.join(ROOT, mod))
        for line in listing.splitlines():
            if not line.strip():
                continue
            path, directory = line.split("\t")
            rel = os.path.relpath(directory, ROOT)
            if any(rel == ex or rel.startswith(ex + "/") for ex in EXCLUDED):
                continue
            found.append((mod, path, rel))
    if not found:
        raise SystemExit("no Go test packages found")
    return found


def load_sensitive():
    """{import path: [(test name, ticket)]} from LOAD_SENSITIVE_TESTS."""
    table = {}
    for line in os.environ.get("LOAD_SENSITIVE_TESTS", "").splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        ticket, path, name = line.split()
        if not re.fullmatch(r"[A-Z]+-[0-9]+", ticket):
            raise SystemExit(f"LOAD_SENSITIVE_TESTS: {line!r} must start with its ticket id")
        table.setdefault(path, []).append((name, ticket))
    return table


def list_tests(mod, path):
    out = go(["test", *MODULE_FLAGS.get(mod, []), "-list", ".", path], cwd=os.path.join(ROOT, mod))
    names = [l for l in out.splitlines() if re.fullmatch(r"(Test|Example|Fuzz)\w*", l)]
    if not names:
        raise SystemExit(f"{path}: -list found no tests in a package planned as split")
    return sorted(names)


def plan(n_shards, with_tests=True):
    """[[unit]] per shard; a unit is {mod, path, rel, weight, part, parts, tests}."""
    pkgs = packages()
    total = sum(WEIGHTS.get(rel, DEFAULT_WEIGHT) for _, _, rel in pkgs)
    share = total / n_shards
    units = []
    for mod, path, rel in pkgs:
        w = WEIGHTS.get(rel, DEFAULT_WEIGHT)
        parts = min(n_shards, max(1, math.ceil(w / (share * 0.6))))
        for p in range(parts):
            units.append({"mod": mod, "path": path, "rel": rel, "weight": w / parts,
                          "part": p, "parts": parts, "tests": None})
    # Longest first onto the lightest shard; the parts of one package go to different shards,
    # because they share that package's databases and must not run side by side.
    shards = [[] for _ in range(n_shards)]
    load = [0.0] * n_shards
    for u in sorted(units, key=lambda u: (-u["weight"], u["rel"], u["part"])):
        taken = {i for i, s in enumerate(shards) if any(x["rel"] == u["rel"] for x in s)}
        i = min((i for i in range(n_shards) if i not in taken), key=lambda i: (load[i], i))
        shards[i].append(u)
        load[i] += u["weight"]
    if with_tests:
        names = {}
        for s in shards:
            for u in s:
                if u["parts"] > 1:
                    if u["rel"] not in names:
                        names[u["rel"]] = list_tests(u["mod"], u["path"])
                    u["tests"] = names[u["rel"]][u["part"]::u["parts"]]
        verify(pkgs, shards, names)
    return shards, load


def verify(pkgs, shards, names):
    seen = {}
    for s in shards:
        for u in s:
            seen.setdefault(u["rel"], []).append(u)
    for _, _, rel in pkgs:
        us = seen.get(rel, [])
        if not us:
            raise SystemExit(f"plan: {rel} is in no shard")
        if us[0]["parts"] == 1:
            if len(us) != 1:
                raise SystemExit(f"plan: {rel} is in {len(us)} shards")
            continue
        assigned = sorted(t for u in us for t in u["tests"])
        if assigned != names[rel]:
            raise SystemExit(f"plan: the parts of {rel} do not cover its tests exactly once")


def anchored(names):
    return "^(" + "|".join(re.escape(n) for n in names) + ")$"


def run_shard(index, n_shards):
    keys = sorted(k for k in os.environ if API_KEY.search(k))
    print(f"*_API_KEY variables in this environment: {len(keys)}")
    if keys:
        raise SystemExit("refusing to run with provider keys set; run through scripts/test-offline.sh")
    shards, load = plan(n_shards)
    mine = shards[index - 1]
    print(f"shard {index}/{n_shards}: {len(mine)} units, planned {load[index - 1]:.0f}s "
          f"(shards: {', '.join(f'{l:.0f}s' for l in load)})")
    sensitive = load_sensitive()
    for path in sensitive:
        if not any(p == path for _, p, _ in packages()):
            raise SystemExit(f"LOAD_SENSITIVE_TESTS names {path}, which is not a planned package")

    # One invocation for the whole packages of each module (go test runs them -p in parallel),
    # one per split part. They run side by side, at most WORKERS at a time.
    jobs = []
    whole = {}
    for u in mine:
        if u["parts"] == 1:
            whole.setdefault(u["mod"], []).append(u)
    for mod, us in sorted(whole.items()):
        args = [*MODULE_FLAGS.get(mod, [])]
        skips = [n for u in us for n, _ in sensitive.get(u["path"], [])]
        if skips:
            args += ["-skip", anchored(skips)]
        jobs.append((f"{mod} ({len(us)} packages)", mod, args, [u["path"] for u in us]))
    for u in mine:
        if u["parts"] > 1:
            tests = u["tests"]
            skips = [n for n, _ in sensitive.get(u["path"], [])]
            args = [*MODULE_FLAGS.get(u["mod"], []), "-run", anchored(tests)]
            if any(s in tests for s in skips):
                args += ["-skip", anchored([s for s in skips if s in tests])]
            jobs.append((f"{u['rel']} part {u['part'] + 1}/{u['parts']} ({len(tests)} tests)",
                         u["mod"], args, [u["path"]]))

    def execute(job):
        label, mod, args, paths = job
        start = time.monotonic()
        out = subprocess.run(["go", "test", "-timeout", "15m", *args, *paths], cwd=os.path.join(ROOT, mod),
                             text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        return label, out.returncode, out.stdout, time.monotonic() - start

    failed = []
    results = []
    workers = int(os.environ.get("GO_UNIT_WORKERS", "3"))
    with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as pool:
        for label, code, output, secs in pool.map(execute, jobs):
            results.append((label, code, secs))
            print(f"::group::{'FAIL' if code else 'ok'} {label} ({secs:.0f}s)")
            print(output, end="")
            print("::endgroup::")
            if code:
                failed.append(label)
                # The failing tests' output, outside the fold, so the log shows it first.
                print("\n".join(l for l in output.splitlines()
                                if l.startswith(("--- FAIL", "FAIL", "panic:")) or "_test.go:" in l)[:20000])

    # The load-sensitive tests this shard owns: alone, one package at a time, after the rest.
    for u in mine:
        for name, ticket in sensitive.get(u["path"], []):
            if u["parts"] > 1 and name not in u["tests"]:
                continue
            label = f"load-sensitive {name} ({ticket}), run alone"
            start = time.monotonic()
            out = subprocess.run(["go", "test", "-timeout", "5m", "-p", "1", *MODULE_FLAGS.get(u["mod"], []),
                                  "-run", anchored([name]), u["path"]], cwd=os.path.join(ROOT, u["mod"]),
                                 text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
            secs = time.monotonic() - start
            results.append((label, out.returncode, secs))
            print(f"::notice::{label}: {'FAIL' if out.returncode else 'ok'} ({secs:.0f}s)")
            print(out.stdout, end="")
            if out.returncode:
                failed.append(label)

    for line in sorted(output_timings(results)):
        print(line)
    if failed:
        for label in failed:
            print(f"::error::go test failed: {label}")
        return 1
    return 0


def output_timings(results):
    for label, code, secs in results:
        yield f"TIMING {secs:7.1f}s {'FAIL' if code else 'ok  '} {label}"


def compile_all():
    """Builds and links every planned package's test binary without running it, so the go-unit
    build cache saved on main holds what the shards compile."""
    by_mod = {}
    for mod, path, _ in packages():
        by_mod.setdefault(mod, []).append(path)
    for mod, paths in by_mod.items():
        start = time.monotonic()
        out = subprocess.run(["go", "test", "-exec", "true", *MODULE_FLAGS.get(mod, []), *paths],
                             cwd=os.path.join(ROOT, mod), text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        print(f"compiled {len(paths)} {mod} test binaries in {time.monotonic() - start:.0f}s")
        if out.returncode:
            print(out.stdout)
            return 1
    return 0


def main():
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="cmd", required=True)
    p = sub.add_parser("plan")
    p.add_argument("--shards", type=int, required=True)
    p.add_argument("--list-tests", action="store_true", help="also list split packages' tests (compiles them)")
    r = sub.add_parser("run")
    r.add_argument("--shard", type=int, required=True)
    r.add_argument("--shards", type=int, required=True)
    sub.add_parser("compile")
    a = ap.parse_args()
    if a.cmd == "compile":
        return compile_all()
    if a.cmd == "plan":
        shards, load = plan(a.shards, with_tests=a.list_tests)
        for i, s in enumerate(shards, 1):
            print(f"shard {i}: planned {load[i - 1]:.0f}s")
            for u in s:
                part = f" part {u['part'] + 1}/{u['parts']}" if u["parts"] > 1 else ""
                print(f"  {u['weight']:6.1f}s {u['rel']}{part}")
        return 0
    if not 1 <= a.shard <= a.shards:
        raise SystemExit("--shard must be between 1 and --shards")
    return run_shard(a.shard, a.shards)


if __name__ == "__main__":
    sys.exit(main())
