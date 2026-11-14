#!/usr/bin/env python3
"""The offline API-collection suites run before the merge, from one script.

test-api-collections-offline.sh is the one list of offline node suites. The release's
test-api-integrations.sh runs it first, and api-collections-offline.yml runs it on every
pull request (TXE-6055), so a provider added to StandardProviders without its redaction
row fails the PR, not the release run (TXE-6051). This checks that both callers still run
the script, that the script still runs both suites, that no other workflow script runs a
suite of its own instead, and that the pull-request trigger has no paths filter: a
filtered required check never reports on a PR outside its paths, which blocks its merge.

The job that runs the script, which is the required check, also runs schemasync
(validate-schema-sync.sh). A provider const registered without its config.schema.json enum
entry otherwise passed every PR check and failed the chart release on main (TXE-7064, run
42894713908), so this checks that the step stays in that job. The same holds for check-api-artifacts.sh
(TXE-7296): the CLI catalog, the docs navigation and the OpenAPI invariants drifted on main
while no PR check ran them.

Usage: test-api-collections-offline-callers.py
"""
import glob
import os
import re
import sys

import yaml

WORKFLOWS = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
SCRIPTS = os.path.join(WORKFLOWS, "scripts")
SCRIPT = "test-api-collections-offline.sh"
WORKFLOW = "api-collections-offline.yml"
RELEASE_CALLER = "test-api-integrations.sh"
SCHEMA_SYNC = "validate-schema-sync.sh"
API_ARTIFACTS = "check-api-artifacts.sh"
PYYAML_PIN = ".github/workflows/requirements/pyyaml.txt"
SUITES = ("collections/collection-scripts.test.mjs", "runners/newman-exit-when-done.test.mjs")
REPO = os.path.abspath(os.path.join(WORKFLOWS, "..", ".."))
# The loop that finds every *.test.mjs, *.test.js and *.test.cjs under tests/e2e/api (TXE-7371:
# lib/pricing.test.js ran in no job while it found .mjs only), and its exclusion list.
DISCOVERY = re.compile(r"""find "\$SUITES_DIR" \\\( -name '\*\.test\.mjs' -o -name '\*\.test\.js' -o -name '\*\.test\.cjs' \\\)""")
EXCLUDED = re.compile(r"^OFFLINE_EXCLUDED=\(([^)]*)\)", re.M)
# A direct run of one of the suites, which belongs in SCRIPT alone.
SUITE_RUN = re.compile(r"\bnode\s+\S*tests/e2e/api/\S+\.test\.mjs")


def read(path):
    with open(path, encoding="utf-8") as fh:
        return fh.read()


def main():
    failures = []

    script = read(os.path.join(SCRIPTS, SCRIPT))
    for suite in SUITES:
        if f"tests/e2e/api/{suite}" not in script and f'"$SUITES_DIR/{suite}"' not in script:
            failures.append(f"{SCRIPT} no longer runs {suite}")

    # Every other *.test.mjs under tests/e2e/api runs because the script finds it (TXE-7067):
    # a listed set would miss the next one added, which is how 34 runners/lib tests ran in no
    # job. The discovery must stay, and an exclusion must name an existing test and say why.
    if not DISCOVERY.search(script):
        failures.append(f"{SCRIPT} no longer finds every *.test.mjs, *.test.js and *.test.cjs under tests/e2e/api; list nothing, find them")
    excluded = EXCLUDED.search(script)
    if not excluded:
        failures.append(f"{SCRIPT} has no OFFLINE_EXCLUDED list")
    else:
        for entry in re.findall(r'"([^"]*)"', excluded.group(1)):
            path, _, reason = entry.partition(":")
            if not reason.strip():
                failures.append(f"{SCRIPT} excludes {path} without a reason")
            if not os.path.isfile(os.path.join(REPO, "tests", "e2e", "api", path.strip())):
                failures.append(f"{SCRIPT} excludes {path}, which does not exist")

    if SCRIPT not in read(os.path.join(SCRIPTS, RELEASE_CALLER)):
        failures.append(f"{RELEASE_CALLER} no longer runs {SCRIPT}")
    for path in sorted(glob.glob(os.path.join(SCRIPTS, "*.sh"))):
        if os.path.basename(path) == SCRIPT:
            continue
        if SUITE_RUN.search(read(path)):
            failures.append(f"{os.path.basename(path)} runs an offline suite directly; add it to {SCRIPT}")

    doc = yaml.safe_load(read(os.path.join(WORKFLOWS, WORKFLOW))) or {}
    triggers = doc.get(True, doc.get("on"))  # PyYAML reads a bare `on:` key as True
    triggers = {triggers: None} if isinstance(triggers, str) else dict.fromkeys(triggers) if isinstance(triggers, list) else triggers or {}
    if "pull_request" not in triggers:
        failures.append(f"{WORKFLOW} no longer triggers on pull_request")
    else:
        filters = triggers["pull_request"] or {}
        for key in ("paths", "paths-ignore", "branches", "branches-ignore"):
            if key in filters:
                failures.append(f"{WORKFLOW} filters pull_request on {key}; the suites must run on every PR")
    def runs(job, name):
        return any(name in (step.get("run") or "") for step in (job.get("steps") or []) if isinstance(step, dict))

    suite_jobs = [
        job for job in (doc.get("jobs") or {}).values() if isinstance(job, dict) and runs(job, SCRIPT)
    ]
    if not suite_jobs:
        failures.append(f"no job in {WORKFLOW} runs {SCRIPT}")
    elif not any(runs(job, SCHEMA_SYNC) for job in suite_jobs):
        failures.append(f"the job in {WORKFLOW} that runs {SCRIPT} no longer runs {SCHEMA_SYNC}")
    if suite_jobs and not any(runs(job, API_ARTIFACTS) for job in suite_jobs):
        failures.append(f"the job in {WORKFLOW} that runs {SCRIPT} no longer runs {API_ARTIFACTS}")

    # The suites' Python checks import yaml, which a runner's python3 need not have: every job
    # that runs the suites, directly or through the release caller, installs the pinned PyYAML
    # in an earlier step (TXE-7424 review).
    for path in sorted(glob.glob(os.path.join(WORKFLOWS, "*.yml"))):
        for job_id, job in ((yaml.safe_load(read(path)) or {}).get("jobs") or {}).items():
            steps = [s for s in ((job or {}).get("steps") or []) if isinstance(s, dict)] if isinstance(job, dict) else []
            first = next((i for i, s in enumerate(steps) if SCRIPT in (s.get("run") or "") or RELEASE_CALLER in (s.get("run") or "")), None)
            if first is not None and not any(PYYAML_PIN in (s.get("run") or "") for s in steps[:first]):
                failures.append(f"{os.path.basename(path)} job {job_id} runs the offline suites without installing {PYYAML_PIN} first")

    for f in failures:
        print(f"FAIL {f}")
    print(f"{len(SUITES)} suites, 2 callers checked, {len(failures)} failures")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
