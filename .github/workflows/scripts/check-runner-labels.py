#!/usr/bin/env python3
"""Fails on a job whose runner the fork does not have.

GitHub queues a job on an unknown label for 24 hours, then cancels it, so the failure
never appears as a failure. The fork has no third-party runners: a job runs on a hosted
label, on the BIFROST_RUNNER_* variables, or carries its own opt-in gate.

Usage: check-runner-labels.py [workflow.yml ...]
  With no arguments, checks every workflow in .github/workflows.
"""
import glob
import os
import re
import sys

import yaml

HOSTED = re.compile(r"^(ubuntu|windows|macos)-[0-9a-z.-]+$")
VARIABLE = re.compile(r"^\$\{\{ vars\.BIFROST_RUNNER_(AMD64|ARM64) \|\| '[a-z0-9.-]+' \}\}$")
GATED = {"codspeed-macro": "vars.BIFROST_CODSPEED_ENABLED == 'true'"}


def problems(path):
    with open(path) as fh:
        doc = yaml.safe_load(fh) or {}
    for name, job in (doc.get("jobs") or {}).items():
        label = job.get("runs-on") if isinstance(job, dict) else None
        if label is None:  # a reusable-workflow job names no runner
            continue
        if not isinstance(label, str):
            yield f"{path}: job {name}: unsupported runs-on {label!r}"
        elif label in GATED:
            if job.get("if") != GATED[label]:
                yield f"{path}: job {name}: {label} needs `if: {GATED[label]}`"
        elif "${{" in label:
            if not VARIABLE.match(label) and "matrix." not in label:
                yield f"{path}: job {name}: unknown runner expression {label}"
        elif not HOSTED.match(label):
            yield f"{path}: job {name}: {label} is not a runner the fork has"


def main():
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    files = sys.argv[1:] or sorted(glob.glob(os.path.join(root, "*.yml")))
    found = [p for f in files for p in problems(f)]
    for p in found:
        print(f"FAIL: {p}", file=sys.stderr)
    print(f"{len(files)} workflows checked, {len(found)} problems")
    return 1 if found else 0


if __name__ == "__main__":
    sys.exit(main())
