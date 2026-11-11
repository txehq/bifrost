#!/usr/bin/env python3
"""Fails when a check or test script in .github/workflows/scripts is run by nothing (TXE-7111).

A check nobody runs rots without anyone seeing it. validate-go-config-fields.sh compared the Go
config structs with config.schema.json and was called by no workflow; its exclusion list fell
behind schemasync's, and it failed on main for eight fields that are left out of the schema on
purpose. Its red result then read as "main is broken".

Rule: every tracked file directly under .github/workflows/scripts whose name says it is a check
(check-*, validate-*, test-*, *.test.*) must be reachable from a workflow, a composite action or
the Makefile: named by one of them, or by a script that is itself reachable. A name that appears
only in a comment line does not count, and neither does a name in this file, which lists scripts
(NOT_RUN) without running any.

NOT_RUN lists the scripts that are deliberately run by nothing, each with its reason or the
ticket that wires it in. An entry for a script that is reachable, or that no longer exists,
fails too, so the list cannot go stale.

Usage: check-check-scripts-are-run.py [--self-test]
"""
import fnmatch
import subprocess
import sys
from pathlib import Path, PurePosixPath

REPO = Path(__file__).resolve().parents[3]
SCRIPTS = ".github/workflows/scripts"
CHECK_NAMES = ["check-*", "validate-*", "test-*", "*.test.*"]
# This file names the scripts in NOT_RUN and runs none of them, so it is never read as a caller.
SELF = SCRIPTS + "/" + Path(__file__).name

# Scripts run by no workflow, action, Makefile target or reachable script, and why.
NOT_RUN = {
    "check-dependency-flow.sh": "not a check: a release helper a person runs to see the next step",
    "test-e2e-api.sh": "not a check: a local runner for the live API collections, which CI starts through the Makefile targets",
    "validate-go-config-fields.sh": "kept for its name only: it runs validate-schema-sync.sh, which every pull request runs",
}


def is_check(name):
    return any(fnmatch.fnmatch(name, pattern) for pattern in CHECK_NAMES)


def code_lines(text):
    """The text without comment-only lines: a script named in a comment is not run by it."""
    return "\n".join(line for line in text.splitlines() if not line.lstrip().startswith(("#", "//")))


def unrun(files, not_run, self_path=SELF):
    """files maps a tracked path to its text. Returns one line per defect; empty passes."""
    scripts = {path: text for path, text in files.items() if PurePosixPath(path).parent.as_posix() == SCRIPTS}
    roots = {
        path: text
        for path, text in files.items()
        if (path.startswith(".github/workflows/") and path.endswith((".yml", ".yaml")) and "/" not in path[len(".github/workflows/") :])
        or path.startswith(".github/actions/")
        or path == "Makefile"
        or path.endswith(".mk")
    }
    reachable = set()
    frontier = [code_lines(text) for text in roots.values()]
    while frontier:
        text = frontier.pop()
        for path, script_text in scripts.items():
            if path not in reachable and PurePosixPath(path).name in text:
                reachable.add(path)
                if path != self_path:
                    frontier.append(code_lines(script_text))
    found = []
    names = {PurePosixPath(path).name: path for path in scripts}
    for name, path in sorted(names.items()):
        if not is_check(name):
            continue
        if path not in reachable and name not in not_run:
            found.append(f"{path}: no workflow, action, Makefile target or reachable script runs it; wire it into a job, or add it to NOT_RUN in check-check-scripts-are-run.py with the reason")
    for name in sorted(not_run):
        if name not in names:
            found.append(f"NOT_RUN names {name}, which is not in {SCRIPTS}; remove the entry")
        elif names[name] in reachable:
            found.append(f"NOT_RUN names {name}, which is now run; remove the entry")
        elif not is_check(name):
            found.append(f"NOT_RUN names {name}, which this check does not cover; remove the entry")
    return found


def self_test():
    wf = ".github/workflows/ci.yml"
    s = SCRIPTS + "/"
    cases = [
        ("a check a workflow runs", {wf: "run: ./.github/workflows/scripts/check-a.py", s + "check-a.py": ""}, {}, 0),
        ("a check nothing runs", {wf: "run: true", s + "validate-b.sh": ""}, {}, 1),
        ("a check named only in a workflow comment", {wf: "    # see check-a.py\nrun: true", s + "check-a.py": ""}, {}, 1),
        ("a test reached through a script a workflow runs", {wf: "run: bash suite.sh", s + "suite.sh": 'python3 "$DIR/x.test.py"', s + "x.test.py": ""}, {}, 0),
        ("a test named only by an unreachable script", {wf: "run: true", s + "helper.sh": "bash test-c.sh", s + "test-c.sh": ""}, {}, 1),
        ("the Makefile counts as a root", {"Makefile": "\t./.github/workflows/scripts/test-d.sh", s + "test-d.sh": ""}, {}, 0),
        ("a script that is not a check is not covered", {wf: "run: true", s + "release-notes.sh": ""}, {}, 0),
        ("a listed script nothing runs", {wf: "run: true", s + "check-e.sh": ""}, {"check-e.sh": "TXE-1: reason"}, 0),
        ("a stale entry: the script is run now", {wf: "run: check-e.sh", s + "check-e.sh": ""}, {"check-e.sh": "TXE-1: reason"}, 1),
        ("a stale entry: the script is gone", {wf: "run: true"}, {"check-f.sh": "reason"}, 1),
        (
            "this checker, run by a workflow, does not make the scripts its list names look run",
            {wf: "run: python3 " + SELF, SELF: 'NOT_RUN = {"check-h.sh": "reason"}', s + "check-h.sh": ""},
            {"check-h.sh": "reason"},
            0,
        ),
        (
            "any other reachable script that names a check does run it",
            {wf: "run: bash runner.sh", s + "runner.sh": 'LIST = {"check-h.sh": "reason"}', s + "check-h.sh": ""},
            {"check-h.sh": "reason"},
            1,
        ),
        ("a nested workflow file is not a root", {".github/workflows/scripts/fixtures/w.yml": "run: check-g.sh", s + "check-g.sh": ""}, {}, 1),
    ]
    failed = 0
    for label, files, not_run, want in cases:
        got = len(unrun(files, not_run))
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
    listed = subprocess.run(["git", "-C", str(REPO), "ls-files", "-z", ".github", "Makefile", "*.mk"], check=True, capture_output=True, text=True).stdout.split("\0")
    files = {}
    for name in listed:
        path = REPO / name
        if name and path.is_file():
            files[name] = path.read_text(encoding="utf-8", errors="replace")
    checks = [name for name in files if PurePosixPath(name).parent.as_posix() == SCRIPTS and is_check(PurePosixPath(name).name)]
    if len(checks) < 40:
        print(f"found {len(checks)} check scripts in {SCRIPTS}, want the whole set: the listing no longer works")
        return 1
    found = unrun(files, NOT_RUN)
    if found:
        print("\n".join(found))
        return 1
    print(f"ok - every one of the {len(checks)} check and test scripts in {SCRIPTS} is run by a workflow, an action, the Makefile or a script they run ({len(NOT_RUN)} listed as deliberately not run)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
