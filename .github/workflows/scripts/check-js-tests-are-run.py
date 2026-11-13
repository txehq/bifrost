#!/usr/bin/env python3
"""Fails when a JavaScript or TypeScript test file has no CI runner (TXE-7371).

The ui vitest suite ran in no CI job. pricingFields.test.ts pinned the number of pricing fields,
two pricing PRs each changed it from the same base, and main went red with nobody seeing it.
tests/e2e/api/lib/pricing.test.js ran in no job either: the offline suites found *.test.mjs only.

Rule: every tracked file named *.test.* or *.spec.* with a JavaScript or TypeScript extension
matches a RUNNERS pattern, and that runner's file still holds the command that runs it (outside
comment lines). check-check-scripts-are-run.py separately keeps each runner script run by a
workflow. A file no runner matches fails, unless NOT_RUN lists its directory with the reason or
the ticket that wires it in. A RUNNERS or NOT_RUN entry that matches no tracked file fails too,
so neither list can go stale.

Usage: check-js-tests-are-run.py [--self-test]
"""
import fnmatch
import re
import subprocess
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parents[3]
WORKFLOW = ".github/workflows/api-collections-offline.yml"
OFFLINE = ".github/workflows/scripts/test-api-collections-offline.sh"
INTEGRATIONS = ".github/workflows/scripts/test-integrations.sh"

# A test file: name.test.ext or name.spec.ext, ext a JavaScript or TypeScript one.
TEST_FILE = re.compile(r"\.(test|spec)\.[cm]?[jt]sx?$")

# (pattern, runner file, the command in it that runs those files). fnmatch's * crosses "/".
RUNNERS = [
    ("ui/*", WORKFLOW, "run: npx vitest run"),
    # .mjs, .js and .cjs; a .ts or .spec. file there is found by nothing and fails.
    ("tests/e2e/api/*.test.*js", OFFLINE, "-name '*.test.mjs' -o -name '*.test.js' -o -name '*.test.cjs'"),
    ("tests/integrations/typescript/tests/*", INTEGRATIONS, 'cd "$TYPESCRIPT_TEST_DIR" && npm test'),
    (".github/workflows/tools/*.test.js", WORKFLOW, "run: npm test"),
    (".claude/skills/security-audit/*.test.cjs", WORKFLOW, "run: node --test .claude/skills/security-audit/*.test.cjs"),
]

# Directories whose test files deliberately run in no CI job, and why.
NOT_RUN = {
    "tests/e2e/features/": "TXE-7385: the Playwright UI suite needs a built gateway and UI; e2e-tests.yml fires only on push to an upstream branch",
}


def code_lines(text):
    """The text without comment-only lines: a command in a comment runs nothing."""
    return "\n".join(line for line in text.splitlines() if not line.lstrip().startswith(("#", "//")))


def unrun(tests, runner_texts, runners=RUNNERS, not_run=NOT_RUN):
    """tests lists tracked test paths; runner_texts maps a runner file to its text (None when
    missing). Returns one line per defect; empty passes."""
    found = []
    live = {}
    for pattern, runner, command in runners:
        text = runner_texts.get(runner)
        if text is None:
            found.append(f"{pattern}: its runner {runner} does not exist; update RUNNERS in check-js-tests-are-run.py")
        elif command not in code_lines(text):
            found.append(f"{pattern}: {runner} no longer runs `{command}`, so these tests run in no job; restore it or point RUNNERS at what runs them now")
        live[pattern] = 0
    used = {prefix: 0 for prefix in not_run}
    for path in sorted(tests):
        matched = [pattern for pattern, _, _ in runners if fnmatch.fnmatch(path, pattern)]
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
    wf, sh = "ci.yml", "suite.sh"
    runners = [("ui/*", wf, "run: npx vitest run"), ("api/*.test.mjs", sh, "-name '*.test.mjs'")]
    texts = {wf: "steps:\n  - run: npx vitest run\n", sh: "find . -name '*.test.mjs'\n"}
    cases = [
        ("every test file has a runner", ["ui/a.test.ts", "ui/b/c.spec.tsx", "api/x.test.mjs"], texts, {}, 0),
        ("a test file no runner matches", ["ui/a.test.ts", "api/x.test.mjs", "other/y.test.ts"], texts, {}, 1),
        ("an extension the runner does not find", ["ui/a.test.ts", "api/x.test.mjs", "api/lib/z.test.js"], texts, {}, 1),
        ("the runner's command removed", ["ui/a.test.ts", "api/x.test.mjs"], {wf: "steps: []\n", sh: texts[sh]}, {}, 1),
        ("the runner's command only in a comment", ["ui/a.test.ts", "api/x.test.mjs"], {wf: "# run: npx vitest run\n", sh: texts[sh]}, {}, 1),
        ("the runner file gone", ["ui/a.test.ts", "api/x.test.mjs"], {sh: texts[sh]}, {}, 1),
        ("a runner that matches nothing", ["ui/a.test.ts"], texts, {}, 1),
        ("a directory listed as not run", ["ui/a.test.ts", "api/x.test.mjs", "e2e/p.spec.ts"], texts, {"e2e/": "TXE-1: reason"}, 0),
        ("a stale not-run entry", ["ui/a.test.ts", "api/x.test.mjs"], texts, {"e2e/": "TXE-1: reason"}, 1),
    ]
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
    for _, runner, _ in RUNNERS:
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
