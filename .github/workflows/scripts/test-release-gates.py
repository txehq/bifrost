#!/usr/bin/env python3
"""Replays release-pipeline.yml job conditions for a transport-only release.

Every Docker image and transport release builds from the workspace's core, framework
and plugins (both Dockerfiles copy go.work and the module trees), so their suites have
to run and pass before an image is published, even when no module publishes itself
because its changelog is empty. This evaluates the real `if:` expressions, and the
detect-changes outputs they read, against scripted job results; no runner is involved.

It also pins the protected-main contract: release jobs build and tag the tested commit,
never write main, and an image is published only after the bifrost-http release it
ships has verified and completed. And every job that runs a script calling go sets up
the Go the workspace requires before it does, and every actions/setup-go step in any
workflow turns its shared built-in cache off. And every artifact a job downloads behind an
environment approval is kept at least as long as the approval window docs/release.md states.

Usage: python3 test-release-gates.py
"""
import ast
import fnmatch
import importlib.util
import json
import os
import re
import subprocess
import tempfile
import unittest
from pathlib import Path

import yaml

WORKFLOW = Path(__file__).resolve().parent.parent / "release-pipeline.yml"
JOBS = yaml.safe_load(WORKFLOW.read_text())["jobs"]
WORKFLOWS = WORKFLOW.parent

# Suites an image or transport release embeds the code of, and the skip flags that may
# legitimately skip each one (an admin skip_tests dispatch, or --skip-core-test for the
# suites that call live providers).
EMBEDDED_SUITES = {
    "test-core": ("skip-tests", "skip-core-test"),
    "test-core-integrations": ("skip-tests", "skip-core-test"),
    "test-core-unit": ("skip-tests",),
    "resolve-cli-versions": ("skip-tests",),
    "test-cli-harness": ("skip-tests",),
    "test-framework": ("skip-tests",),
    "test-plugins": ("skip-tests",),
}
PUBLISHERS = ["bifrost-http-prep", "docker-build-amd64", "docker-build-arm64", "docker-build-ubi9-amd64", "docker-build-ubi9-arm64"]
DOCKER_PUBLISHERS = PUBLISHERS[1:]
# Jobs that follow a due bifrost-http release. The docs changelog clears
# transports/changelog.md, and detection reads an empty one as "no release due", so it
# must not be generated for a release that never completed.
RELEASE_FOLLOWERS = [*DOCKER_PUBLISHERS, "push-mintlify-changelog"]
# Jobs that build or tag release artifacts from the checkout.
BUILDERS = ["build-binaries-x86", "build-binaries-arm64", "bifrost-http-release", *DOCKER_PUBLISHERS]
# (workflow, job) that used to push to main and now leave a patch artifact instead.
PATCH_JOBS = {
    ("release-pipeline.yml", "bifrost-http-prep"): "release-prep",
    ("release-pipeline.yml", "push-mintlify-changelog"): "docs-changelog",
    ("release-cli.yml", "push-mintlify-changelog"): "cli-docs-changelog",
}
UPLOAD_ARTIFACT = "actions/upload-artifact@bbbca2ddaa5d8feaa63e36b76fdaad77386f024f"
# go.work's go directive. An older go on PATH fetches this toolchain from proxy.golang.org,
# which most release jobs' egress allowlists block.
WORKSPACE_GO = re.search(r"^go (\S+)$", (WORKFLOWS.parent.parent / "go.work").read_text(), re.M).group(1)
# Every job gets Go from this action (TXE-7621; check-go-setup.py enforces it), and it is the one
# place actions/setup-go runs.
SETUP_GO_ACTION = "./.github/actions/setup-go"
ACTIONS = WORKFLOWS.parent / "actions"


def setup_go_action_steps():
    """The actions/setup-go steps inside the composite actions: the wrapper's, and any other."""
    for path in sorted(ACTIONS.glob("*/action.y*ml")):
        doc = yaml.safe_load(path.read_text()) or {}
        for step in ((doc.get("runs") or {}).get("steps") or []):
            if str(step.get("uses", "")).startswith("actions/setup-go@"):
                yield path, step
GO_COMMAND = re.compile(r"(?:^|[\s;&|(`])go (?:build|env|generate|get|install|list|mod|run|test|vet|work)\b")


def scripts_calling_go():
    """Workflow scripts whose non-comment lines run a go command, directly or through
    another workflow script they call or source."""
    code = {
        script.name: "\n".join(line for line in script.read_text().splitlines() if not line.lstrip().startswith("#"))
        for script in (WORKFLOWS / "scripts").glob("*.sh")
    }
    found = {name for name, text in code.items() if GO_COMMAND.search(text)}
    while True:
        callers = {name for name, text in code.items() if name not in found and any(callee in text for callee in found)}
        if not callers:
            return found
        found |= callers


def evaluate(expression, context):
    """Evaluates a GitHub Actions expression over context, which maps dotted names
    (needs.x.result, needs.x.outputs.y, steps.x.outputs.y) to strings."""
    expr = expression.strip()
    if expr.startswith("${{") and expr.endswith("}}"):
        expr = expr[3:-2]
    unknown = []

    def ref(match):
        name = match.group(0)
        if name not in context:
            unknown.append(name)
        return repr(context.get(name, ""))

    expr = re.sub(r"\b(?:needs|steps)\.[A-Za-z0-9_-]+\.(?:result|outputs\.[A-Za-z0-9_-]+)", ref, expr)
    expr = expr.replace("always()", "True").replace("!cancelled()", "True").replace("cancelled()", "False")
    expr = expr.replace("&&", " and ").replace("||", " or ").replace("!=", " __NE__ ").replace("==", " __EQ__ ")
    expr = expr.replace("!", " not ").replace("__NE__", "!=").replace("__EQ__", "==")
    if unknown:
        raise AssertionError(f"expression reads unset context {sorted(set(unknown))}: {expression}")
    value = _evaluate_node(ast.parse(" ".join(expr.split()), mode="eval").body)
    if isinstance(value, str):
        return value
    return "true" if value else "false"


def _evaluate_node(node):
    """Walks the translated expression; anything beyond literals, and/or/not and
    ==/!= is rejected, so no workflow text can run as code."""
    if isinstance(node, ast.Constant) and isinstance(node.value, (str, bool)):
        return node.value
    if isinstance(node, ast.BoolOp):
        values = [_evaluate_node(v) for v in node.values]
        return all(values) if isinstance(node.op, ast.And) else any(values)
    if isinstance(node, ast.UnaryOp) and isinstance(node.op, ast.Not):
        return not _evaluate_node(node.operand)
    if isinstance(node, ast.Compare) and len(node.ops) == 1 and isinstance(node.ops[0], (ast.Eq, ast.NotEq)):
        equal = _evaluate_node(node.left) == _evaluate_node(node.comparators[0])
        return equal if isinstance(node.ops[0], ast.Eq) else not equal
    raise AssertionError(f"unsupported expression element {ast.dump(node)}")


def transport_only_context(results, skip_tests="false", skip_core_test="false", transport_release="true"):
    """A transport/Docker release with every module changelog empty. With
    transport_release="false" only the image is released (its version is already tagged)."""
    flags = {
        "core-needs-release": "false",
        "framework-needs-release": "false",
        "plugins-need-release": "false",
        "bifrost-http-needs-release": transport_release,
        "docker-needs-release": "true",
    }
    context = {f"steps.detect.outputs.{k}": v for k, v in flags.items()}
    context.update({f"needs.detect-changes.outputs.{k}": v for k, v in flags.items()})
    for name, expression in JOBS["detect-changes"]["outputs"].items():
        if name.endswith("needs-tests"):
            context[f"needs.detect-changes.outputs.{name}"] = evaluate(str(expression), context)
    context.update({
        "needs.check-skip.outputs.should-skip": "false",
        "needs.check-skip.outputs.skip-tests": skip_tests,
        "needs.check-skip.outputs.skip-core-test": skip_core_test,
        "needs.detect-changes.result": "success",
        "needs.detect-changes.outputs.changed-plugins": "[]",
    })
    for job in JOBS:
        context.setdefault(f"needs.{job}.result", results.get(job, "success"))
    return context


class TransportOnlyReleaseGates(unittest.TestCase):
    def test_embedded_suites_run_for_a_transport_only_release(self):
        context = transport_only_context({})
        for suite in EMBEDDED_SUITES:
            with self.subTest(suite=suite):
                self.assertEqual(evaluate(str(JOBS[suite]["if"]), context), "true")

    def test_publishing_requires_every_embedded_suite_to_pass(self):
        context = transport_only_context({})
        for publisher in PUBLISHERS:
            with self.subTest(publisher=publisher):
                self.assertEqual(evaluate(str(JOBS[publisher]["if"]), context), "true")

    def test_an_unexplained_skip_or_failure_blocks_publishing(self):
        for suite in EMBEDDED_SUITES:
            for result in ("skipped", "failure", "cancelled"):
                if suite == "test-core" and result == "failure":
                    continue  # the flaky-approval path, covered below
                context = transport_only_context({suite: result, "approve-flaky-test-core": "skipped"})
                for publisher in PUBLISHERS:
                    with self.subTest(suite=suite, result=result, publisher=publisher):
                        self.assertEqual(evaluate(str(JOBS[publisher]["if"]), context), "false")

    def test_a_failed_test_core_publishes_only_after_flaky_approval(self):
        for approval, expected in (("success", "true"), ("skipped", "false"), ("failure", "false")):
            context = transport_only_context({"test-core": "failure", "approve-flaky-test-core": approval})
            for publisher in PUBLISHERS:
                with self.subTest(approval=approval, publisher=publisher):
                    self.assertEqual(evaluate(str(JOBS[publisher]["if"]), context), expected)

    def test_explicit_skip_flags_still_allow_their_skips(self):
        everything_skipped = {suite: "skipped" for suite in EMBEDDED_SUITES}
        context = transport_only_context(everything_skipped, skip_tests="true")
        for publisher in PUBLISHERS:
            with self.subTest(flag="skip-tests", publisher=publisher):
                self.assertEqual(evaluate(str(JOBS[publisher]["if"]), context), "true")
        live_skipped = {"test-core": "skipped", "test-core-integrations": "skipped", "approve-flaky-test-core": "skipped"}
        context = transport_only_context(live_skipped, skip_core_test="true")
        for publisher in PUBLISHERS:
            with self.subTest(flag="skip-core-test", publisher=publisher):
                self.assertEqual(evaluate(str(JOBS[publisher]["if"]), context), "true")
        # --skip-core-test covers only the live suites.
        context = transport_only_context({**live_skipped, "test-core-unit": "skipped"}, skip_core_test="true")
        for publisher in PUBLISHERS:
            with self.subTest(flag="skip-core-test does not cover unit", publisher=publisher):
                self.assertEqual(evaluate(str(JOBS[publisher]["if"]), context), "false")


class DockerWaitsForTheTransportRelease(unittest.TestCase):
    """An image ships the transport, so while a bifrost-http release is due it is
    published only after prep verified the commit and the release completed. A skipped
    prep or release is not a pass: the fork's v2.1.1 images went out with both skipped.
    The docs changelog waits the same way (see RELEASE_FOLLOWERS)."""

    def test_a_due_release_that_did_not_complete_blocks_every_image(self):
        for job in ("bifrost-http-prep", "bifrost-http-release"):
            for result in ("skipped", "failure", "cancelled"):
                context = transport_only_context({job: result})
                for publisher in RELEASE_FOLLOWERS:
                    with self.subTest(job=job, result=result, publisher=publisher):
                        self.assertEqual(evaluate(str(JOBS[publisher]["if"]), context), "false")

    def test_a_completed_release_publishes_every_image(self):
        context = transport_only_context({})
        for publisher in RELEASE_FOLLOWERS:
            with self.subTest(publisher=publisher):
                self.assertEqual(evaluate(str(JOBS[publisher]["if"]), context), "true")

    def test_an_image_only_release_does_not_wait_for_a_transport_release(self):
        context = transport_only_context({"bifrost-http-prep": "skipped", "bifrost-http-release": "skipped"}, transport_release="false")
        for publisher in RELEASE_FOLLOWERS:
            with self.subTest(publisher=publisher):
                self.assertEqual(evaluate(str(JOBS[publisher]["if"]), context), "true")

    def test_images_need_the_jobs_their_condition_reads(self):
        for publisher in RELEASE_FOLLOWERS:
            with self.subTest(publisher=publisher):
                self.assertIn("bifrost-http-prep", JOBS[publisher]["needs"])
                self.assertIn("bifrost-http-release", JOBS[publisher]["needs"])


class MemoryGate(unittest.TestCase):
    """test-memory runs for a core or bifrost-http release and gates every job that names
    it in its condition. A failure passes only with the break-glass approval; a skip passes
    only under skip-tests or when the gate had nothing to cover, as for an image-only
    republish, which must not be stranded."""

    # The break-glass job itself reads the result to run on a failure; it is not gated.
    GATED = [job for job, spec in JOBS.items() if job != "approve-memory-regression" and "needs.test-memory.result" in str(spec.get("if", ""))]

    def test_the_gate_covers_every_image_publisher(self):
        for publisher in DOCKER_PUBLISHERS:
            with self.subTest(publisher=publisher):
                self.assertIn(publisher, self.GATED)

    def test_an_image_only_release_is_not_blocked_by_the_skipped_gate(self):
        context = transport_only_context({"test-memory": "skipped", "bifrost-http-prep": "skipped", "bifrost-http-release": "skipped"}, transport_release="false")
        self.assertEqual(evaluate(str(JOBS["test-memory"]["if"]), context), "false")
        for publisher in DOCKER_PUBLISHERS:
            with self.subTest(publisher=publisher):
                self.assertEqual(evaluate(str(JOBS[publisher]["if"]), context), "true")

    def test_a_transport_release_needs_the_gate_to_pass(self):
        for result in ("skipped", "failure", "cancelled"):
            context = transport_only_context({"test-memory": result, "approve-memory-regression": "skipped"})
            self.assertEqual(evaluate(str(JOBS["test-memory"]["if"]), context), "true")
            for job in self.GATED:
                with self.subTest(result=result, job=job):
                    self.assertEqual(evaluate(str(JOBS[job]["if"]), context), "false")

    def test_a_failed_gate_passes_only_with_approval(self):
        for approval, expected in (("success", "true"), ("skipped", "false"), ("failure", "false")):
            context = transport_only_context({"test-memory": "failure", "approve-memory-regression": approval})
            for publisher in PUBLISHERS:
                if publisher not in self.GATED:
                    continue
                with self.subTest(approval=approval, publisher=publisher):
                    self.assertEqual(evaluate(str(JOBS[publisher]["if"]), context), expected)


class ReleaseJobsLeaveMainToReviewedPRs(unittest.TestCase):
    """main requires a reviewed codex-review check, so no release job writes it: generated
    changes leave as patch artifacts, and every artifact is built from the tested commit."""

    def test_no_job_pulls_or_pushes_a_branch(self):
        for name in ("release-pipeline.yml", "release-cli.yml", "openapi-bundle.yml"):
            jobs = yaml.safe_load((WORKFLOWS / name).read_text())["jobs"]
            for job_name, job in jobs.items():
                for step in job.get("steps", []):
                    run = step.get("run", "")
                    with self.subTest(workflow=name, job=job_name, step=step.get("name")):
                        self.assertNotIn("git pull", run)
                        self.assertNotIn("git commit", run)
                        self.assertIsNone(re.search(r"git push origin (main|HEAD|\"?\$)", run))

    def test_scripts_that_pushed_to_main_no_longer_write_git(self):
        for script in ("release-bifrost-http-prep.sh", "push-mintlify-changelog.sh", "push-cli-mintlify-changelog.sh"):
            lines = (WORKFLOWS / "scripts" / script).read_text().splitlines()
            code = "\n".join(line for line in lines if not line.lstrip().startswith("#"))
            for command in ("git pull", "git push", "git commit", "git config", "go mod tidy"):
                with self.subTest(script=script, command=command):
                    self.assertFalse(command in code, f"{script} runs {command}")

    def test_builders_assert_the_tested_commit_before_building(self):
        for job_name in BUILDERS:
            steps = JOBS[job_name]["steps"]
            checks = [i for i, step in enumerate(steps) if "is not the tested commit" in step.get("run", "")]
            builds = [i for i, step in enumerate(steps)
                      if "docker-build-push-action" in step.get("uses", "")
                      or any(s in step.get("run", "") for s in ("build-executables.sh", "release-bifrost-http-finalize.sh"))]
            with self.subTest(job=job_name):
                self.assertTrue(checks and builds, "no tested-commit check or no build step found")
                self.assertLess(checks[0], builds[0])

    def test_patch_jobs_hold_no_write_token_and_upload_only_their_patch(self):
        for (name, job_name), kind in PATCH_JOBS.items():
            job = yaml.safe_load((WORKFLOWS / name).read_text())["jobs"][job_name]
            with self.subTest(workflow=name, job=job_name):
                self.assertEqual(job["permissions"], {"contents": "read"})
                checkout = next(s for s in job["steps"] if s.get("uses", "").startswith("actions/checkout@"))
                self.assertIs(checkout["with"].get("persist-credentials"), False)
                self.assertNotIn("token", checkout["with"])
                uploads = [s for s in job["steps"] if s.get("uses", "").startswith("actions/upload-artifact@")]
                self.assertEqual(len(uploads), 1)
                upload = uploads[0]
                self.assertTrue(upload["uses"].startswith(UPLOAD_ARTIFACT))
                self.assertEqual(upload["if"], "${{ !cancelled() }}")
                self.assertNotIn("continue-on-error", upload)
                self.assertEqual(upload["with"]["path"], f"${{{{ runner.temp }}}}/release-patches/{kind}-v*.patch")
                self.assertTrue(upload["with"]["name"].startswith(f"{kind}-v"))
                self.assertTrue(upload["with"]["name"].endswith("-${{ github.sha }}"))
                self.assertLessEqual(upload["with"]["retention-days"], 14)
                self.assertIs(upload["with"]["overwrite"], True)

    def test_openapi_bundle_only_checks_the_spec(self):
        workflow = yaml.safe_load((WORKFLOWS / "openapi-bundle.yml").read_text())
        self.assertEqual(workflow["permissions"], {"contents": "read"})
        for job in workflow["jobs"].values():
            runs = " ".join(step.get("run", "") for step in job["steps"])
            self.assertIn("is stale", runs)
            self.assertNotIn("git push", runs)


class GoStepsHaveTheWorkspaceGo(unittest.TestCase):
    """A release job that reaches go without setup-go gets the runner image's older Go,
    which tries to fetch WORKSPACE_GO and is blocked: bifrost-http-release died that
    way before tagging v2.1.3 (TXE-4977)."""

    def test_scripts_calling_go_are_detected_directly_and_through_wrappers(self):
        go_scripts = scripts_calling_go()
        self.assertIn("release-bifrost-http-finalize.sh", go_scripts)
        # release-all-plugins.sh runs go only through release-single-plugin.sh.
        self.assertIn("release-all-plugins.sh", go_scripts)

    def test_every_go_step_follows_setup_go_for_the_workspace_version(self):
        go_scripts = scripts_calling_go()
        checked = 0
        for job_name, job in JOBS.items():
            steps = job.get("steps") or []
            for i, step in enumerate(steps):
                run = step.get("run", "")
                if not (GO_COMMAND.search(run) or any(script in run for script in go_scripts)):
                    continue
                checked += 1
                setups = [s for s in steps[:i] if s.get("uses", "") == SETUP_GO_ACTION]
                with self.subTest(job=job_name, step=step.get("name")):
                    self.assertTrue(setups, f"runs go with no {SETUP_GO_ACTION} step before it")
        self.assertGreater(checked, 0)

    def test_the_setup_go_action_installs_the_workspace_version(self):
        steps = [step for path, step in setup_go_action_steps() if path.parent.name == "setup-go"]
        self.assertEqual(len(steps), 1, f"{SETUP_GO_ACTION} runs actions/setup-go once")
        self.assertEqual(str((steps[0].get("with") or {}).get("go-version")), WORKSPACE_GO)


class SetupGoCacheIsOff(unittest.TestCase):
    """actions/setup-go's built-in cache (on unless `cache: false`) keys one entry per Go
    version and go.sum hash, shared by every setup-go job of the repository and never
    overwritten on an exact hit, so the first job to save decides what every job restores
    (TXE-6214: a 6 KB stub). Here it has never restored anything (no go.sum at the root:
    "Dependencies file is not found"), so `cache: false` costs nothing and keeps an
    upstream sync from turning it on. A job that wants a Go build cache uses actions/cache
    with a key naming its purpose. The org scan's setup-go-cache-off rule checks the same
    after the merge; this fails the pull request (TXE-6242)."""

    def test_every_setup_go_step_sets_cache_false(self):
        checked = 0
        for path in sorted(WORKFLOWS.glob("*.y*ml")):
            jobs = (yaml.safe_load(path.read_text()) or {}).get("jobs") or {}
            for job_name, job in jobs.items():
                for step in job.get("steps") or []:
                    if not str(step.get("uses", "")).startswith("actions/setup-go@"):
                        continue
                    checked += 1
                    with self.subTest(workflow=path.name, job=job_name, step=step.get("name")):
                        self.assertIs((step.get("with") or {}).get("cache"), False,
                                      "set `cache: false` on actions/setup-go (TXE-6242)")
        # Since TXE-7621 the workflows reach actions/setup-go through ./.github/actions/setup-go.
        for path, step in setup_go_action_steps():
            checked += 1
            with self.subTest(action=str(path.relative_to(WORKFLOWS.parent.parent)), step=step.get("name")):
                self.assertIs((step.get("with") or {}).get("cache"), False,
                              "set `cache: false` on actions/setup-go (TXE-6242)")
        self.assertGreater(checked, 0)


GO_CACHE_ACTION = "./.github/actions/go-cache"
# The repository's Actions cache holds 10 GB (actions/cache/storage-limit) and evicts the least
# recently used entry past it. The Go purposes may declare 6 GB between them: a go.sum change
# writes a new entry per purpose before the old ones age out, and small setup-* caches share
# the rest.
GO_CACHE_BUDGET_MB = 6144


def workflow_steps():
    """(workflow file, workflow doc, job name, index, step) for every step of every workflow."""
    for path in sorted(WORKFLOWS.glob("*.y*ml")):
        doc = yaml.safe_load(path.read_text()) or {}
        for job_name, job in (doc.get("jobs") or {}).items():
            for i, step in enumerate(job.get("steps") or []):
                yield path.name, doc, job_name, i, step


def push_branches(workflow_file):
    """The branches a push runs this workflow on: its own push trigger, or, for a workflow that
    only runs when called, its callers' (release-pipeline.yml runs from release.yml)."""
    doc = yaml.safe_load((WORKFLOWS / workflow_file).read_text()) or {}
    on = doc.get("on", doc.get(True)) or {}
    branches = list((on.get("push") or {}).get("branches", []))
    if "workflow_call" in on:
        for path in sorted(WORKFLOWS.glob("*.y*ml")):
            caller = yaml.safe_load(path.read_text()) or {}
            if any(j.get("uses") == f"./.github/workflows/{workflow_file}" for j in (caller.get("jobs") or {}).values()):
                branches += push_branches(path.name)
    return branches


class ActionsCacheHasOneBudget(unittest.TestCase):
    """Every write to the Actions cache goes through the purpose-keyed go-cache action, whose
    saves declare their compressed size, and the declared sizes fit the budget (TXE-6254).

    On 2026-11-08 the cache held 12.2 GB against its 10 GB cap: 172 BuildKit layer blobs from
    the two UBI9 image builds (`cache-to: type=gha,mode=max`), ~1 GB per release per arch for
    the `go build` layer, which every release changes. Their only hits were apk and WORKDIR
    steps while the export cost 96-122 s a job (evidence txe-6254). An unbounded writer like
    that evicts every other entry, so none may come back: a new cache is a go-cache purpose
    with a max-mb that fits GO_CACHE_BUDGET_MB, or this test fails."""

    def go_cache_steps(self):
        for wf, doc, job_name, i, step in workflow_steps():
            if step.get("uses") == GO_CACHE_ACTION:
                w = step.get("with") or {}
                yield wf, doc, job_name, i, w.get("purpose"), w.get("mode", "restore"), w

    def test_no_buildkit_layer_cache_in_the_actions_cache(self):
        for wf, _, job_name, _, step in workflow_steps():
            w = step.get("with") or {}
            for key in ("cache-from", "cache-to"):
                with self.subTest(workflow=wf, job=job_name, step=step.get("name"), key=key):
                    self.assertNotIn("type=gha", str(w.get(key, "")),
                                     "BuildKit layer cache in the Actions cache is unbounded (TXE-6254)")

    def test_no_bare_actions_cache(self):
        for wf, _, job_name, _, step in workflow_steps():
            with self.subTest(workflow=wf, job=job_name, step=step.get("name")):
                self.assertFalse(str(step.get("uses", "")).startswith("actions/cache"),
                                 f"cache through {GO_CACHE_ACTION} with a purpose and max-mb (TXE-6254)")

    def test_each_purpose_has_one_saver_on_main_within_budget(self):
        savers, restored, total = {}, set(), 0
        for wf, doc, job_name, i, purpose, mode, w in self.go_cache_steps():
            with self.subTest(workflow=wf, job=job_name, purpose=purpose, mode=mode):
                self.assertRegex(str(purpose), r"^[a-z0-9][a-z0-9-]*$")
                self.assertIn(mode, ("restore", "save"))
                if mode == "restore":
                    restored.add(purpose)
                    continue
                self.assertNotIn(purpose, savers, f"also saved by {savers.get(purpose)}")
                savers[purpose] = f"{wf} {job_name}"
                self.assertRegex(str(w.get("max-mb", "")), r"^[0-9]+$", "a save declares max-mb")
                total += int(w["max-mb"])
                self.assertIn("main", push_branches(wf),
                              "a save runs only on main, so its workflow must run on a push to main")
                steps = doc["jobs"][job_name]["steps"]
                self.assertTrue(any(s.get("uses") == GO_CACHE_ACTION
                                    and (s.get("with") or {}).get("purpose") == purpose
                                    and (s.get("with") or {}).get("mode", "restore") == "restore"
                                    for s in steps[:i]), "save without a restore of it earlier in the job")
        self.assertTrue(savers)
        self.assertEqual(restored - set(savers), set(), "restored purposes nothing saves")
        self.assertLessEqual(total, GO_CACHE_BUDGET_MB, "declared go-cache max-mb total is over the budget")

    def test_restore_follows_setup_go(self):
        for wf, doc, job_name, i, purpose, mode, _ in self.go_cache_steps():
            if mode != "restore":
                continue
            steps = doc["jobs"][job_name]["steps"]
            with self.subTest(workflow=wf, job=job_name, purpose=purpose):
                self.assertTrue(any(s.get("uses", "") == SETUP_GO_ACTION for s in steps[:i]),
                                f"go-cache reads `go env`; restore it after {SETUP_GO_ACTION}")


TRIGGER_SCRIPT = WORKFLOWS / "scripts" / "release-trigger.sh"
ENTRY = WORKFLOWS / "release.yml"
ENTRY_DOC = yaml.safe_load(ENTRY.read_text())
ENTRY_JOBS = ENTRY_DOC["jobs"]
WORKFLOW_DOC = yaml.safe_load(WORKFLOW.read_text())
CALL = "./.github/workflows/release-pipeline.yml"
# Real Actions push payloads, captured from $GITHUB_EVENT_PATH (TXE-7566); see each file's _captured.
PAYLOADS = {name: json.loads((WORKFLOWS / "scripts" / "testdata" / name).read_text())
            for name in ("push-event-new-branch.json", "push-event-two-commits.json")}


def triggers(doc):
    return doc.get("on", doc.get(True)) or {}


class ReleaseOnlyFromAChosenCommit(unittest.TestCase):
    """A release is attempted from a commit someone chose: the push that changes a version
    file, or a dispatch. release.yml's gate decides from the git range, and only its gated
    `release` job calls the release jobs, so every other push to main runs no release job and
    asks nobody for a live-providers approval (TXE-7511, TXE-7566)."""

    def test_the_release_jobs_start_only_from_the_gated_caller(self):
        self.assertEqual(set(triggers(WORKFLOW_DOC)), {"workflow_call"}, "release-pipeline.yml must not start on its own")
        self.assertEqual(set(ENTRY_JOBS), {"gate", "release"})
        release = ENTRY_JOBS["release"]
        self.assertEqual(release["uses"], CALL)
        self.assertIn("gate", release["needs"])
        self.assertIn("if", release, "the caller must run only when the gate says the run may release")
        for may, expected in (("false", "false"), ("true", "true"), ("", "false")):
            with self.subTest(may_release=may):
                self.assertEqual(evaluate(str(release["if"]), {"needs.gate.outputs.may-release": may}), expected)
        callers = [(wf, job) for wf, doc, job, _, _ in workflow_jobs() if (doc["jobs"][job].get("uses") == CALL)]
        self.assertEqual(callers, [("release.yml", "release")], "only the gated caller may call the release jobs")

    def test_the_gate_reads_the_range_of_the_event(self):
        steps = ENTRY_JOBS["gate"]["steps"]
        trigger = next(s for s in steps if s.get("id") == "trigger")
        self.assertIn("release-trigger.sh", trigger["run"])
        self.assertEqual(trigger["env"], {"EVENT_NAME": "${{ github.event_name }}",
                                          "BEFORE_SHA": "${{ github.event.before }}",
                                          "AFTER_SHA": "${{ github.sha }}"})
        checkout = next(s for s in steps if str(s.get("uses", "")).startswith("actions/checkout@"))
        self.assertEqual(checkout["with"]["fetch-depth"], 0, "the gate diffs before..after, so it needs the history")
        self.assertLess(steps.index(checkout), steps.index(trigger))
        self.assertEqual(ENTRY_JOBS["gate"]["outputs"]["may-release"], "${{ steps.trigger.outputs.may-release }}")

    def test_every_event_field_read_exists_in_a_real_push_payload(self):
        # A field Actions leaves out of the push payload reads as empty, silently: the file
        # lists that sank the first concurrency fix (#358). Fields read only on a dispatch
        # (inputs) are not push fields.
        paths = set()
        for path in (ENTRY, WORKFLOW):
            paths |= set(re.findall(r"github\.event\.([A-Za-z0-9_.*]+)", path.read_text()))
        paths = {p.rstrip(".") for p in paths if not p.startswith("inputs")}
        self.assertIn("before", paths)
        for name, payload in PAYLOADS.items():
            for field in sorted(paths):
                with self.subTest(payload=name, field=field):
                    self.assertTrue(resolve(payload, field.split(".")), f"github.event.{field} is not in a real push payload")

    def test_a_new_branch_push_from_a_real_payload_releases_nothing(self):
        payload = PAYLOADS["push-event-new-branch.json"]
        with tempfile.TemporaryDirectory() as repo:
            subprocess.run(["git", "init", "-q"], cwd=repo, check=True)
            self.assertEqual(self.run_trigger(repo, "push", payload["before"], payload["after"]), "false")

    def run_trigger(self, repo, event, before, after):
        out = subprocess.run(["bash", str(TRIGGER_SCRIPT), event, before, after], cwd=repo,
                             capture_output=True, text=True, env={**os.environ, "GITHUB_OUTPUT": ""})
        self.assertEqual(out.returncode, 0, out.stderr)
        decided = [line for line in out.stdout.splitlines() if line.startswith("may-release=")]
        self.assertEqual(len(decided), 1, out.stdout)
        return decided[0].split("=", 1)[1]

    def test_the_trigger_releases_only_a_version_change_or_a_dispatch(self):
        with tempfile.TemporaryDirectory() as repo:
            def git(*args):
                return subprocess.run(["git", "-c", "user.name=t", "-c", "user.email=t@t", *args], cwd=repo,
                                      check=True, capture_output=True, text=True).stdout.strip()

            def commit(path, text):
                full = Path(repo) / path
                full.parent.mkdir(parents=True, exist_ok=True)
                full.write_text(text)
                git("add", "-A")
                git("commit", "-q", "-m", path)
                return git("rev-parse", "HEAD")

            git("init", "-q")
            base = commit("transports/version", "2.4.0-sync.10\n")
            bump = commit("transports/version", "2.4.0-sync.11\n")
            code = commit("core/schemas/x.go", "package schemas\n")
            plugin = commit("plugins/logging/version", "1.0.1\n")
            nested = commit("plugins/logging/sub/version", "not a plugin version\n")
            changelog = commit("transports/changelog.md", "- note\n")
            cases = [
                ("push", base, bump, "true"),            # the version bump
                ("push", bump, code, "false"),           # a merge after it
                ("push", bump, plugin, "true"),          # several commits, one bumps a plugin
                ("push", plugin, nested, "false"),       # not a version file
                ("push", nested, changelog, "false"),
                ("push", "0" * 40, bump, "false"),       # a new branch: no range to read
                ("push", "f" * 40, bump, "false"),       # an old head that is gone
                ("push", "", bump, "false"),
                ("workflow_dispatch", "", code, "true"),  # the retry and the deliberate release
                ("schedule", base, bump, "false"),
            ]
            for event, before, after, expected in cases:
                with self.subTest(event=event, before=before[:7], after=after[:7]):
                    self.assertEqual(self.run_trigger(repo, event, before, after), expected)


def workflow_jobs():
    for path in sorted(WORKFLOWS.glob("*.y*ml")):
        doc = yaml.safe_load(path.read_text()) or {}
        for job in (doc.get("jobs") or {}):
            yield path.name, doc, job, None, None


def resolve(value, parts):
    """True when the dotted path exists in value; `*` maps over a list and needs every item."""
    if not parts:
        return value is not None
    head, rest = parts[0], parts[1:]
    if head == "*":
        return isinstance(value, list) and bool(value) and all(resolve(v, rest) for v in value)
    return isinstance(value, dict) and head in value and resolve(value[head], rest)


class TheCallerPassesEverySecretTheReleaseJobsRead(unittest.TestCase):
    """A called workflow sees a repository secret only when its caller passes it, and an
    unpassed one reads as empty, silently. Provider credentials are never passed: they are
    live-providers environment secrets read by the jobs that name it, and passing one, or
    `secrets: inherit`, fails check-provider-key-environment.py. So every other secret the
    release jobs read is declared by release-pipeline.yml and passed by name from release.yml."""

    def test_declared_passed_and_read_are_one_list(self):
        spec = importlib.util.spec_from_file_location("provider_keys", WORKFLOWS / "scripts" / "check-provider-key-environment.py")
        keys = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(keys)
        read = {n for n in re.findall(r"secrets\.([A-Z0-9_]+)", WORKFLOW.read_text())
                if n != "GITHUB_TOKEN" and not keys.is_provider_credential(n)}
        declared = set(triggers(WORKFLOW_DOC)["workflow_call"].get("secrets") or {})
        passed = ENTRY_JOBS["release"].get("secrets")
        self.assertIsInstance(passed, dict, "pass secrets by name, never `inherit`")
        self.assertEqual(declared, read, "release-pipeline.yml declares exactly the repository secrets it reads")
        self.assertEqual(set(passed), read, "release.yml passes exactly those")
        for name, value in passed.items():
            with self.subTest(secret=name):
                self.assertEqual(value, f"${{{{ secrets.{name} }}}}")
                self.assertFalse(keys.is_provider_credential(name))


class ReleasesShareOneGroup(unittest.TestCase):
    """Two releases must never publish at once (both could see the same tag missing and race
    to create it in release-bifrost-http-finalize.sh), and a push that may not release must
    never replace a release run pending in the group. GitHub keeps one pending run per group
    and cancels the older one when another joins. So the one group sits on release.yml's gated
    caller job: it covers the whole called workflow, and a caller skipped by its `if:` never
    joins it (both measured with a scratch probe, 2026-11-16, TXE-7566)."""

    def test_the_group_is_on_the_gated_caller_only(self):
        self.assertEqual(ENTRY_JOBS["release"]["concurrency"], {"group": "release-pipeline", "cancel-in-progress": False})
        self.assertNotIn("concurrency", ENTRY_DOC, "a workflow-level group would hold every push")
        self.assertNotIn("concurrency", WORKFLOW_DOC, "the called workflow is already inside the caller's group")
        self.assertNotIn("concurrency", ENTRY_JOBS["gate"])
        for job, spec in JOBS.items():
            with self.subTest(job=job):
                self.assertNotIn("concurrency", spec, "a job-level group in a run serializes or cancels its own jobs")

    def test_no_other_workflow_joins_the_release_group(self):
        for path in sorted(WORKFLOWS.glob("*.y*ml")):
            text = path.read_text()
            if path.name == "release.yml":
                continue
            with self.subTest(workflow=path.name):
                self.assertNotRegex(text, r"group:\s*['\"]?release-pipeline['\"]?\s*$")


# The release runbook owns the approval window; the check below reads it from there.
RUNBOOK = WORKFLOWS.parent.parent / "docs" / "release.md"
# Environments with no approval rule. They hold secrets for trusted and untrusted CI runs and
# never wait. Every other environment name counts as an approval gate, so a new one is checked
# before anyone classifies it.
UNGATED_ENVIRONMENTS = {"ci-trusted-branch", "pr-testing"}
UPLOAD = "actions/upload-artifact@"
DOWNLOAD = "actions/download-artifact@"


def approval_window_days():
    match = re.search(r"\*\*Approval window: (\d+) days\.\*\*", RUNBOOK.read_text())
    assert match, f"{RUNBOOK.name} must state '**Approval window: <n> days.**'"
    return int(match.group(1))


def environment_names(environment):
    """Every environment a job's `environment:` can name. An expression yields its quoted
    literals; one with none yields itself, which is no ungated name, so it counts as a gate."""
    if isinstance(environment, dict):
        environment = environment.get("name")
    if not environment:
        return set()
    environment = str(environment)
    if "${{" not in environment:
        return {environment}
    return set(re.findall(r"'([^']+)'", environment)) or {environment}


def is_gated(spec):
    return bool(environment_names(spec.get("environment")) - UNGATED_ENVIRONMENTS)


def needs_of(spec):
    needs = spec.get("needs") or []
    return [needs] if isinstance(needs, str) else list(needs)


def ancestors(jobs, job):
    seen, stack = set(), needs_of(jobs[job])
    while stack:
        parent = stack.pop()
        if parent not in seen:
            seen.add(parent)
            stack.extend(needs_of(jobs.get(parent) or {}))
    return seen


def artifact_matcher(name):
    """A regex for an artifact name; an expression in it (a matrix value) matches anything."""
    parts = re.split(r"\$\{\{.*?\}\}", str(name))
    return re.compile("^" + ".*".join(re.escape(p) for p in parts) + "$")


def concrete(name, fill="x"):
    """The name with each expression replaced by `fill`."""
    return re.sub(r"\$\{\{.*?\}\}", fill, name)


def gated_consumer_findings(jobs, window):
    """Every artifact a job downloads after an approval gate that its producer keeps for less
    than `window` days. The gates that matter are the consumer and its ancestors that the
    producer does not wait for: they can still be pending when the artifact is uploaded."""
    uploads = []
    for job, spec in jobs.items():
        for step in spec.get("steps") or []:
            if str(step.get("uses", "")).startswith(UPLOAD):
                uploads.append((job, step.get("with") or {}))
    findings = []
    for consumer, spec in jobs.items():
        for step in spec.get("steps") or []:
            if not str(step.get("uses", "")).startswith(DOWNLOAD):
                continue
            want = step.get("with") or {}
            if "run-id" in want:
                findings.append(f"{consumer}: downloads from another run; its retention cannot be read here")
                continue
            if want.get("name"):
                wanted = lambda n, w=str(want["name"]): bool(artifact_matcher(n).match(concrete(w)) or artifact_matcher(w).match(concrete(n)))
            elif want.get("pattern"):
                wanted = lambda n, p=concrete(str(want["pattern"]), "*"): fnmatch.fnmatchcase(concrete(n), p)
            else:
                wanted = lambda n: True  # no name and no pattern downloads every artifact
            producers = [(job, up) for job, up in uploads if wanted(str(up.get("name", "artifact")))]
            if not producers:
                findings.append(f"{consumer}: downloads {want.get('name') or want.get('pattern') or 'every artifact'}, which no job in this workflow uploads")
                continue
            reach = ancestors(jobs, consumer) | {consumer}
            for producer, up in producers:
                gates = sorted(j for j in reach - ancestors(jobs, producer) - {producer} if is_gated(jobs[j]))
                if not gates:
                    continue
                days = up.get("retention-days")
                if not isinstance(days, int) or days < window:
                    findings.append(
                        f"{consumer} downloads {up.get('name')!r} from {producer} after the gate(s) {', '.join(gates)}, "
                        f"but its retention-days is {days!r}; it must be a literal of at least {window} (docs/release.md)")
    return findings


class GatedConsumersOutliveTheApprovalWindow(unittest.TestCase):
    """A job behind an environment approval waits for a reviewer, and an artifact it downloads
    must still exist when the reviewer comes. sync.13 (run 44223574926) waited about 32 hours
    while bifrost-http-binary was kept for 1 day, and every job that read it failed (TXE-7895).
    The window is the one the release runbook states, and the consumers are derived from every
    workflow's YAML, so a new gated consumer or a new environment is checked as it lands."""

    def test_the_runbook_states_the_window(self):
        self.assertGreaterEqual(approval_window_days(), 1)

    def test_every_gated_consumer_outlives_the_window(self):
        window = approval_window_days()
        for path in sorted(WORKFLOWS.glob("*.y*ml")):
            jobs = (yaml.safe_load(path.read_text()) or {}).get("jobs") or {}
            with self.subTest(workflow=path.name):
                self.assertEqual(gated_consumer_findings(jobs, window), [])

    def test_the_release_gates_are_seen(self):
        # The derivation sees the consumers that failed sync.13 as gated, so a pass above is
        # not a pass because nothing was found.
        self.assertTrue(is_gated(JOBS["test-cli-harness"]))
        self.assertTrue(is_gated(JOBS["test-api-integrations"]))
        self.assertFalse(is_gated(JOBS["build-gateway"]))

    def test_the_check_catches_a_short_retention_behind_a_gate(self):
        def upload(name, days):
            return {"uses": UPLOAD + "x", "with": {"name": name, "retention-days": days}}

        def download(**kw):
            return {"uses": DOWNLOAD + "x", "with": kw}

        jobs = {
            "build": {"steps": [upload("bin", 1), upload("report-${{ matrix.n }}", 1)]},
            "gate": {"needs": "build", "environment": {"name": "new-approval-env"}, "steps": []},
            "after-gate": {"needs": ["gate"], "steps": [download(name="bin")]},
            "gated": {"needs": "build", "environment": "live-providers", "steps": [download(pattern="report-*")]},
            "ungated": {"needs": "build", "environment": "${{ cond && 'pr-testing' || 'ci-trusted-branch' }}",
                        "steps": [download(name="bin")]},
            "all": {"needs": "gate", "steps": [download()]},
        }
        findings = gated_consumer_findings(jobs, 7)
        self.assertEqual(len([f for f in findings if f.startswith("after-gate ")]), 1)
        self.assertEqual(len([f for f in findings if f.startswith("gated ")]), 1)
        self.assertEqual(len([f for f in findings if f.startswith("all ")]), 2)
        self.assertFalse([f for f in findings if f.startswith("ungated")])
        jobs["build"]["steps"] = [upload("bin", 7), upload("report-${{ matrix.n }}", 7)]
        self.assertEqual(gated_consumer_findings(jobs, 7), [])
        jobs["gate"]["needs"] = []
        jobs["build"]["needs"] = "gate"  # a gate the producer already waited for does not count
        jobs["build"]["steps"] = [upload("bin", 1)]
        jobs["gated"]["steps"] = []
        self.assertEqual(gated_consumer_findings(jobs, 7), [])


if __name__ == "__main__":
    unittest.main()
