#!/usr/bin/env python3
"""Fails on a provider credential that reaches a job outside the live-providers environment.

A job runs the code of the ref that triggered it: workflow_dispatch runs the workflow file
and checkout of the branch the caller picks, push runs the pushed branch, and a job's
`if:` or a `ref:` pinned in the file is part of that same branch, so its author can
change it. Whoever can push a branch can therefore run any code with any repository
secret. The trigger, the checkout ref and an authorize job cannot keep a key from a
writer; only an environment whose protection rules the writer cannot change can
(TXE-6975, the Codex security finding on PR #120 about run-core-tests.yml).

So provider credentials are secrets of the live-providers environment, never repository
or organization secrets, and every job that reads one names that environment by its
literal name. A reviewer approves each run before the job gets a runner and the
environment's secrets. This check enforces the workflow half of that. It fails when:

  * a job reads a provider credential and does not name `environment: live-providers`
    (a literal, not an expression, so no event can route the job to an unprotected
    environment); this includes the original finding, a key in the job-level env of a
    job that checks out a caller-chosen ref;
  * the workflow-level `env:` reads a provider credential, which hands it to every job;
  * a reusable-workflow call passes a provider credential, in `secrets:` or `with:`, or
    uses `secrets: inherit`: a calling job cannot name an environment, so the called
    workflow's job must name live-providers and read the secret itself.

A read is any use of the secrets context inside `${{ }}`, the only place it exists, in
every scope above. `secrets.NAME` and `secrets['NAME']` read the secret they name. Any
other use (`toJSON(secrets)`, `secrets[expr]`, `secrets.*`) can reach every secret, so it
counts as reading a provider credential.

A provider credential is a secret whose name ends in `_API_KEY`, or one of
OTHER_PROVIDER_CREDENTIALS below: the vendor credentials that are not called API keys.
Add a new provider's key as `<PROVIDER>_API_KEY` (AGENTS.md, "Adding a New Provider"
step 6); a vendor credential of another shape goes in that list in the same change.

The repository half, that the keys exist only as environment secrets and that the
environment requires a reviewer, is a setting this check cannot read with a pull
request's token: see AGENTS.md step 6.

Usage: check-provider-key-environment.py [workflow.yml ...]
  With no arguments, checks every workflow (*.yml and *.yaml) in .github/workflows.
"""
import glob
import os
import re
import sys

import yaml

ENVIRONMENT = "live-providers"

# Vendor credentials in the provider test jobs whose names do not end in _API_KEY.
OTHER_PROVIDER_CREDENTIALS = {
    "AWS_ACCESS_KEY_ID",  # Bedrock
    "AWS_SECRET_ACCESS_KEY",
    "AWS_SESSION_TOKEN",
    "AZURE_CLIENT_SECRET",  # Azure OpenAI service principal
    "CLOUDFLARE_API_TOKEN",  # Workers AI
    "DATABRICKS_TOKEN",
    "GITHUB_COPILOT_PRIVATE_KEY",  # the Copilot provider's GitHub App key
    "VERTEX_CREDENTIALS",  # Vertex service-account JSON
}

# Every use of the secrets context: `secrets.NAME` and `secrets['NAME']` name one secret;
# anything else (`toJSON(secrets)`, `secrets[x]`, `secrets.*`) can reach any secret.
SECRETS_USE = re.compile(
    r"(?<![\w.])secrets\b"
    r"(?:\s*\.\s*(?P<dotted>[A-Za-z_][A-Za-z0-9_-]*)"
    r"|\s*\[\s*(?P<q>['\"])(?P<quoted>[A-Za-z_][A-Za-z0-9_-]*)(?P=q)\s*\])?",
    re.IGNORECASE,
)


def is_provider_credential(name):
    upper = name.upper()
    return upper.endswith("_API_KEY") or upper in OTHER_PROVIDER_CREDENTIALS


def strings(node, path):
    """Yields (path, string) for every string value under node, keys included."""
    if isinstance(node, dict):
        for key, value in node.items():
            sub = f"{path}.{key}"
            if isinstance(key, str):
                yield sub, key
            yield from strings(value, sub)
    elif isinstance(node, list):
        for i, value in enumerate(node):
            yield from strings(value, f"{path}[{i}]")
    elif isinstance(node, str):
        yield path, node


def expressions(text):
    """Yields the body of each `${{ ... }}` in text, the only place the secrets context
    exists. A `}}` inside a quoted string does not end the expression: GitHub strings are
    single-quoted, with '' for a quote. An expression left open runs to the end of text."""
    start = text.find("${{")
    while start != -1:
        i, quoted = start + 3, False
        while i < len(text):
            if text[i] == "'":
                if quoted and text.startswith("''", i):
                    i += 2
                    continue
                quoted = not quoted
            elif not quoted and text.startswith("}}", i):
                break
            i += 1
        yield text[start + 3:i]
        start = text.find("${{", i + 2)


def string_spans(expression):
    """Returns the (start, end) of each single-quoted string literal in expression."""
    spans, i = [], 0
    while i < len(expression):
        if expression[i] == "'":
            start, i = i, i + 1
            while i < len(expression):
                if expression.startswith("''", i):
                    i += 2
                elif expression[i] == "'":
                    break
                else:
                    i += 1
            spans.append((start, i + 1))
        i += 1
    return spans


def secret_reads(node, path):
    """Yields (path, name) for each provider credential read under node, and
    (path, None) for each dynamic read of the secrets context, which can reach any."""
    for where, text in strings(node, path):
        for expression in expressions(text):
            literals = string_spans(expression)
            for use in SECRETS_USE.finditer(expression):
                if any(start <= use.start() < end for start, end in literals):
                    continue  # the word inside a string, not the secrets context
                name = use.group("dotted") or use.group("quoted")
                if name is None:
                    yield where, None
                elif is_provider_credential(name):
                    yield where, name


def summarize(reads):
    """Describes reads: the credentials named, then dynamic access if any."""
    names = sorted({n for _, n in reads if n is not None})
    parts = []
    if names:
        shown = ", ".join(names[:4])
        parts.append(shown + (f" and {len(names) - 4} more" if len(names) > 4 else ""))
    if any(n is None for _, n in reads):
        parts.append("secrets dynamically (secrets[...], toJSON(secrets)), which can reach any provider credential")
    return " and ".join(parts)


def environment_name(job):
    env = job.get("environment")
    if isinstance(env, dict):
        env = env.get("name")
    return env


def checkout_refs(job):
    refs = []
    for step in job.get("steps") or []:
        if isinstance(step, dict) and str(step.get("uses", "")).startswith("actions/checkout"):
            refs.append(str((step.get("with") or {}).get("ref", "the triggering ref")))
    return refs


def problems(path):
    with open(path) as fh:
        doc = yaml.safe_load(fh) or {}
    name = os.path.basename(path)

    top = list(secret_reads(doc.get("env") or {}, "env"))
    if top:
        yield (f"{name}: the workflow-level env reads {summarize(top)}, which every job "
               f"inherits; read provider credentials only in jobs that name "
               f"`environment: {ENVIRONMENT}`")

    for job_id, job in (doc.get("jobs") or {}).items():
        if not isinstance(job, dict):
            continue
        where = f"{name}: job {job_id}"

        if "uses" in job:  # a reusable-workflow call: it cannot name an environment
            if job.get("secrets") == "inherit":
                yield (f"{where}: `secrets: inherit` hands every provider credential to the "
                       f"called workflow; its job must name `environment: {ENVIRONMENT}` "
                       f"and read the secret itself")
            found = list(secret_reads({k: v for k, v in job.items() if k != "uses"}, "call"))
            if found:
                yield (f"{where}: passes {summarize(found)} to a reusable workflow; a calling "
                       f"job cannot name an environment, so the called workflow's job must "
                       f"name `environment: {ENVIRONMENT}` and read the secret itself")
            continue

        if environment_name(job) == ENVIRONMENT:
            continue

        found = list(secret_reads(job, "job"))
        if not found:
            continue
        named = environment_name(job)
        gate = f"names environment {named!r}" if named is not None else "names no environment"
        refs = checkout_refs(job)
        ref_text = f", checks out {' and '.join(refs)}" if refs else ""
        job_env = [r for r in found if r[0].startswith("job.env.")]
        rest = [r for r in found if not r[0].startswith("job.env.")]
        if job_env:
            yield (f"{where}: job-level env reads {summarize(job_env)}{ref_text}, and the job "
                   f"{gate}: name `environment: {ENVIRONMENT}` so a reviewer approves the run "
                   f"before the job gets the keys")
        if rest:
            yield (f"{where}: reads {summarize(rest)}{ref_text}, and the job {gate}: name "
                   f"`environment: {ENVIRONMENT}` so a reviewer approves the run before the "
                   f"job gets the keys")


def main():
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    files = sys.argv[1:] or sorted(
        glob.glob(os.path.join(root, "*.yml")) + glob.glob(os.path.join(root, "*.yaml"))
    )
    found = [p for f in files for p in problems(f)]
    for p in found:
        print(f"FAIL: {p}", file=sys.stderr)
    print(f"{len(files)} workflows checked, {len(found)} provider-credential problems")
    return 1 if found else 0


if __name__ == "__main__":
    sys.exit(main())
