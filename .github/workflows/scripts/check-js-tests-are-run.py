#!/usr/bin/env python3
"""Fails when a JavaScript or TypeScript test file has no CI runner (TXE-7371).

The ui vitest suite ran in no CI job. pricingFields.test.ts pinned the number of pricing fields,
two pricing PRs each changed it from the same base, and main went red with nobody seeing it.
tests/e2e/api/lib/pricing.test.js ran in no job either: the offline suites found *.test.mjs only.

Rule: every tracked file named *.test.* or *.spec.* with a JavaScript or TypeScript extension
matches a RUNNERS pattern, and the files that runner depends on still hold the command and the
discovery that run it (outside comment lines). A pattern names exactly what its runner's own
discovery (a package script's glob, a vitest include) picks up, and the discovery is a
requirement too, so a file the runner would skip fails here instead of passing unrun. check-check-scripts-are-run.py separately keeps each runner script run by a
workflow. A file no runner matches fails, unless NOT_RUN lists its directory with the reason or
the ticket that wires it in. A RUNNERS or NOT_RUN entry that matches no tracked file fails too,
so neither list can go stale.

Usage: check-js-tests-are-run.py [--self-test]
"""
import re
import subprocess
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parents[3]
WORKFLOW = ".github/workflows/api-collections-offline.yml"
OFFLINE = ".github/workflows/scripts/test-api-collections-offline.sh"
INTEGRATIONS = ".github/workflows/scripts/test-integrations.sh"
E2E_WORKFLOW = ".github/workflows/e2e-ui-offline.yml"
E2E_RUNNER = "tests/e2e/scripts/run-e2e.mjs"
E2E_PLAN = "tests/e2e/scripts/e2e-plan.mjs"

# A test file: name.test.ext or name.spec.ext, ext a JavaScript or TypeScript one.
TEST_FILE = re.compile(r"\.(test|spec)\.[cm]?[jt]sx?$")

# (pattern, requirements). The pattern is a regular expression over the tracked path and must
# name exactly the files the runner's own discovery picks up, so a file it would skip matches
# nothing here and fails. Each requirement is (file, text, present): the file holds the text
# outside comment lines (present), or does not hold it (not present).
RUNNERS = [
    # vitest's default include is every *.test.* / *.spec.* JavaScript or TypeScript file, so ui's
    # config must not narrow it.
    (r"^ui/.+\.(test|spec)\.[cm]?[jt]sx?$", [(WORKFLOW, "run: npx vitest run", True), ("ui/vitest.config.ts", "include", False)]),
    (r"^tests/e2e/api/.+\.test\.[cm]?js$", [(OFFLINE, "-name '*.test.mjs' -o -name '*.test.js' -o -name '*.test.cjs'", True)]),
    (
        r"^tests/integrations/typescript/tests/.+\.test\.ts$",
        [
            (INTEGRATIONS, 'cd "$TYPESCRIPT_TEST_DIR" && npm test', True),
            ("tests/integrations/typescript/package.json", '"test": "vitest run"', True),
            ("tests/integrations/typescript/vitest.config.ts", "include: ['tests/**/*.test.ts']", True),
        ],
    ),
    (
        r"^\.github/workflows/tools/[^/]+\.test\.js$",
        [(WORKFLOW, "run: npm test", True), (".github/workflows/tools/package.json", '"test": "node --test *.test.js"', True)],
    ),
    (r"^\.claude/skills/security-audit/[^/]+\.test\.cjs$", [(WORKFLOW, "run: node --test .claude/skills/security-audit/*.test.cjs", True)]),
    # The Playwright UI suite, offline in two shards (TXE-7385). run-e2e.mjs takes specFiles from
    # e2e-plan.mjs: every *.spec.ts under features/ except the accessibility audit (its own config).
    (
        r"^tests/e2e/features/(?!accessibility/).+\.spec\.ts$",
        [
            (E2E_WORKFLOW, "run-e2e.mjs --offline --shard", True),
            (E2E_PLAN, "d.name.endsWith('.spec.ts')", True),
            (E2E_PLAN, "d.name === 'accessibility' ? []", True),
            (E2E_RUNNER, "specFiles(E2E_ROOT)", True),
        ],
    ),
    # The runner's own tests: the shard split and the quarantine rules (TXE-7385).
    (r"^tests/e2e/scripts/[^/]+\.test\.mjs$", [(E2E_WORKFLOW, "run: node --test tests/e2e/scripts/*.test.mjs", True)]),
    # The sample suite the quarantine test runs through Playwright.
    (
        r"^tests/e2e/selftest/quarantine/[^/]+\.spec\.ts$",
        [
            ("tests/e2e/scripts/quarantine.test.mjs", "selftest/playwright.quarantine.config.ts", True),
            ("tests/e2e/selftest/playwright.quarantine.config.ts", 'testDir: "./quarantine"', True),
        ],
    ),
]

# Directories whose test files deliberately run in no CI job, and why.
NOT_RUN = {
    "tests/e2e/features/accessibility/": "an axe-core audit that scores every route against MIN_SCORE and writes a report (make run-a11y-audit, playwright.a11y.config.ts), not a pass/fail suite; the offline UI job leaves it out",
}


def code_lines(text):
    """The text without comment-only lines: a command in a comment runs nothing."""
    return "\n".join(line for line in text.splitlines() if not line.lstrip().startswith(("#", "//")))


def unrun(tests, runner_texts, runners=RUNNERS, not_run=NOT_RUN):
    """tests lists tracked test paths; runner_texts maps a runner file to its text (None when
    missing). Returns one line per defect; empty passes."""
    found = []
    live = {}
    for pattern, requirements in runners:
        for runner, text, present in requirements:
            source = runner_texts.get(runner)
            if source is None:
                found.append(f"{pattern}: {runner} does not exist; update RUNNERS in check-js-tests-are-run.py")
            elif present and text not in code_lines(source):
                found.append(f"{pattern}: {runner} no longer holds `{text}`, so these tests may run in no job; restore it or point RUNNERS at what runs them now")
            elif not present and text in code_lines(source):
                found.append(f"{pattern}: {runner} now holds `{text}`, which can narrow what runs; make the RUNNERS pattern match exactly what it runs")
        live[pattern] = 0
    used = {prefix: 0 for prefix in not_run}
    for path in sorted(tests):
        matched = [pattern for pattern, _ in runners if re.search(pattern, path)]
        for pattern in matched:
            live[pattern] += 1
        skipped = [prefix for prefix in not_run if path.startswith(prefix)]
        for prefix in skipped:
            used[prefix] += 1
        if not matched and not skipped:
            found.append(f"{path}: no CI job runs it; add it to a runner a required job runs and to RUNNERS in check-js-tests-are-run.py, or list its directory in NOT_RUN with the reason")
    for pattern, count in live.items():
        if count == 0:
            found.append(f"RUNNERS pattern {pattern} matches no tracked test file; remove the entry")
    for prefix, count in used.items():
        if count == 0:
            found.append(f"NOT_RUN names {prefix}, which holds no tracked test file; remove the entry")
    return found


def self_test():
    wf, sh, cfg, pkg = "ci.yml", "suite.sh", "ui/vitest.config.ts", "tools/package.json"
    runners = [
        (r"^ui/.+\.(test|spec)\.[cm]?[jt]sx?$", [(wf, "run: npx vitest run", True), (cfg, "include", False)]),
        (r"^api/.+\.test\.mjs$", [(sh, "-name '*.test.mjs'", True)]),
        (r"^tools/[^/]+\.test\.js$", [(wf, "run: npm test", True), (pkg, '"test": "node --test *.test.js"', True)]),
    ]
    texts = {
        wf: "steps:\n  - run: npx vitest run\n  - run: npm test\n",
        sh: "find . -name '*.test.mjs'\n",
        cfg: "export default {}\n",
        pkg: '{"scripts": {"test": "node --test *.test.js"}}\n',
    }
    base = ["ui/a.test.ts", "api/x.test.mjs", "tools/t.test.js"]
    cases = [
        ("every test file has a runner", base + ["ui/b/c.spec.tsx"], texts, {}, 0),
        ("a test file in a subdirectory the runner's glob skips", base + ["tools/sub/u.test.js"], texts, {}, 1),
        ("a .spec file where the runner finds .test only", base + ["api/y.spec.mjs"], texts, {}, 1),
        ("the package script narrowed to one file", base, {**texts, pkg: '{"scripts": {"test": "node --test t.test.js"}}'}, {}, 1),
        ("the vitest config narrowed with an include", base, {**texts, cfg: "export default { test: { include: ['a/**'] } }\n"}, {}, 1),
    ]
    base_cases = [
        ("every test file has a runner", ["ui/a.test.ts", "ui/b/c.spec.tsx", "api/x.test.mjs"], texts, {}, 0),
        ("a test file no runner matches", ["ui/a.test.ts", "api/x.test.mjs", "other/y.test.ts"], texts, {}, 1),
        ("an extension the runner does not find", ["ui/a.test.ts", "api/x.test.mjs", "api/lib/z.test.js"], texts, {}, 1),
        ("the runner's command removed", ["ui/a.test.ts", "api/x.test.mjs"], {**texts, wf: "steps:\n  - run: npm test\n"}, {}, 1),
        ("the runner's command only in a comment", ["ui/a.test.ts", "api/x.test.mjs"], {**texts, wf: "# run: npx vitest run\n  - run: npm test\n"}, {}, 1),
        ("a runner's file gone", ["ui/a.test.ts", "api/x.test.mjs"], {k: v for k, v in texts.items() if k != sh}, {}, 1),
        ("a runner that matches nothing", ["ui/a.test.ts"], texts, {}, 1),
        ("a directory listed as not run", ["ui/a.test.ts", "api/x.test.mjs", "e2e/p.spec.ts"], texts, {"e2e/": "TXE-1: reason"}, 0),
        ("a stale not-run entry", ["ui/a.test.ts", "api/x.test.mjs"], texts, {"e2e/": "TXE-1: reason"}, 1),
    ]
    cases += [(label, tests + ["tools/t.test.js"], runner_texts, not_run, want) for label, tests, runner_texts, not_run, want in base_cases]
    failed = 0
    for label, tests, runner_texts, not_run, want in cases:
        got = len(unrun(tests, runner_texts, runners, not_run))
        if got != want:
            failed += 1
            print(f"  FAIL - {label}: {got} findings, want {want}")
        else:
            print(f"  ok - {label}")
    return failed


def main():
    if sys.argv[1:] == ["--self-test"]:
        failed = self_test()
        print("self-test: all passed" if failed == 0 else f"self-test: {failed} failed")
        return 1 if failed else 0
    if sys.argv[1:]:
        print(__doc__)
        return 2
    listed = subprocess.run(["git", "-C", str(REPO), "ls-files", "-z"], check=True, capture_output=True, text=True).stdout.split("\0")
    tests = [name for name in listed if name and "/node_modules/" not in name and TEST_FILE.search(name)]
    if len(tests) < 150:
        print(f"found {len(tests)} test files, want the whole set: the listing no longer works")
        return 1
    runner_texts = {}
    for _, requirements in RUNNERS:
        for runner, _, _ in requirements:
            path = REPO / runner
            runner_texts[runner] = path.read_text(encoding="utf-8") if path.is_file() else None
    found = unrun(tests, runner_texts)
    if found:
        print("\n".join(found))
        return 1
    skipped = sum(1 for name in tests if any(name.startswith(prefix) for prefix in NOT_RUN))
    print(f"ok - {len(tests) - skipped} of {len(tests)} JavaScript and TypeScript test files have a CI runner ({skipped} in NOT_RUN: {', '.join(NOT_RUN)})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
