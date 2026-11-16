#!/usr/bin/env python3
"""Fails on a job that gets Go other than through ./.github/actions/setup-go (TXE-7621).

That action is where Go's version and the retrying GOPROXY live: a transient proxy.golang.org
failure (an HTTP/2 stream error on 16 Nov 2026) failed required jobs that had to be rerun by hand.
A job that installs Go itself, or sets its own GOPROXY, gets no retries, so this check fails on:

  1. a step that uses actions/setup-go directly, in a workflow or in any composite action other
     than .github/actions/setup-go;
  2. a job that runs go without a ./.github/actions/setup-go step. "Runs go" is a go command in a
     step's script, a `make` (the Makefile's targets build Go), or either inside a repository
     script the step calls, followed three deep;
  3. a ./.github/actions/setup-go step before the job's actions/checkout step (a local action is
     read from the checked-out tree);
  4. GOPROXY set in a workflow, job or step env, or assigned in a step's script. GOPROXY=off
     (the offline guard some test scripts set) is allowed.

Usage: check-go-setup.py [--root DIR] [workflow.yml ...]
  With no files, checks every workflow in DIR/.github/workflows and every composite action in
  DIR/.github/actions. DIR defaults to the repository root.
"""
import argparse
import glob
import os
import re
import sys

import yaml

WRAPPER = "./.github/actions/setup-go"

# Command position: the start of a line, after && || ; | ( or then/do, behind VAR=value
# assignments. A go command inside a quoted string or prose is not run.
ENV = r"(?:[A-Z_][A-Z0-9_]*=\S*[ \t]+)*"
CMD_POS = (
    r"(?:^|&&|\|\||;|\(|\bthen\b|\bdo\b|\bif\b|\belif\b|\bwhile\b|\buntil\b)[ \t]*(?:![ \t]+)?"
    + ENV + r"(?:sudo[ \t]+(?:-\S+[ \t]+)*)?"
)
GO_RE = re.compile(
    CMD_POS + ENV
    + r"go[ \t]+(?:build|test|run|vet|mod|install|list|generate|work|tool|get)\b",
    re.MULTILINE,
)
MAKE_RE = re.compile(CMD_POS + ENV + r"make\b(?![-.\w])", re.MULTILINE)
# A Python script that runs go builds an argv list whose first element is "go"; a Node script
# passes "go" as the command to a child_process call or a run() helper (tests/e2e/scripts/run-e2e.mjs).
PY_GO_RE = re.compile(r"""\[\s*["']go["']\s*,""")
JS_GO_RE = re.compile(r"""\b(?:run|spawn|spawnSync|execFile|execFileSync)\(\s*["']go["']""")
# A script the step calls: behind an interpreter, where it may be quoted (bash "$DIR/x.sh"), or as
# a bare command, where it may not (a quoted name at the start of a line is data, such as the
# script names in check-egress-allowlist.sh's own lists).
SCRIPT_PATH = r"((?:\$\{?[A-Za-z_]+\}?/|\./)?[A-Za-z0-9_./${}-]+\.(?:sh|py|mjs|js))\b"
# Inside a script, a script path assigned to a variable is followed too: the script runs it later
# through the variable (RELEASE_SCRIPT="./.github/workflows/scripts/release-single-plugin.sh").
# A path in any other argument is data (a grep or cp of the script), so it is not followed.
PATHY_SCRIPT_RE = re.compile(
    r"""(?:^|[\s;])[A-Za-z_][A-Za-z0-9_]*=["']?((?:\$\{?[A-Za-z_]+\}?/|\./|[A-Za-z0-9_.-]+/)[A-Za-z0-9_./${}-]*\.(?:sh|py|mjs|js))\b""",
    re.MULTILINE,
)
SCRIPT_RE = re.compile(
    CMD_POS
    + r"(?:(?:bash|sh|source|python3?|node|exec)[ \t]+(?:-\S+[ \t]+)*[\"']?|\.[ \t]+[\"']?)?"
    + SCRIPT_PATH,
    re.MULTILINE,
)
# In a step's own script every script path is followed, wherever it stands on the line: a wrapper
# such as scripts/test-offline.sh runs the command after it (`test-offline.sh python3 x.py`).
ANY_SCRIPT_RE = re.compile(r"""(?:^|[\s"'=(])""" + SCRIPT_PATH, re.MULTILINE)
# A script that writes its own `go` (a stub on PATH, as build-executables.test.sh does) runs that,
# not the toolchain.
FAKE_GO_RE = re.compile(r""">[ \t]*["']?\$\{?[A-Za-z_]+\}?(?:/[A-Za-z0-9_.-]+)*/go["']?[ \t]*<<""")
GOPROXY_ASSIGN = re.compile(r"(?:^|[\s;&|(])(?:export[ \t]+)?GOPROXY=(?!off\b)", re.MULTILINE)


HEREDOC_RE = re.compile(r"""<<(-?)[ \t]*(["']?)([A-Za-z_][A-Za-z0-9_]*)\2""")


def strip_comments(text):
    """Shell code without whole-line comments and without heredoc bodies. A heredoc body is data
    the script writes (check-egress-allowlist.test.sh writes fixture workflows that name
    test-core.sh), not commands it runs; its opening line, which says where it goes, stays."""
    out, end = [], None
    for line in text.splitlines():
        if end is not None:
            if (line.lstrip("\t") if end[0] else line) == end[1]:
                end = None
            continue
        if line.lstrip().startswith("#"):
            continue
        out.append(line)
        m = HEREDOC_RE.search(line)
        if m:
            end = (m.group(1) == "-", m.group(3))
    return "\n".join(out)


def resolve(root, ref):
    """The repository file a script reference names, or None. $VAR/ prefixes resolve against the
    repository root and the scripts directory, which is what the workflows' variables point at."""
    ref = ref.strip("'\"")
    rel = re.sub(r"^\$\{?[A-Za-z_]+\}?/", "", ref)
    rel = rel[2:] if rel.startswith("./") else rel
    for base in ("", ".github/workflows/scripts"):
        path = os.path.normpath(os.path.join(root, base, rel))
        if path.startswith(root) and os.path.isfile(path):
            return path
    return None


def runs_go(root, text, seen, depth=0):
    """True when text runs go or make, directly or through a repository script it calls."""
    if FAKE_GO_RE.search(text):
        return False
    if GO_RE.search(text) or MAKE_RE.search(text):
        return True
    if depth >= 3:
        return False
    refs = ANY_SCRIPT_RE.findall(text) if depth == 0 else SCRIPT_RE.findall(text) + PATHY_SCRIPT_RE.findall(text)
    for ref in refs:
        path = resolve(root, ref)
        if path is None or path in seen:
            continue
        seen.add(path)
        with open(path, errors="replace") as fh:
            code = fh.read()
        if path.endswith((".py", ".mjs", ".js")):
            # These run go only through an argv list or a child process call; the shell patterns
            # would read the script names and Makefile targets they handle as data (test-api-
            # collections-offline-callers.py reads the scripts it checks, and runs none of them).
            if (PY_GO_RE if path.endswith(".py") else JS_GO_RE).search(code):
                return True
            continue
        code = strip_comments(code)
        if runs_go(root, code, seen, depth + 1):
            return True
    return False


def env_sets_goproxy(env):
    return isinstance(env, dict) and "GOPROXY" in env and str(env["GOPROXY"]).strip() != "off"


def check_workflow(root, path):
    rel = os.path.relpath(path, root)
    with open(path) as fh:
        doc = yaml.safe_load(fh) or {}
    if env_sets_goproxy(doc.get("env")):
        yield f"{rel}: sets GOPROXY in the workflow env; {WRAPPER} owns it (rule 4)"
    for name, job in (doc.get("jobs") or {}).items():
        if not isinstance(job, dict):
            continue
        where = f"{rel}: job {name}"
        if env_sets_goproxy(job.get("env")):
            yield f"{where}: sets GOPROXY in the job env; {WRAPPER} owns it (rule 4)"
        steps = [s for s in (job.get("steps") or []) if isinstance(s, dict)]
        checkout_at = next((i for i, s in enumerate(steps) if (s.get("uses") or "").startswith("actions/checkout@")), None)
        wrapper_at = None
        go = False
        for i, step in enumerate(steps):
            uses = step.get("uses") or ""
            label = step.get("name") or uses or f"step {i + 1}"
            if uses.startswith("actions/setup-go@"):
                yield f"{where}: step '{label}' uses actions/setup-go directly; use {WRAPPER} (rule 1)"
            if uses == WRAPPER:
                wrapper_at = i if wrapper_at is None else wrapper_at
            if env_sets_goproxy(step.get("env")):
                yield f"{where}: step '{label}' sets GOPROXY in its env; {WRAPPER} owns it (rule 4)"
            run = strip_comments(step.get("run") or "")
            if GOPROXY_ASSIGN.search(run):
                yield f"{where}: step '{label}' assigns GOPROXY; {WRAPPER} owns it (rule 4)"
            if not go and run and runs_go(root, run, set()):
                go = (i, label)
        if go and wrapper_at is None:
            yield f"{where}: step '{go[1]}' runs go, but the job has no {WRAPPER} step (rule 2)"
        if wrapper_at is not None and (checkout_at is None or checkout_at > wrapper_at):
            yield f"{where}: {WRAPPER} runs before actions/checkout, so the runner cannot read it (rule 3)"


def check_action(root, path):
    rel = os.path.relpath(path, root)
    if os.path.dirname(rel) == ".github/actions/setup-go":
        return
    with open(path) as fh:
        doc = yaml.safe_load(fh) or {}
    for step in ((doc.get("runs") or {}).get("steps") or []):
        if isinstance(step, dict) and (step.get("uses") or "").startswith("actions/setup-go@"):
            yield f"{rel}: uses actions/setup-go; only {WRAPPER} may (rule 1)"


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--root", default=os.path.dirname(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))))
    p.add_argument("files", nargs="*")
    args = p.parse_args()
    root = os.path.abspath(args.root)
    workflows = args.files or sorted(glob.glob(os.path.join(root, ".github/workflows/*.yml")))
    actions = [] if args.files else sorted(glob.glob(os.path.join(root, ".github/actions/*/action.yml")))
    found = [p for f in workflows for p in check_workflow(root, os.path.abspath(f))]
    found += [p for f in actions for p in check_action(root, f)]
    for problem in found:
        print(f"FAIL: {problem}", file=sys.stderr)
    print(f"{len(workflows)} workflows and {len(actions)} composite actions checked, {len(found)} problems")
    return 1 if found else 0


if __name__ == "__main__":
    sys.exit(main())
