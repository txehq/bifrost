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
  * a reusable-workflow call uses `secrets: inherit`, or passes a provider credential in
    `with:` or in any other form than the one below: a calling job cannot name an
    environment, so the called workflow's job must name live-providers and read the secret
    itself.

A called workflow's job sees only the secrets its workflow declares in
`on.workflow_call.secrets` and its caller passes, even when the job names an environment
that holds the secret (TXE-7574, measured with a scratch environment on 2026-11-22: an
environment secret the caller did not pass read as empty, declared or not). When the caller
passes `NAME: ${{ secrets.NAME }}`, the caller's own value is empty, because the secret
exists only in the environment, and the called job that names the environment reads the
environment's value. A non-empty value from the caller wins over the environment's, so the
caller must pass exactly that form and nothing else. So the check also fails when:

  * a job in a called workflow names `environment: live-providers` and reads a secret that
    `on.workflow_call.secrets` does not declare, or reads secrets dynamically (no caller
    can pass those by name);
  * a caller of a workflow in this repository does not pass every secret that workflow's
    live-providers jobs read as `NAME: ${{ secrets.NAME }}`;
  * a caller passes a provider credential that the called workflow's live-providers jobs
    do not read, or passes one to a workflow this check cannot read;
  * a caller passes provider credentials without needing a job whose
    `environment-only-secrets` step covers them all. The value a caller passes is its own:
    empty while the secret exists only in the environment, but a repository or organization
    secret of that name would be passed instead, and a called job, with or without the
    environment, would read it with no approval (measured 2026-11-22). That step runs in a
    job that names no environment, so it reads exactly those secrets, and fails the run if
    any is non-empty. It is the one step outside live-providers that may read a provider
    credential, and its shape is pinned (see guard()).

A job in a called workflow that names no environment and reads a provider credential fails
under the first rule above, as in any other workflow.

A read is any use of the secrets context inside `${{ }}`, the only place it exists, in
every scope above. `secrets.NAME` and `secrets['NAME']` read the secret they name. Any
other use (`toJSON(secrets)`, `secrets[expr]`, `secrets.*`) can reach every secret, so it
counts as reading a provider credential.

A provider credential is a secret whose name ends in `_API_KEY`, or one of
OTHER_PROVIDER_CREDENTIALS below: the vendor credentials that are not called API keys.
Add a new provider's key as `<PROVIDER>_API_KEY` (AGENTS.md, "Adding a New Provider"
step 6); a vendor credential of another shape goes in that list in the same change.

It also fails when a job that names `environment: live-providers` does not start with the
canary step (TXE-7574). The step reads the environment secret LIVE_PROVIDERS_CANARY and
fails the job when it is empty. Only steps that harden the runner may come before it.
A job in a called workflow gets the environment's secrets only if GitHub hands them to
it. The canary shows they arrived on every run, before any provider key is used, and
without the keys being present.

It also fails when a job that names `environment: live-providers` does not count its
provider keys (TXE-8129). The canary shows the environment's secrets arrived, not that a
provider key is among them; with none, the provider suites skip and the job still passes.
So the step that holds the keys runs count-provider-keys.py as its first command, which
warns and writes "keyless: provider suites skipped" to the step summary when none is set.
The steps that run it must between them see every provider credential the job reads, in
their own env or the job's.

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

ENVIRONMENT = "live-providers"


def load_yaml(path):
    # Imported here, not at the top: count-provider-keys.py loads this module on the
    # live-providers runners for is_provider_credential alone, and needs no PyYAML.
    import yaml

    with open(path) as fh:
        return yaml.safe_load(fh) or {}

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
    # Not a vendor credential: the live-providers canary (TXE-7574). It follows the same
    # rules, so it is read only in jobs that name the environment, and a caller passes it
    # only as `LIVE_PROVIDERS_CANARY: ${{ secrets.LIVE_PROVIDERS_CANARY }}`. A literal or a
    # differently named secret would win over the environment's value, and the canary would
    # then prove nothing about how environment secrets reach those jobs.
    "LIVE_PROVIDERS_CANARY",
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


def all_secret_reads(node, path):
    """Yields (path, name) for each secret read under node, and (path, None) for each
    dynamic read of the secrets context, which can reach any."""
    for where, text in strings(node, path):
        for expression in expressions(text):
            literals = string_spans(expression)
            for use in SECRETS_USE.finditer(expression):
                if any(start <= use.start() < end for start, end in literals):
                    continue  # the word inside a string, not the secrets context
                yield where, use.group("dotted") or use.group("quoted")


def secret_reads(node, path):
    """Yields (path, name) for each provider credential read under node, and
    (path, None) for each dynamic read of the secrets context, which can reach any."""
    for where, name in all_secret_reads(node, path):
        if name is None or is_provider_credential(name):
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


CANARY_SECRET = "LIVE_PROVIDERS_CANARY"
CANARY_STEP_ID = "live-providers-canary"
CANARY_ENV_VALUE = "${{ secrets.%s }}" % CANARY_SECRET


def is_runner_hardening(step):
    return isinstance(step, dict) and str(step.get("uses", "")).startswith("step-security/harden-runner@")


def canary_problem(job):
    """Describes what is wrong with a live-providers job's canary step, or returns None."""
    steps = [s for s in job.get("steps") or []]
    for i, step in enumerate(steps):
        if isinstance(step, dict) and step.get("id") == CANARY_STEP_ID:
            break
    else:
        return f"has no step with id `{CANARY_STEP_ID}`"
    if not all(is_runner_hardening(s) for s in steps[:i]):
        return f"runs a step other than the runner hardening before `{CANARY_STEP_ID}`"
    if (step.get("env") or {}).get(CANARY_SECRET) != CANARY_ENV_VALUE:
        return f"`{CANARY_STEP_ID}` does not set env {CANARY_SECRET}: {CANARY_ENV_VALUE}"
    run = str(step.get("run", ""))
    if f'-z "${CANARY_SECRET}"' not in run or "exit 1" not in run:
        return f'`{CANARY_STEP_ID}` does not fail (`exit 1`) when "${CANARY_SECRET}" is empty (`-z`)'
    for key in ("if", "continue-on-error", "uses"):
        if key in step:
            return f"`{CANARY_STEP_ID}` sets `{key}`, so it can pass without the secret"
    return None


KEY_COUNT_COMMAND = 'python3 "$GITHUB_WORKSPACE/.github/workflows/scripts/count-provider-keys.py"'


def first_command(run):
    for line in str(run).splitlines():
        line = line.strip()
        if line and not line.startswith("#"):
            return line
    return None


def key_count_problem(job):
    """Describes what is wrong with a live-providers job's provider-key count, or returns
    None (TXE-8129). The count runs as the first command of a step, so it counts what that
    step's suites get, and the steps that run it must between them see every provider
    credential the job reads (their own env or the job's)."""
    counting = [s for s in job.get("steps") or []
                if isinstance(s, dict) and first_command(s.get("run", "")) == KEY_COUNT_COMMAND]
    if not counting:
        named = any(isinstance(s, dict) and "count-provider-keys.py" in str(s.get("run", ""))
                    for s in job.get("steps") or [])
        if named:
            return f"runs count-provider-keys.py, but not as `{KEY_COUNT_COMMAND}` first in its step"
        return f"has no step that runs `{KEY_COUNT_COMMAND}` as its first command"
    job_env = {str(k).upper() for k in (job.get("env") or {})}
    seen = set(job_env)
    for step in counting:
        seen |= {str(k).upper() for k in (step.get("env") or {})}
    read = {n.upper() for _, n in secret_reads(job, "job") if n is not None} - {CANARY_SECRET}
    unseen = read - seen
    if unseen:
        return (f"reads {listed(unseen)} in a step where count-provider-keys.py does not "
                f"run, so the count would not show whether those keys arrived")
    return None


GUARD_STEP_ID = "environment-only-secrets"
GUARD_LIST = re.compile(r"for name in\s+(.*?);\s*do\b", re.DOTALL)


def guard(job):
    """Returns (step, names, problem) for a job's `environment-only-secrets` step, or
    (None, None, None) when it has none. The step is the one place outside live-providers
    that may read provider credentials. Its job names no environment, so it reads exactly
    the repository and organization secrets of those names, which are the values a caller
    passes with `NAME: ${{ secrets.NAME }}`, and it fails when any is non-empty. It must come
    first after the runner hardening, read each name as itself, loop over exactly those
    names, and fail without a way to pass."""
    steps = list(job.get("steps") or [])
    for i, step in enumerate(steps):
        if isinstance(step, dict) and step.get("id") == GUARD_STEP_ID:
            break
    else:
        return None, None, None
    if not all(is_runner_hardening(s) for s in steps[:i]):
        return step, None, f"runs a step other than the runner hardening before `{GUARD_STEP_ID}`"
    for key in ("if", "continue-on-error", "uses"):
        if key in step:
            return step, None, f"`{GUARD_STEP_ID}` sets `{key}`, so it can pass with a key present"
    env = step.get("env") or {}
    for key, value in env.items():
        if not (isinstance(key, str) and is_provider_credential(key) and passes_through(key, value)):
            return step, None, (f"`{GUARD_STEP_ID}` sets env {key} to something other than the "
                                f"provider credential of that name")
    run = str(step.get("run", ""))
    loop = GUARD_LIST.search(run)
    looped = {n for n in re.split(r"[\s\\]+", loop.group(1)) if n} if loop else set()
    if looped != set(env):
        return step, None, (f"`{GUARD_STEP_ID}` does not loop `for name in ...; do` over "
                            f"exactly the names in its env")
    if '-n "${!name}"' not in run or "exit 1" not in run:
        return step, None, (f'`{GUARD_STEP_ID}` does not fail (`exit 1`) when one of them is '
                            f'non-empty (`-n "${{!name}}"`)')
    return step, {str(k).upper() for k in env}, None


LOCAL_WORKFLOW = "./.github/workflows/"


def workflow_call(doc):
    """Returns the workflow's `on.workflow_call` mapping ({} when it is bare), or None when
    the workflow cannot be called. PyYAML reads the bare key `on` as True."""
    on = doc.get("on", doc.get(True))
    if on == "workflow_call" or (isinstance(on, list) and "workflow_call" in on):
        return {}
    if isinstance(on, dict) and "workflow_call" in on:
        return on["workflow_call"] if isinstance(on["workflow_call"], dict) else {}
    return None


def environment_secret_reads(doc):
    """Returns {name: job id} for each secret that a job naming live-providers reads, with
    the key None for a dynamic read. GITHUB_TOKEN is always there and is left out."""
    reads = {}
    for job_id, job in (doc.get("jobs") or {}).items():
        if isinstance(job, dict) and "uses" not in job and environment_name(job) == ENVIRONMENT:
            for _, secret in all_secret_reads(job, "job"):
                if secret is None or secret.upper() != "GITHUB_TOKEN":
                    reads.setdefault(secret, job_id)
    return reads


def called_workflow(caller_path, uses):
    """Loads the workflow a `uses: ./.github/workflows/<file>` call names, from the caller's
    own directory, or returns None for a workflow this check cannot read."""
    if not isinstance(uses, str) or not uses.startswith(LOCAL_WORKFLOW):
        return None
    path = os.path.join(os.path.dirname(os.path.abspath(caller_path)), uses[len(LOCAL_WORKFLOW):])
    if not os.path.isfile(path):
        return None
    doc = load_yaml(path)
    return doc if workflow_call(doc) is not None else None


def passes_through(name, value):
    """True for `NAME: ${{ secrets.NAME }}`: the caller's own value of the secret, which is
    empty for an environment-only secret, so the called job reads the environment's."""
    return isinstance(value, str) and value.strip() == "${{ secrets.%s }}" % name


def listed(names):
    names = sorted(names)
    shown = ", ".join(names[:4])
    return shown + (f" and {len(names) - 4} more" if len(names) > 4 else "")


def problems(path):
    doc = load_yaml(path)
    name = os.path.basename(path)
    call = workflow_call(doc)
    declared = {str(k).upper() for k in ((call or {}).get("secrets") or {})}

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
            called = called_workflow(path, job["uses"])
            needed = environment_secret_reads(called) if called is not None else {}
            needed_upper = {n.upper() for n in needed if n is not None}
            passed = job.get("secrets") if isinstance(job.get("secrets"), dict) else {}
            through = {k for k, v in passed.items()
                       if isinstance(k, str) and k.upper() in needed_upper and passes_through(k, v)}
            rest = {k: v for k, v in job.items() if k != "uses"}
            if passed:
                rest["secrets"] = {k: v for k, v in passed.items() if k not in through}
            found = list(secret_reads(rest, "call"))
            if found:
                yield (f"{where}: passes {summarize(found)} to a reusable workflow; a calling "
                       f"job cannot name an environment, so the called workflow's job must "
                       f"name `environment: {ENVIRONMENT}` and read the secret itself. A "
                       f"caller passes a provider credential only as "
                       f"`NAME: ${{{{ secrets.NAME }}}}`, and only to a workflow in this "
                       f"repository whose live-providers jobs read it")
            through_upper = {k.upper() for k in through}
            missing = {n for n in needed if n is not None and n.upper() not in through_upper}
            if missing and job.get("secrets") != "inherit":
                yield (f"{where}: calls {job['uses']}, whose live-providers jobs read "
                       f"{listed(missing)}, and does not pass each as "
                       f"`NAME: ${{{{ secrets.NAME }}}}`; a called job reads an environment "
                       f"secret as empty unless its caller passes it by its own name "
                       f"(TXE-7574)")
            credentials = {k.upper() for k in through if is_provider_credential(k)}
            if credentials:
                needs = job.get("needs") or []
                needs = [needs] if isinstance(needs, str) else needs
                guarded = set()
                for need in needs:
                    needed_job = (doc.get("jobs") or {}).get(need)
                    if isinstance(needed_job, dict) and "environment" not in needed_job:
                        _, names, problem = guard(needed_job)
                        if names and not problem:
                            guarded |= names
                unguarded = credentials - guarded
                if unguarded:
                    yield (f"{where}: passes {listed(unguarded)} by name, and needs no job "
                           f"whose `{GUARD_STEP_ID}` step fails when one of them is a "
                           f"repository or organization secret; such a secret would be the "
                           f"value passed, and every called job, those without the "
                           f"environment too, would read it with no approval (TXE-7574)")
            continue

        if environment_name(job) == ENVIRONMENT:
            problem = canary_problem(job)
            if problem:
                yield (f"{where}: names `environment: {ENVIRONMENT}` but {problem}; the "
                       f"canary step fails the job when the environment's secrets did not "
                       f"reach it (TXE-7574)")
            problem = key_count_problem(job)
            if problem:
                yield (f"{where}: names `environment: {ENVIRONMENT}` but {problem}; a job "
                       f"with no provider key skips its suites and still passes, and the count "
                       f"is what says so (TXE-8129)")
            if call is not None:
                reads = {s for _, s in all_secret_reads(job, "job")
                         if s is None or s.upper() != "GITHUB_TOKEN"}
                if None in reads:
                    yield (f"{where}: names `environment: {ENVIRONMENT}` in a called workflow "
                           f"and reads secrets dynamically (secrets[...], toJSON(secrets)); a "
                           f"called job gets only the secrets its caller passes by name, so "
                           f"read each one by name")
                undeclared = {s for s in reads if s is not None and s.upper() not in declared}
                if undeclared:
                    yield (f"{where}: names `environment: {ENVIRONMENT}` in a called workflow "
                           f"and reads {listed(undeclared)}, which on.workflow_call.secrets "
                           f"does not declare; a called job reads an environment secret as "
                           f"empty unless its workflow declares it and the caller passes it "
                           f"as `NAME: ${{{{ secrets.NAME }}}}` (TXE-7574)")
            continue

        step, _, problem = guard(job)
        if problem:
            yield (f"{where}: {problem}; it is the only step outside "
                   f"`environment: {ENVIRONMENT}` that may read provider credentials (TXE-7574)")
        elif step is not None:
            # Only the guard's validated env entries are exempt; a secret read anywhere else in
            # the step (its run, name or shell) is checked like any other.
            bare = {k: v for k, v in step.items() if k != "env"}
            job = dict(job, steps=[bare if s is step else s for s in job.get("steps") or []])
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
