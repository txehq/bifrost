#!/usr/bin/env python3
"""Counts the provider keys this step received, and says so when there are none (TXE-8129).

The live-providers environment can hold no provider key at all: on 22 Nov 2026 Connor
decided not to add any for now. A job then runs keyless: the provider suites skip and the
job still goes green, having tested only startup. The canary step proves the environment's
secrets reach the job, not that any provider key is among them.

Every job that names `environment: live-providers` runs this script as the first command of
the step that holds its provider keys, so it counts the keys that step's suites actually
get. It reads its own environment, takes the names that check-provider-key-environment.py
treats as provider credentials (the canary aside), and prints how many are set and which.
A credential set to one of the checker's REGISTRATION_ONLY_PLACEHOLDERS (BFL_API_KEY's
"registration-only-no-upstream-call") is named as such and not counted: it calls nothing.
It prints names and counts of names only, never a value or anything derived from one, such
as its length.

  * No key set: a `::warning::` naming the job, and the step-summary line
    "keyless: provider suites skipped". It does not fail: running keyless is the current
    decision.
  * Keys set: the count and the names, in the log and the step summary. When keys are
    added, this is the evidence they arrived.

check-provider-key-environment.py fails a live-providers job that does not run this
script first in a step that sees every provider credential the job reads.

Usage: count-provider-keys.py   (always exits 0 once it has counted)
"""
import importlib.util
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))


def load_checker():
    spec = importlib.util.spec_from_file_location(
        "check_provider_key_environment", os.path.join(HERE, "check-provider-key-environment.py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def count(environ, is_provider_credential, canary, placeholders):
    """Returns (present, configured, placeholder): sorted names of the provider credentials in
    environ. present: set to a real value. configured: all of them. placeholder: set to a
    registration-only placeholder, which lets a provider register and calls nothing, so it is
    not a key. The canary is not a provider key."""
    names = sorted(n for n in environ if is_provider_credential(n) and n.upper() != canary)
    placeholder = [n for n in names if environ[n].strip() in placeholders]
    present = [n for n in names if environ[n].strip() and n not in placeholder]
    return present, names, placeholder


def report(environ, present, configured, placeholder):
    """Returns (log lines, step-summary lines) for the counts. Names and counts of names
    only: never a value, nor anything derived from one such as its length."""
    job = environ.get("GITHUB_JOB", "this job")
    workflow = environ.get("GITHUB_WORKFLOW", "")
    where = f"{workflow} / {job}" if workflow else job
    note = (f"; {', '.join(placeholder)} set to a registration-only placeholder, not counted"
            if placeholder else "")
    if present:
        line = f"{len(present)} of {len(configured)} provider keys set: {', '.join(present)}{note}"
        return [f"{where}: {line}"], [f"- `{job}`: {line}"]
    log = [
        f"::warning title=Keyless live-providers job::{where}: none of the {len(configured)} "
        f"provider keys this step reads is set{note}, so the provider suites skip and the job "
        f"tests startup only (TXE-8129)"
    ]
    return log, [f"- `{job}`: keyless: provider suites skipped (0 of {len(configured)} provider keys set{note})"]


def main():
    checker = load_checker()
    present, configured, placeholder = count(
        os.environ, checker.is_provider_credential, checker.CANARY_SECRET,
        checker.REGISTRATION_ONLY_PLACEHOLDERS)
    log, summary = report(os.environ, present, configured, placeholder)
    for line in log:
        print(line)
    path = os.environ.get("GITHUB_STEP_SUMMARY")
    if path:
        with open(path, "a") as fh:
            fh.write("\n".join(summary) + "\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
