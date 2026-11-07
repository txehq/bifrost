#!/usr/bin/env python3
"""The offline API-collection suites run before the merge, from one script.

test-api-collections-offline.sh is the one list of offline node suites. The release's
test-api-integrations.sh runs it first, and api-collections-offline.yml runs it on every
pull request (TXE-6055), so a provider added to StandardProviders without its redaction
row fails the PR, not the release run (TXE-6051). This checks that both callers still run
the script, that the script still runs both suites, that no other workflow script runs a
suite of its own instead, and that the pull-request trigger has no paths filter: a
filtered required check never reports on a PR outside its paths, which blocks its merge.

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
SUITES = ("collections/collection-scripts.test.mjs", "runners/newman-exit-when-done.test.mjs")
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
    runs_script = any(
        SCRIPT in (step.get("run") or "")
        for job in (doc.get("jobs") or {}).values() if isinstance(job, dict)
        for step in (job.get("steps") or []) if isinstance(step, dict)
    )
    if not runs_script:
        failures.append(f"no job in {WORKFLOW} runs {SCRIPT}")

    for f in failures:
        print(f"FAIL {f}")
    print(f"{len(SUITES)} suites, 2 callers checked, {len(failures)} failures")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
