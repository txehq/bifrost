#!/usr/bin/env python3
"""Tests go-unit-shards.py (TXE-7078) with no Go toolchain: `go list`, `go test -list` and the
`go test` runs are replaced by a fake module tree, so the plan's coverage, the split-test
selection, the load-sensitive reruns, the key refusal and failure propagation are checked here.

Usage: python3 .github/workflows/scripts/test-go-unit-shards.py
"""
import contextlib
import importlib.util
import io
import os
import re
import subprocess
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("go_unit_shards", os.path.join(HERE, "go-unit-shards.py"))
shards_mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(shards_mod)

PREFIX = "example.com/m"

# rel dir -> top-level test names. core and framework/logstore are heavy (WEIGHTS) and split;
# the two excluded packages must never be planned.
TREE = {
    "core": [f"TestCore{i:02d}" for i in range(9)],
    "core/schemas": ["TestSchemaA", "TestSchemaB"],
    "core/internal/llmtests": ["TestLive"],
    "framework/logstore": [f"TestLog{i:02d}" for i in range(13)],
    "framework/warp": ["TestWarp"],
    "plugins/alpha": ["TestAlpha"],
    "plugins/semanticcache": ["TestCache"],
}
MODULES = ["core", "framework", "plugins/alpha", "plugins/semanticcache"]


def module_of(rel):
    return next(m for m in sorted(MODULES, key=len, reverse=True) if rel == m or rel.startswith(m + "/"))


def import_path(rel):
    return f"{PREFIX}/{rel}"


def fake_go(args, cwd=shards_mod.ROOT, check=True):
    mod = os.path.relpath(cwd, shards_mod.ROOT)
    if args[0] == "list":
        return "".join(f"{import_path(rel)}\t{os.path.join(shards_mod.ROOT, rel)}\n"
                       for rel in TREE if module_of(rel) == mod)
    if args[0] == "test" and "-list" in args:
        rel = args[-1][len(PREFIX) + 1:]
        return "\n".join(TREE[rel] + [f"ok  \t{args[-1]}\t0.01s"]) + "\n"
    raise AssertionError(f"unexpected go {args}")


class FakeRun:
    """Records every `go test` the runner starts; fails the ones whose package is in fail."""

    def __init__(self, fail=()):
        self.calls = []
        self.fail = set(fail)

    def __call__(self, cmd, cwd=None, **kwargs):
        self.calls.append(cmd)
        code = 1 if any(import_path(rel) in cmd for rel in self.fail) else 0
        return subprocess.CompletedProcess(cmd, code, stdout="--- FAIL: TestX\n" if code else "ok\n")

    def selected(self, cmd):
        """The tests of TREE a recorded `go test` command would run."""
        run = cmd[cmd.index("-run") + 1] if "-run" in cmd else None
        skip = cmd[cmd.index("-skip") + 1] if "-skip" in cmd else None
        names = set()
        for rel, tests in TREE.items():
            if import_path(rel) not in cmd:
                continue
            for t in tests:
                if run and not re.fullmatch(run, t):
                    continue
                if skip and re.fullmatch(skip, t):
                    continue
                names.add((rel, t))
        return names


@contextlib.contextmanager
def fake_world(env=None):
    base = {k: v for k, v in os.environ.items() if not shards_mod.API_KEY.search(k)}
    base.pop("LOAD_SENSITIVE_TESTS", None)
    base.update(env or {})
    with mock.patch.object(shards_mod, "go", fake_go), \
         mock.patch.object(shards_mod, "modules", lambda: list(MODULES)), \
         mock.patch.dict(os.environ, base, clear=True):
        yield


def run_quiet(fn, *args):
    out = io.StringIO()
    with contextlib.redirect_stdout(out):
        code = fn(*args)
    return code, out.getvalue()


PLANNED = {rel for rel in TREE if rel not in ("core/internal/llmtests", "plugins/semanticcache")}


class PlanTest(unittest.TestCase):
    def test_every_package_and_test_is_planned_exactly_once(self):
        with fake_world():
            for n in (1, 2, 3, 4, 7):
                shards, _ = shards_mod.plan(n)
                self.assertEqual(len(shards), n)
                covered = []
                for s in shards:
                    for u in s:
                        covered += [(u["rel"], t) for t in (u["tests"] if u["parts"] > 1 else TREE[u["rel"]])]
                self.assertEqual(sorted(covered), sorted((rel, t) for rel in PLANNED for t in TREE[rel]), n)

    def test_excluded_packages_are_never_planned(self):
        with fake_world():
            rels = {u["rel"] for s in shards_mod.plan(4)[0] for u in s}
        self.assertEqual(rels, PLANNED)

    def test_heavy_packages_split_onto_distinct_shards(self):
        with fake_world():
            shards, _ = shards_mod.plan(4)
        for rel in ("core", "framework/logstore"):
            homes = [i for i, s in enumerate(shards) for u in s if u["rel"] == rel]
            self.assertGreater(len(homes), 1, rel)
            self.assertEqual(len(homes), len(set(homes)), f"{rel}: two parts on one shard")

    def test_parts_stay_apart_when_the_lightest_shard_already_holds_one(self):
        # Weights where plain longest-first packing would put two of core's parts on one shard.
        weights = {"core": 150, "core/schemas": 80, "framework/logstore": 60, "framework/warp": 100, "plugins/alpha": 1}
        with fake_world(), mock.patch.object(shards_mod, "WEIGHTS", weights):
            shards, _ = shards_mod.plan(5)
        for rel in ("core", "framework/logstore"):
            homes = [i for i, s in enumerate(shards) for u in s if u["rel"] == rel]
            self.assertEqual(len(homes), len(set(homes)), f"{rel}: two parts on one shard")

    def test_verify_fails_on_a_dropped_or_duplicated_unit(self):
        with fake_world():
            pkgs = shards_mod.packages()
            shards, _ = shards_mod.plan(4)
            names = {rel: sorted(TREE[rel]) for rel in ("core", "framework/logstore")}
            split = next(u for s in shards for u in s if u["parts"] > 1)
            split["tests"] = split["tests"][1:]
            with self.assertRaises(SystemExit):
                shards_mod.verify(pkgs, shards, names)
            shards, _ = shards_mod.plan(4)
            whole = next(u for s in shards for u in s if u["parts"] == 1)
            shards[0].append(dict(whole))
            with self.assertRaises(SystemExit):
                shards_mod.verify(pkgs, shards, names)

    def test_load_sensitive_entry_needs_a_ticket(self):
        with fake_world({"LOAD_SENSITIVE_TESTS": f"{PREFIX}/core TestCore01 extra\n"}):
            with self.assertRaises(SystemExit):
                shards_mod.load_sensitive()


class RunTest(unittest.TestCase):
    N = 4

    def run_all(self, env=None, fail=()):
        results = []
        with fake_world(env):
            for i in range(1, self.N + 1):
                fake = FakeRun(fail)
                with mock.patch.object(shards_mod.subprocess, "run", fake):
                    code, out = run_quiet(shards_mod.run_shard, i, self.N)
                results.append((code, out, fake))
        return results

    def test_shards_together_run_every_planned_test_once(self):
        ran = []
        for code, _, fake in self.run_all():
            self.assertEqual(code, 0)
            for cmd in fake.calls:
                ran += sorted(fake.selected(cmd))
        self.assertEqual(sorted(ran), sorted((rel, t) for rel in PLANNED for t in TREE[rel]))

    def test_load_sensitive_test_in_a_split_package_is_skipped_then_run_alone_once(self):
        self.check_load_sensitive(("core", "TestCore04"))

    def test_load_sensitive_test_in_a_whole_package_is_skipped_then_run_alone_once(self):
        self.check_load_sensitive(("core/schemas", "TestSchemaB"))

    def check_load_sensitive(self, target):
        env = {"LOAD_SENSITIVE_TESTS": f"TXE-7066 {import_path(target[0])} {target[1]}\n"}
        alone, main_runs = [], []
        for code, out, fake in self.run_all(env):
            self.assertEqual(code, 0)
            for cmd in fake.calls:
                if "-p" in cmd and cmd[cmd.index("-p") + 1] == "1":
                    alone.append(cmd)
                    self.assertIn("TXE-7066", out)
                else:
                    main_runs.append(target in fake.selected(cmd))
        self.assertEqual(len(alone), 1)
        self.assertEqual(FakeRun().selected(alone[0]), {target})
        self.assertFalse(any(main_runs), "the load-sensitive test also ran in a main run")

    def test_load_sensitive_entry_for_an_unplanned_package_fails(self):
        env = {"LOAD_SENSITIVE_TESTS": f"TXE-1 {PREFIX}/nowhere TestX\n"}
        with fake_world(env), mock.patch.object(shards_mod.subprocess, "run", FakeRun()):
            with self.assertRaises(SystemExit):
                run_quiet(shards_mod.run_shard, 1, self.N)

    def test_a_failing_package_fails_its_shard_only(self):
        results = self.run_all(fail=["plugins/alpha"])
        failing = [i for i, (code, _, _) in enumerate(results) if code]
        self.assertEqual(len(failing), 1)
        code, out, _ = results[failing[0]]
        self.assertIn("::error::go test failed: plugins/alpha", out)
        self.assertIn("--- FAIL: TestX", out)

    def test_refuses_to_run_with_a_provider_key_set(self):
        with fake_world({"OPENAI_API_KEY": "x"}), mock.patch.object(shards_mod.subprocess, "run", FakeRun()) as fake:
            with self.assertRaises(SystemExit):
                run_quiet(shards_mod.run_shard, 1, self.N)
        self.assertEqual(fake.calls, [])

    def test_prints_the_key_count(self):
        code, out, _ = self.run_all()[0]
        self.assertIn("*_API_KEY variables in this environment: 0", out)


if __name__ == "__main__":
    unittest.main(verbosity=1)
