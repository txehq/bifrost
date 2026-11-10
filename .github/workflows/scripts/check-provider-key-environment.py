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
  * a reusable-workflow call passes a provider credential or `secrets: inherit`: a
    calling job cannot name an environment, so the called workflow's job must name
    live-providers and read the secret itself;
  * a job outside live-providers reads secrets dynamically (`secrets[...]`,
    `toJSON(secrets)`), which can reach any credential.

A provider credential is a secret whose name ends in `_API_KEY`, or one of
OTHER_PROVIDER_CREDENTIALS below: the vendor credentials that are not called API keys.
Add a new provider's key as `<PROVIDER>_API_KEY` (AGENTS.md, "Adding a New Provider"
step 6); a vendor credential of another shape goes in that list in the same change.

The repository half, that the keys exist only as environment secrets and that the
environment requires a reviewer, is a setting this check cannot read with a pull
request's token: see AGENTS.md step 6.

Usage: check-provider-key-environment.py [workflow.yml ...]
  With no arguments, checks every workflow in .github/workflows.
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

SECRET_REF = re.compile(r"\bsecrets\.([A-Za-z_][A-Za-z0-9_]*)")
DYNAMIC_SECRETS = re.compile(r"\bsecrets\s*\[|toJSON\(\s*secrets\s*\)", re.IGNORECASE)


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


def credentials(node, path):
    """Yields (path, secret name) for every provider credential read under node."""
    for where, text in strings(node, path):
        for name in SECRET_REF.findall(text):
            if is_provider_credential(name):
                yield where, name


def environment_name(job):
    env = job.get("environment")
    if isinstance(env, dict):
        env = env.get("name")
    return env


def describe(names):
    names = sorted(set(names))
    shown = ", ".join(names[:4])
    return shown + (f" and {len(names) - 4} more" if len(names) > 4 else "")


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

    top = list(credentials(doc.get("env") or {}, "env"))
    if top:
        yield (f"{name}: the workflow-level env reads {describe(n for _, n in top)}, which every "
               f"job inherits; read provider credentials only in jobs that name "
               f"`environment: {ENVIRONMENT}`")

    for job_id, job in (doc.get("jobs") or {}).items():
        if not isinstance(job, dict):
            continue
        where = f"{name}: job {job_id}"

        if "uses" in job:  # a reusable-workflow call: it cannot name an environment
            passed = job.get("secrets")
            if passed == "inherit":
                yield (f"{where}: `secrets: inherit` hands every provider credential to the "
                       f"called workflow; its job must name `environment: {ENVIRONMENT}` "
                       f"and read the secret itself")
            else:
                found = list(credentials(passed or {}, "secrets"))
                if found:
                    yield (f"{where}: passes {describe(n for _, n in found)} to a reusable "
                           f"workflow; a calling job cannot name an environment, so the "
                           f"called workflow's job must name `environment: {ENVIRONMENT}` "
                           f"and read the secret itself")
            continue

        if environment_name(job) == ENVIRONMENT:
            continue

        found = list(credentials(job, "job"))
        if found:
            env_level = sorted({n for p, n in found if p.startswith("job.env.")})
            named = environment_name(job)
            gate = (f"names environment {named!r}" if named is not None
                    else "names no environment")
            refs = checkout_refs(job)
            ref_text = f", checks out {' and '.join(refs)}" if refs else ""
            if env_level:
                yield (f"{where}: job-level env reads {describe(env_level)}{ref_text}, and the "
                       f"job {gate}: name `environment: {ENVIRONMENT}` so a reviewer approves "
                       f"the run before the job gets the keys")
            rest = sorted({n for p, n in found if not p.startswith("job.env.")})
            if rest:
                yield (f"{where}: reads {describe(rest)}{ref_text}, and the job {gate}: name "
                       f"`environment: {ENVIRONMENT}` so a reviewer approves the run before "
                       f"the job gets the keys")
            continue

        for _, text in strings(job, "job"):
            if DYNAMIC_SECRETS.search(text):
                yield (f"{where}: reads secrets dynamically ({text.strip()[:60]!r}), which "
                       f"can reach any provider credential; name the secrets it needs, or "
                       f"name `environment: {ENVIRONMENT}`")
                break


def main():
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    files = sys.argv[1:] or sorted(glob.glob(os.path.join(root, "*.yml")))
    found = [p for f in files for p in problems(f)]
    for p in found:
        print(f"FAIL: {p}", file=sys.stderr)
    print(f"{len(files)} workflows checked, {len(found)} provider-credential problems")
    return 1 if found else 0


if __name__ == "__main__":
    sys.exit(main())
