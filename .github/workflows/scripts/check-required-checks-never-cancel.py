#!/usr/bin/env python3
"""Fails when a run of a required check could end cancelled on a pull request's head.

GitHub keeps every check run a head gets. When a required check's run on the head ends
cancelled, the pull request stays BLOCKED beside a passing run of the same check, until
someone re-runs the cancelled one (TXE-7104: codex-review on PRs #157, #214 and #226).
A `concurrency` group is what cancels such a run: with cancel-in-progress a newer run
cancels the one in progress, and without it a newer queued run cancels the one pending.
Two runs on one head are ordinary: a review, a reply on a review thread or a body edit
each start codex-review again without moving the head. So a required check must decide
in every run it starts, and each run must reach its own verdict.

For every job whose check name is in REQUIRED_CHECKS (the branch protection of main),
this fails when:

  * the job's workflow declares `concurrency`;
  * the job, or any job it needs (directly or through other needs), declares
    `concurrency`: a cancelled need skips the required job;
  * the job has no `timeout-minutes`, or a `*_WAIT_SECONDS` value in its env or a step's
    env is not at least a minute shorter than that timeout: a timeout ends the run by
    cancelling it, not with the check's verdict.

With no arguments it reads every workflow in .github/workflows and also fails when a
required check has no job there, so a renamed job cannot leave the list stale. The list
mirrors the branch protection, which a pull request's token cannot read: change both
together.

Usage: check-required-checks-never-cancel.py [workflow.yml ...]
"""
import glob
import os
import re
import sys

import yaml

# The required status checks of main's branch protection.
REQUIRED_CHECKS = ("codex-review", "API collection offline suites")

WAIT_VAR = re.compile(r"_WAIT_SECONDS$")


def check_name(job_id, job):
    """The name GitHub gives the job's check run: its name, or its id without one."""
    name = job.get("name")
    return name if isinstance(name, str) and name else job_id


def needs_of(job):
    needs = job.get("needs") or []
    return [needs] if isinstance(needs, str) else list(needs)


def waits(job):
    """Every (where, name, value) of a *_WAIT_SECONDS env var in the job or its steps."""
    scopes = [("job env", job.get("env") or {})]
    for i, step in enumerate(job.get("steps") or []):
        label = step.get("name") or str(step.get("run") or "").split("\n")[0] or f"step {i + 1}"
        scopes.append((f"step '{label}'", step.get("env") or {}))
    for where, env in scopes:
        if not isinstance(env, dict):
            continue
        for name, value in env.items():
            if WAIT_VAR.search(str(name)):
                yield where, name, value


def check_workflow(path, found):
    with open(path) as f:
        workflow = yaml.safe_load(f) or {}
    jobs = workflow.get("jobs") or {}
    rel = os.path.relpath(path)
    problems = []
    for job_id, job in jobs.items():
        if not isinstance(job, dict):
            continue
        name = check_name(job_id, job)
        if name not in REQUIRED_CHECKS:
            continue
        found.setdefault(name, []).append(rel)
        where = f"{rel}: job {job_id} (required check '{name}')"
        if "concurrency" in workflow:
            problems.append(f"{where}: the workflow declares concurrency, so a second run on the same head cancels one of the two")
        seen, queue = set(), [job_id]
        while queue:
            current = queue.pop()
            if current in seen:
                continue
            seen.add(current)
            other = jobs.get(current)
            if not isinstance(other, dict):
                continue
            if "concurrency" in other:
                via = "it" if current == job_id else f"job {current}, which it needs,"
                problems.append(f"{where}: {via} declares concurrency, so a second run on the same head cancels one of the two")
            queue.extend(needs_of(other))
        timeout = job.get("timeout-minutes")
        if not isinstance(timeout, (int, float)):
            problems.append(f"{where}: no numeric timeout-minutes, so nothing bounds a wait below the 6-hour limit that cancels the run")
            continue
        for scope, var, value in waits(job):
            try:
                seconds = float(value)
            except (TypeError, ValueError):
                problems.append(f"{where}: {scope} sets {var} to {value!r}, not a number of seconds the timeout can be checked against")
                continue
            if seconds > timeout * 60 - 60:
                problems.append(f"{where}: {scope} sets {var}={value}, not a minute short of timeout-minutes {timeout}, "
                                "so the timeout can cancel the run before it decides")
    return problems


def main(argv):
    if argv:
        paths, discover = argv, False
    else:
        root = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
        paths = sorted(glob.glob(os.path.join(root, "*.yml")) + glob.glob(os.path.join(root, "*.yaml")))
        discover = True
    found, problems = {}, []
    for path in paths:
        problems.extend(check_workflow(path, found))
    if discover:
        for name in REQUIRED_CHECKS:
            if name not in found:
                problems.append(f"required check '{name}' has no job in .github/workflows: rename it in REQUIRED_CHECKS and the branch protection together")
    for problem in problems:
        print(f"FAIL {problem}")
    if problems:
        return 1
    print(f"Required checks never cancel: {len(paths)} workflows checked, "
          f"{sum(len(v) for v in found.values())} required-check jobs.")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
