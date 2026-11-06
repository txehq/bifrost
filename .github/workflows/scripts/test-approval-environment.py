#!/usr/bin/env python3
"""Checks which approval environment the pull_request workflows pick.

Upstream 9c594766d (#8869) puts the root job of codspeed, dependency-review and
workflow-lint in the pr-testing environment for an outside pull request, so the run
waits for a reviewer, and in ci-trusted-branch otherwise. This repository changes the
test (TXE-5856): a pull request goes to pr-testing exactly when its head branch is in
another repository. Upstream's head.repo.fork is true for this fork's own branches,
and its author_association test misreads txehq members, whose membership is private,
as outsiders.

This script reads each job's `environment:` expression from the workflow files and
evaluates it, with a small evaluator for the GitHub expression subset these lines use,
against the events below. It fails when a case picks the wrong environment, when a
listed job loses its expression, or when another job starts naming pr-testing
without being added here.

Usage: test-approval-environment.py
"""
import glob
import json
import os
import re
import sys

import yaml

ROOT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")

# (workflow file, job id) for every job that must carry the approval expression.
GATED_JOBS = {
    ("codspeed.yml", "benchmarks"),
    ("dependency-review.yml", "dependency-review"),
    ("workflow-lint.yml", "check-egress-allowlist"),
}

REPO = "txehq/bifrost"


def pr_event(head_repo, author_association, head_is_fork):
    head = None if head_repo is None else {"full_name": head_repo, "fork": head_is_fork}
    return {
        "event_name": "pull_request",
        "repository": REPO,
        "event": {
            "pull_request": {
                "author_association": author_association,
                "head": {"repo": head},
            }
        },
    }


def plain_event(name):
    return {"event_name": name, "repository": REPO, "event": {}}


CASES = [
    # name, github context, expected environment
    ("external fork pull request", pr_event("someone/bifrost", "NONE", True), "pr-testing"),
    ("member's own fork", pr_event("connorwang/bifrost", "MEMBER", True), "pr-testing"),
    ("head repository deleted", pr_event(None, "CONTRIBUTOR", None), "pr-testing"),
    # head.repo.fork is true here because txehq/bifrost is itself a fork, and a private
    # member is reported as CONTRIBUTOR: PR #94's first head was sent to pr-testing.
    ("same-repository branch, private member", pr_event(REPO, "CONTRIBUTOR", True), "ci-trusted-branch"),
    ("same-repository branch, public member", pr_event(REPO, "MEMBER", True), "ci-trusted-branch"),
    ("same-repository branch, bot", pr_event(REPO, "NONE", True), "ci-trusted-branch"),
    ("push", plain_event("push"), "ci-trusted-branch"),
    ("manual run", plain_event("workflow_dispatch"), "ci-trusted-branch"),
]

# Upstream's line, kept to show why the fork differs: it holds a member's branch.
UPSTREAM = (
    "${{ github.event_name == 'pull_request' && (github.event.pull_request.head.repo.fork"
    " || !contains(fromJSON('[\"OWNER\",\"MEMBER\"]'), github.event.pull_request.author_association))"
    " && 'pr-testing' || 'ci-trusted-branch' }}"
)


# --- a GitHub expression evaluator for the subset these lines use -------------------

TOKEN = re.compile(
    r"\s*(?:(?P<str>'(?:[^']|'')*')|(?P<op>==|!=|&&|\|\||[!(),])|(?P<name>[A-Za-z_][A-Za-z0-9_.-]*))"
)


def tokenize(src):
    pos, out = 0, []
    src = src.strip()
    while pos < len(src):
        m = TOKEN.match(src, pos)
        if not m or m.end() == pos:
            raise ValueError(f"cannot read expression at {src[pos:]!r}")
        pos = m.end()
        if m.group("str") is not None:
            out.append(("str", m.group("str")[1:-1].replace("''", "'")))
        elif m.group("op") is not None:
            out.append(("op", m.group("op")))
        else:
            out.append(("name", m.group("name")))
    return out


def truthy(v):
    return v not in (None, False, "", 0)


def equal(a, b):
    if isinstance(a, str) and isinstance(b, str):
        return a.lower() == b.lower()
    return a == b


class Evaluator:
    def __init__(self, tokens, context):
        self.t, self.i, self.ctx = tokens, 0, context

    def peek(self):
        return self.t[self.i] if self.i < len(self.t) else (None, None)

    def take(self, value=None):
        tok = self.peek()
        if value is not None and tok[1] != value:
            raise ValueError(f"expected {value!r}, got {tok!r}")
        self.i += 1
        return tok

    def parse(self):
        v = self.or_()
        if self.i != len(self.t):
            raise ValueError(f"unexpected {self.peek()!r}")
        return v

    def or_(self):
        v = self.and_()
        while self.peek() == ("op", "||"):
            self.take()
            rhs = self.and_()
            v = v if truthy(v) else rhs
        return v

    def and_(self):
        v = self.cmp()
        while self.peek() == ("op", "&&"):
            self.take()
            rhs = self.cmp()
            v = rhs if truthy(v) else v
        return v

    def cmp(self):
        v = self.unary()
        while self.peek() in (("op", "=="), ("op", "!=")):
            op = self.take()[1]
            rhs = self.unary()
            v = equal(v, rhs) if op == "==" else not equal(v, rhs)
        return v

    def unary(self):
        if self.peek() == ("op", "!"):
            self.take()
            return not truthy(self.unary())
        return self.primary()

    def primary(self):
        kind, val = self.take()
        if kind == "str":
            return val
        if (kind, val) == ("op", "("):
            v = self.or_()
            self.take(")")
            return v
        if kind == "name" and self.peek() == ("op", "("):
            self.take()
            args = [self.or_()]
            while self.peek() == ("op", ","):
                self.take()
                args.append(self.or_())
            self.take(")")
            return self.call(val, args)
        if kind == "name":
            return self.lookup(val)
        raise ValueError(f"unexpected {val!r}")

    def call(self, name, args):
        if name == "fromJSON":
            return json.loads(args[0])
        if name == "contains":
            hay, needle = args
            if isinstance(hay, list):
                return any(equal(x, needle) for x in hay)
            return isinstance(hay, str) and isinstance(needle, str) and needle.lower() in hay.lower()
        raise ValueError(f"unsupported function {name}")

    def lookup(self, dotted):
        parts = dotted.split(".")
        if parts[0] != "github":
            raise ValueError(f"unsupported context {dotted}")
        v = self.ctx
        for p in parts[1:]:
            v = v.get(p) if isinstance(v, dict) else None
        return v


def evaluate(expr, context):
    m = re.fullmatch(r"\s*\$\{\{(.*)\}\}\s*", expr, re.S)
    if not m:
        raise ValueError(f"not a single expression: {expr!r}")
    return Evaluator(tokenize(m.group(1)), context).parse()


# --- the checks ---------------------------------------------------------------------

def environment_jobs():
    found = {}
    for path in sorted(glob.glob(os.path.join(ROOT, "*.yml"))):
        with open(path) as fh:
            doc = yaml.safe_load(fh) or {}
        for job_id, job in (doc.get("jobs") or {}).items():
            env = job.get("environment") if isinstance(job, dict) else None
            if isinstance(env, dict):
                env = env.get("name")
            if isinstance(env, str) and "${{" in env and "pr-testing" in env:
                found[(os.path.basename(path), job_id)] = env
    return found


def main():
    failures = []
    jobs = environment_jobs()
    for key in sorted(GATED_JOBS - jobs.keys()):
        failures.append(f"{key[0]} job {key[1]}: no approval environment expression")
    for key in sorted(jobs.keys() - GATED_JOBS):
        failures.append(f"{key[0]} job {key[1]}: names pr-testing but is not listed in GATED_JOBS")

    checked = 0
    for key in sorted(jobs.keys() & GATED_JOBS):
        for name, ctx, want in CASES:
            got = evaluate(jobs[key], ctx)
            checked += 1
            if got != want:
                failures.append(f"{key[0]} job {key[1]}, {name}: picked {got!r}, want {want!r}")

    # The evaluator must reproduce the defect that made the fork differ, or the
    # cases above prove nothing about it.
    member = pr_event(REPO, "CONTRIBUTOR", True)
    if evaluate(UPSTREAM, member) != "pr-testing":
        failures.append("evaluator self-check: upstream's expression should hold a private member's branch")
    if evaluate(UPSTREAM, plain_event("push")) != "ci-trusted-branch":
        failures.append("evaluator self-check: upstream's expression should not hold a push")

    for f in failures:
        print(f"FAIL {f}")
    print(f"{checked} cases across {len(jobs.keys() & GATED_JOBS)} jobs, {len(failures)} failures")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
