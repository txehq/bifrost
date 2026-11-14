#!/usr/bin/env python3
"""Fails when a release-blocking job that needs no vendor key has no required PR check (TXE-7424).

#292 changed the error type a live session returns. Every required PR check passed, and the
regression surfaced only in the sync.10 Release Pipeline's test-live job (tests/live against
the fake upstream: no vendor key, no vendor call). That job stopped the release, because
bifrost-http-prep runs only when test-live succeeded or was skipped. A test that needs no
vendor key can run on the pull request, so a release should never be the first place it fails.

The rule. A job of release-pipeline.yml that a publishing job (PUBLISHING_JOBS) needs directly
is a release gate: its failure stops the release. A gate is keyless when every secret it reads
is in NON_VENDOR_SECRETS (a GitHub token, the coverage upload token, the test encryption key,
the R2 report bucket). Every keyless gate must be in exactly one of:

  COUNTERPARTS  the pull-request jobs that run what the gate runs. Each entry names the gate's
                command (`release`) and the PR job's command (`pr`); both must still appear in
                the respective job's run steps, or in a .github/workflows/scripts file a step
                calls, so a renamed script cannot leave a counterpart that runs something else.
                The PR job's workflow must run on pull_request, and the job must be a required
                check (REQUIRED_CHECKS of check-required-checks-never-cancel.py, the branch
                protection) or a job that one of them needs and whose result it reads.
  NOT_MOVED     gates that do not run on pull requests, each with the reason or the ticket
                that moves it.

A gate that reads a vendor secret is out of scope: a PR from a fork never gets the keys. An
entry for a job that is not a keyless gate fails too, so neither list can go stale.

Usage: check-release-gates-run-on-prs.py [--self-test]
"""
import importlib.util
import os
import re
import sys

import yaml

WORKFLOWS = os.path.normpath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
RELEASE_WORKFLOW = "release-pipeline.yml"

# The jobs that publish something. A job one of them needs directly can stop a release.
PUBLISHING_JOBS = ("core-release", "framework-release", "plugins-release", "bifrost-http-prep")

# Secrets that are not a vendor credential. A gate that reads any other secret needs live keys.
NON_VENDOR_SECRETS = {"GITHUB_TOKEN", "GH_TOKEN", "CODECOV_TOKEN", "BIFROST_ENCRYPTION_KEY"}
NON_VENDOR_SECRET_PREFIXES = ("R2_",)

# gate -> the pull-request jobs that run what it runs.
COUNTERPARTS = {
    "test-live": [
        {"workflow": "go-unit-tests.yml", "job": "live-fake", "release": "test-live.sh fake", "pr": "test-live.sh fake"},
    ],
    "test-agentgateway": [
        {"workflow": "go-unit-tests.yml", "job": "agentgateway", "release": "test-agentgateway-e2e.sh", "pr": "test-agentgateway-e2e.sh"},
    ],
    # go-unit-shards.py runs every package with tests in core, framework, transports and the
    # plugins, found with `go list` (its EXCLUDED list holds only live-key suites).
    "test-core-unit": [
        {"workflow": "go-unit-tests.yml", "job": "shard", "release": "go list", "pr": "go-unit-shards.py run"},
    ],
    # make test-memory runs the _AllocationScaling and _Retention tests of core and transports,
    # ordinary tests of packages the shards run.
    "test-memory": [
        {"workflow": "go-unit-tests.yml", "job": "shard", "release": "make test-memory", "pr": "go-unit-shards.py run"},
    ],
    # The transports unit tests run in the shards (-short, as here; the release adds -race).
    # The Helm/config schema check the script starts with runs in the API collection suites.
    "test-bifrost-http": [
        {"workflow": "go-unit-tests.yml", "job": "shard", "release": "test-bifrost-http.sh", "pr": "go-unit-shards.py run"},
        {"workflow": "api-collections-offline.yml", "job": "api-collections-offline", "release": "test-bifrost-http.sh", "pr": "validate-helm-schema.sh"},
    ],
    "test-migrations": [
        {"workflow": "go-unit-tests.yml", "job": "migrations", "release": "run-migration-tests.sh postgres", "pr": "run-migration-tests.sh postgres"},
        {"workflow": "go-unit-tests.yml", "job": "migrations", "release": "run-migration-tests.sh sqlite", "pr": "run-migration-tests.sh sqlite"},
    ],
}

# Keyless gates that do not run on pull requests, and why.
NOT_MOVED = {
    "check-skip": "pipeline plumbing: reads the commit message's skip flags, tests nothing",
    "detect-changes": "pipeline plumbing: decides which modules release, tests nothing",
    "resolve-cli-versions": "not a test: picks the CLI versions test-cli-harness runs, which needs vendor keys",
    "approve-flaky-test-core": "a manual approval, run only after test-core (vendor keys) failed",
    "approve-memory-regression": "a manual approval, run only after test-memory failed",
    "test-cost-accuracy": "TXE-7452: builds its mocker and hitter from an unpinned checkout of maximhq/bifrost-benchmarking; "
    "pin it to a commit before PR code runs it",
}

SECRET = re.compile(r"secrets\.([A-Za-z0-9_]+)")
SCRIPT_CALL = re.compile(r"\.github/workflows/scripts/([A-Za-z0-9_.-]+)")


def needs_of(job):
    needs = (job or {}).get("needs") or []
    return [needs] if isinstance(needs, str) else list(needs)


def check_name(job_id, job):
    name = job.get("name")
    return name if isinstance(name, str) and name else job_id


def is_vendor_secret(name):
    return name not in NON_VENDOR_SECRETS and not name.startswith(NON_VENDOR_SECRET_PREFIXES)


def run_text(job, scripts):
    """The job's run steps, and the text of every workflow script a step calls."""
    runs = "\n".join(str(step.get("run") or "") for step in (job.get("steps") or []) if isinstance(step, dict))
    called = [scripts[name] for name in SCRIPT_CALL.findall(runs) if name in scripts]
    return "\n".join([runs, *called])


def triggers(workflow):
    on = workflow.get("on", workflow.get(True))
    if isinstance(on, str):
        return {on}
    if isinstance(on, list):
        return set(on)
    return set(on or {})


def problems(release, workflows, scripts, counterparts, not_moved, required):
    """release: the parsed release workflow. workflows: file name -> parsed workflow.
    scripts: script name -> text. Returns one line per defect; empty passes."""
    out = []
    jobs = release.get("jobs") or {}
    gates = set()
    for publisher in PUBLISHING_JOBS:
        if publisher not in jobs:
            out.append(f"{RELEASE_WORKFLOW}: publishing job {publisher} is gone; update PUBLISHING_JOBS")
            continue
        gates.update(n for n in needs_of(jobs[publisher]) if n not in PUBLISHING_JOBS)
    keyless = set()
    for gate in sorted(gates):
        job = jobs.get(gate)
        if not isinstance(job, dict):
            out.append(f"{RELEASE_WORKFLOW}: a publishing job needs {gate}, which is not a job")
            continue
        if not any(is_vendor_secret(s) for s in SECRET.findall(yaml.safe_dump(job))):
            keyless.add(gate)
    for gate in sorted(keyless):
        listed = [name for name, table in (("COUNTERPARTS", counterparts), ("NOT_MOVED", not_moved)) if gate in table]
        if not listed:
            out.append(f"{RELEASE_WORKFLOW}: job {gate} can stop a release and reads no vendor key, but no required PR check runs "
                       "what it runs: add a PR job and list it in COUNTERPARTS, or list the gate in NOT_MOVED with its reason")
        elif len(listed) > 1:
            out.append(f"{gate} is in both COUNTERPARTS and NOT_MOVED")
    for table_name, table in (("COUNTERPARTS", counterparts), ("NOT_MOVED", not_moved)):
        for gate in sorted(set(table) - keyless):
            why = "is not a job a publishing job needs" if gate not in gates else "reads a vendor key"
            out.append(f"{table_name} lists {gate}, which {why}: remove the entry")

    for gate, entries in sorted(counterparts.items()):
        if gate not in keyless:
            continue
        gate_text = run_text(jobs[gate], scripts)
        for entry in entries:
            where = f"COUNTERPARTS[{gate}] -> {entry['workflow']}:{entry['job']}"
            if entry["release"] not in gate_text:
                out.append(f"{where}: release job {gate} no longer runs '{entry['release']}'; update the entry")
            workflow = workflows.get(entry["workflow"])
            if workflow is None:
                out.append(f"{where}: no such workflow")
                continue
            if "pull_request" not in triggers(workflow):
                out.append(f"{where}: the workflow does not run on pull_request")
            pr_jobs = workflow.get("jobs") or {}
            pr_job = pr_jobs.get(entry["job"])
            if not isinstance(pr_job, dict):
                out.append(f"{where}: no such job")
                continue
            if entry["pr"] not in run_text(pr_job, scripts):
                out.append(f"{where}: the job no longer runs '{entry['pr']}'; update the entry")
            if any(is_vendor_secret(s) for s in SECRET.findall(yaml.safe_dump(pr_job))):
                out.append(f"{where}: the PR job reads a vendor secret, so a fork's PR cannot run it")
            if check_name(entry["job"], pr_job) in required:
                continue
            readers = [
                job_id
                for job_id, job in pr_jobs.items()
                if isinstance(job, dict)
                and check_name(job_id, job) in required
                and entry["job"] in needs_of(job)
                and f"needs.{entry['job']}.result" in yaml.safe_dump(job)
            ]
            if not readers:
                out.append(f"{where}: the job is not a required check, and no required check in its workflow needs it and "
                           f"reads needs.{entry['job']}.result")
    return out


def load_required():
    path = os.path.join(WORKFLOWS, "scripts", "check-required-checks-never-cancel.py")
    spec = importlib.util.spec_from_file_location("required_checks", path)
    assert spec is not None and spec.loader is not None, path
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return set(module.REQUIRED_CHECKS)


def load_repo():
    workflows = {}
    for name in sorted(os.listdir(WORKFLOWS)):
        if name.endswith((".yml", ".yaml")):
            with open(os.path.join(WORKFLOWS, name)) as f:
                workflows[name] = yaml.safe_load(f) or {}
    scripts = {}
    scripts_dir = os.path.join(WORKFLOWS, "scripts")
    for name in os.listdir(scripts_dir):
        path = os.path.join(scripts_dir, name)
        if os.path.isfile(path):
            with open(path, errors="replace") as f:
                scripts[name] = f.read()
    return workflows, scripts


def self_test():
    release = yaml.safe_load(
        """
jobs:
  build: {steps: [{run: go build}]}
  test-keyless: {needs: [build], steps: [{run: ./.github/workflows/scripts/run-it.sh fast}]}
  test-keyed: {env: {K: "${{ secrets.OPENAI_API_KEY }}"}, steps: [{run: make keyed}]}
  test-token: {env: {T: "${{ secrets.GITHUB_TOKEN }}", R: "${{ secrets.R2_BUCKET }}"}, steps: [{run: make token}]}
  core-release: {needs: [test-keyless, test-keyed, test-token], steps: [{run: release}]}
  framework-release: {needs: [core-release], steps: [{run: release}]}
  plugins-release: {needs: [core-release], steps: [{run: release}]}
  bifrost-http-prep: {needs: [core-release], steps: [{run: release}]}
"""
    )
    pr = yaml.safe_load(
        """
on: {pull_request: {}}
jobs:
  fast: {steps: [{run: ./.github/workflows/scripts/run-it.sh fast}]}
  token: {steps: [{run: make token}]}
  aggregate: {name: Required, needs: [fast, token], steps: [{run: 'test "${{ needs.fast.result }}" = success'}]}
"""
    )
    scripts = {"run-it.sh": "echo running"}
    required = {"Required"}
    good_cp = {"test-keyless": [{"workflow": "pr.yml", "job": "fast", "release": "run-it.sh fast", "pr": "run-it.sh fast"}]}
    good_nm = {"test-token": "reason"}
    cases = [
        ("passes", good_cp, good_nm, {"pr.yml": pr}, []),
        ("unlisted keyless gate", {}, good_nm, {"pr.yml": pr}, ["job test-keyless can stop a release"]),
        ("stale NOT_MOVED", good_cp, {**good_nm, "gone": "x"}, {"pr.yml": pr}, ["NOT_MOVED lists gone"]),
        ("keyed gate listed", good_cp, {**good_nm, "test-keyed": "x"}, {"pr.yml": pr}, ["NOT_MOVED lists test-keyed, which reads a vendor key"]),
        ("listed twice", good_cp, {**good_nm, "test-keyless": "x"}, {"pr.yml": pr}, ["in both COUNTERPARTS and NOT_MOVED"]),
        ("PR command renamed", {"test-keyless": [{**good_cp["test-keyless"][0], "pr": "other.sh"}]}, good_nm, {"pr.yml": pr},
         ["no longer runs 'other.sh'"]),
        ("release command renamed", {"test-keyless": [{**good_cp["test-keyless"][0], "release": "other.sh"}]}, good_nm, {"pr.yml": pr},
         ["release job test-keyless no longer runs 'other.sh'"]),
        ("no such PR job", {"test-keyless": [{**good_cp["test-keyless"][0], "job": "nope"}]}, good_nm, {"pr.yml": pr}, ["no such job"]),
        ("not required", {"test-keyless": [{**good_cp["test-keyless"][0], "job": "token", "pr": "make token"}]}, good_nm, {"pr.yml": pr},
         ["no required check in its workflow needs it and reads needs.token.result"]),
        ("not on pull_request", good_cp, good_nm, {"pr.yml": {**pr, "on": {"push": {}}}}, ["does not run on pull_request"]),
    ]
    failed = 0
    for label, cps, nms, workflows, expect in cases:
        got = problems(release, workflows, scripts, cps, nms, required)
        ok = (not expect and not got) or (expect and all(any(e in g for g in got) for e in expect))
        if not ok:
            failed += 1
            print(f"FAIL self-test '{label}': expected {expect or 'no problems'}, got {got}")
    if failed:
        return 1
    print(f"check-release-gates-run-on-prs self-test: {len(cases)} cases passed")
    return 0


def main(argv):
    if argv == ["--self-test"]:
        return self_test()
    if argv:
        print(__doc__)
        return 2
    workflows, scripts = load_repo()
    found = problems(workflows[RELEASE_WORKFLOW], workflows, scripts, COUNTERPARTS, NOT_MOVED, load_required())
    for problem in found:
        print(f"FAIL {problem}")
    if found:
        return 1
    print(f"Release gates run on PRs: {len(COUNTERPARTS)} keyless gates have a required PR counterpart, "
          f"{len(NOT_MOVED)} are recorded in NOT_MOVED.")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
