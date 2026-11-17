#!/usr/bin/env python3
"""Every container image CI pulls comes from the GHCR mirror, pinned by digest (TXE-7684).

WHY. CI pulled its service images (Postgres, Weaviate, Redis, Qdrant, ClickHouse, the Dockerfile
bases) from Docker Hub without logging in. Anonymous pulls share a rate limit per address, and on
17 Nov 2026 "Harness fixture folders (offline)" failed on PR #249 with `toomanyrequests` while
pulling cr.weaviate.io/semitechnologies/weaviate:1.32.4 (run 43981835859). cr.weaviate.io fronts
Docker Hub and shares that limit. Each failure cost the merge-slot holder a CI cycle and a manual
rerun.

HOW. One source of truth: each image reference in a file CI reads is written as

    ${BIFROST_CI_REGISTRY:-docker.io}/<path>:<tag>@sha256:<digest>      (compose files, scripts)
    FROM ${BIFROST_CI_REGISTRY}/<path>:<tag>@sha256:<digest>            (Dockerfiles, with
                                                                         ARG BIFROST_CI_REGISTRY=docker.io)

Outside CI the variable is unset, so a laptop pulls docker.io as before. In CI the shared action
.github/actions/ci-images logs in to GHCR with GITHUB_TOKEN, checks that the mirror holds every
pinned digest, and sets BIFROST_CI_REGISTRY=ghcr.io/<owner>/bifrost-ci. Pulls then go to GHCR,
authenticated, with no anonymous Docker Hub limit. ci-images-mirror.yml copies each pinned
upstream image into the mirror byte for byte (crane copy), so the mirror keeps the upstream digest
and a pull by digest verifies it.

Subcommands:
  list                    print each pinned image: <upstream>@<digest> -> <mirror>
  lint                    fail on a Docker Hub or cr.weaviate.io image a CI file pulls directly,
                          a mirrored reference with no digest, two digests for one tag, and a
                          job that uses the action without the GHCR egress or packages:read
  check-mirror [--wait S] fail unless the mirror holds every pinned digest; with --wait, retry for
                          up to S seconds (a PR that adds an image waits for its mirror job)
  mirror                  copy every pinned image the mirror lacks, then check-mirror
  --self-test             the lint rules on fixture text

The lint scans the files CI reads images from: .github/** (workflows, actions, scripts, configs),
the compose files under tests/, and transports/Dockerfile*. framework/ and plugins/ compose files
are local development stacks no job starts.
"""
import json
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

REPO = Path(__file__).resolve().parents[3]
VAR = "BIFROST_CI_REGISTRY"
ACTION = "./.github/actions/ci-images"
MIRROR_HOSTS = ("ghcr.io:443", "pkg-containers.githubusercontent.com:443")
# Registries whose anonymous pulls share Docker Hub's rate limit. cr.weaviate.io fronts Docker Hub
# (its 429 body says "You have reached your unauthenticated pull rate limit").
HUB_REGISTRIES = {"docker.io", "index.docker.io", "registry-1.docker.io", "cr.weaviate.io"}

# Files under the scanned paths that name an image no job pulls, and why.
EXEMPT = {
    ".github/workflows/scripts/validate-helm-templates.sh": "busybox appears in Helm values rendered to YAML, never pulled",
}

DIGEST = r"sha256:[0-9a-f]{64}"
NAME = r"[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*"
PATH_RE = rf"{NAME}(?:/{NAME})*"
TAG = r"[\w][\w.-]{0,127}"
# ${BIFROST_CI_REGISTRY:-docker.io}/path:tag@digest, or ${BIFROST_CI_REGISTRY}/... in a Dockerfile.
MIRRORED_RE = re.compile(
    r"\$\{" + VAR + r"(?::-(?P<default>[^}]*))?\}/(?P<path>" + PATH_RE + r")(?::(?P<tag>" + TAG + r"))?(?:@(?P<digest>" + DIGEST + r"))?"
)
REF_RE = re.compile(
    r"^(?:(?P<registry>[a-z0-9.-]+(?::\d+)?)/)?(?P<path>" + PATH_RE + r")(?::(?P<tag>" + TAG + r"))?(?:@(?P<digest>" + DIGEST + r"))?$"
)
IMAGE_KEY_RE = re.compile(r"^\s*-?\s*image:\s*[\"']?([^\"'\s#]+)")
FROM_RE = re.compile(r"^\s*FROM\s+(?:--\S+\s+)*(\S+)(?:\s+AS\s+(\S+))?", re.IGNORECASE)
ASSIGN_RE = re.compile(r"\b[A-Z_]*IMAGE[A-Z_]*=[\"']?([^\"'\s]+)")
DOCKER_CMD_RE = re.compile(r"\bdocker\s+(?:pull|run|create)\b(.*)")


def tracked():
    out = subprocess.run(["git", "-C", str(REPO), "ls-files"], check=True, capture_output=True, text=True).stdout
    return out.splitlines()


def scanned(path):
    name = path.rsplit("/", 1)[-1]
    if path.startswith(".github/"):
        return not re.search(r"\.test\.[a-z]+$", name)  # test fixtures print refs into temp files
    if path.startswith("tests/") and re.match(r"(docker-)?compose[\w.-]*\.ya?ml$", name):
        return True
    return path.startswith("transports/Dockerfile")


class Image:
    def __init__(self, path, tag, digest, where):
        self.path, self.tag, self.digest, self.where = path, tag, digest, where

    def upstream(self):
        return f"docker.io/{self.path}@{self.digest}"

    def mirror_repo(self, owner):
        return f"{owner}/bifrost-ci/{self.path}"

    def mirror(self, owner):
        return f"ghcr.io/{self.mirror_repo(owner)}:{self.tag}"


def candidates(path, text):
    """(line number, reference, is_dockerfile) for every image reference a line names."""
    is_dockerfile = path.rsplit("/", 1)[-1].startswith("Dockerfile")
    stages = set()
    for number, line in enumerate(text.splitlines(), 1):
        stripped = line.lstrip()
        if stripped.startswith(("#", "//")):
            continue
        refs = []
        if is_dockerfile:
            m = FROM_RE.match(line)
            if m:
                if m.group(1).lower() not in stages:
                    refs.append(m.group(1))
                if m.group(2):
                    stages.add(m.group(2).lower())
        else:
            m = IMAGE_KEY_RE.match(line)
            if m:
                refs.append(m.group(1))
            # A NAME_IMAGE=value names an image only with a tag or digest: IMAGE_NAME="bifrost" does not.
            refs += [v for v in ASSIGN_RE.findall(line) if MIRRORED_RE.search(v) or re.search(r"[:@]", v)]
            m = DOCKER_CMD_RE.search(line)
            if m:
                for token in m.group(1).split():
                    token = token.strip("\"'")
                    if token.startswith("-") or token in ("\\", "|", "&&", ";"):
                        continue
                    if MIRRORED_RE.search(token) or ("$" not in token and REF_RE.match(token) and re.search(r"[a-z]", token.split(":")[0]) and (":" in token or "@" in token)):
                        refs.append(token)
        for ref in refs:
            yield number, ref, is_dockerfile


def analyse(files):
    """files maps a path to its text. Returns (images, problems)."""
    images, problems = [], []
    for path, text in sorted(files.items()):
        if path in EXEMPT:
            continue
        dockerfile_arg = re.search(r"^\s*ARG\s+" + VAR + r"=docker\.io\s*$", text, re.MULTILINE)
        for number, ref, is_dockerfile in candidates(path, text):
            where = f"{path}:{number}"
            m = MIRRORED_RE.search(ref)
            if m:
                default = m.group("default")
                if is_dockerfile and default is None and not dockerfile_arg:
                    problems.append(f"{where}: {ref} needs `ARG {VAR}=docker.io` before the first FROM")
                    continue
                if not is_dockerfile and default != "docker.io":
                    problems.append(f"{where}: {ref} must default to docker.io: ${{{VAR}:-docker.io}}/<path>")
                    continue
                if not m.group("tag") or not m.group("digest"):
                    problems.append(f"{where}: {ref} must carry a tag and a digest: the mirror is checked by digest")
                    continue
                images.append(Image(m.group("path"), m.group("tag"), m.group("digest"), where))
                continue
            if "$" in ref:
                continue  # a reference built from a variable, e.g. the image a job just built
            r = REF_RE.match(ref)
            if not r:
                continue
            registry = r.group("registry")
            if registry and "." not in registry and ":" not in registry and registry != "localhost":
                registry = None  # "library/postgres": the first component is a path, not a host
            if registry is None or registry in HUB_REGISTRIES:
                problems.append(
                    f"{where}: {ref} pulls from {registry or 'Docker Hub'} anonymously; write it as "
                    f"${{{VAR}:-docker.io}}/<path>:<tag>@<digest> so CI pulls the GHCR mirror (TXE-7684)"
                )
    seen = {}
    for image in images:
        key = (image.path, image.tag)
        if key in seen and seen[key].digest != image.digest:
            problems.append(f"{image.where}: {image.path}:{image.tag} is pinned to {image.digest}, but {seen[key].where} pins {seen[key].digest}")
        seen.setdefault(key, image)
    return images, problems


def workflow_problems(workflows):
    """workflows maps a path to parsed YAML. A job using the action needs GHCR egress and packages:read."""
    problems = []
    for path, doc in sorted(workflows.items()):
        top = (doc or {}).get("permissions")
        for job_name, job in ((doc or {}).get("jobs") or {}).items():
            if not isinstance(job, dict):
                continue
            steps = [s for s in job.get("steps") or [] if isinstance(s, dict)]
            if not any((s.get("uses") or "") == ACTION for s in steps):
                continue
            perms = job.get("permissions", top)
            level = perms.get("packages") if isinstance(perms, dict) else perms
            if level not in ("read", "write", "read-all", "write-all"):
                problems.append(f"{path} :: job '{job_name}' uses {ACTION} without permissions packages: read")
            for step in steps:
                if "step-security/harden-runner" in (step.get("uses") or ""):
                    w = step.get("with") or {}
                    if str(w.get("egress-policy", "")).strip() == "block":
                        allowed = set(str(w.get("allowed-endpoints") or "").split())
                        missing = [h for h in MIRROR_HOSTS if h not in allowed]
                        if missing:
                            problems.append(f"{path} :: job '{job_name}' uses {ACTION} but its egress allowlist omits {', '.join(missing)}")
    return problems


def load_files():
    files = {}
    for path in tracked():
        if scanned(path):
            p = REPO / path
            if p.is_file():
                try:
                    files[path] = p.read_text(encoding="utf-8")
                except UnicodeDecodeError:
                    pass
    return files


def load_workflows():
    import yaml

    out = {}
    for p in sorted((REPO / ".github/workflows").glob("*.y*ml")):
        out[str(p.relative_to(REPO))] = yaml.safe_load(p.read_text(encoding="utf-8"))
    return out


def unique(images):
    seen, out = set(), []
    for image in images:
        if (image.path, image.tag, image.digest) not in seen:
            seen.add((image.path, image.tag, image.digest))
            out.append(image)
    return out


def owner():
    return os.environ.get("GITHUB_REPOSITORY_OWNER", "txehq").lower()


def ghcr_has(image, token, actor):
    """True when the mirror serves the image's digest; False when it answers that it does not."""
    repo = image.mirror_repo(owner())
    req = urllib.request.Request(f"https://ghcr.io/token?service=ghcr.io&scope=repository:{repo}:pull")
    if token:
        import base64

        req.add_header("Authorization", "Basic " + base64.b64encode(f"{actor}:{token}".encode()).decode())
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            bearer = json.load(resp).get("token", "")
    except urllib.error.HTTPError as e:
        if e.code in (401, 403, 404):
            return False  # GHCR refuses a token for a repository that does not exist yet
        raise
    head = urllib.request.Request(f"https://ghcr.io/v2/{repo}/manifests/{image.digest}", method="HEAD")
    head.add_header("Authorization", f"Bearer {bearer}")
    head.add_header(
        "Accept",
        ", ".join(
            [
                "application/vnd.oci.image.index.v1+json",
                "application/vnd.docker.distribution.manifest.list.v2+json",
                "application/vnd.oci.image.manifest.v1+json",
                "application/vnd.docker.distribution.manifest.v2+json",
            ]
        ),
    )
    try:
        with urllib.request.urlopen(head, timeout=30) as resp:
            served = resp.headers.get("Docker-Content-Digest", image.digest)
    except urllib.error.HTTPError as e:
        if e.code in (401, 403, 404):
            return False
        raise
    if served != image.digest:
        raise SystemExit(f"FAIL: {image.mirror(owner())} serves {served} for {image.digest}")
    return True


def check_mirror(images, wait):
    token = os.environ.get("GHCR_TOKEN") or os.environ.get("GITHUB_TOKEN", "")
    actor = os.environ.get("GITHUB_ACTOR", "ci")
    deadline = time.monotonic() + wait
    missing = list(images)
    while True:
        still = []
        for image in missing:
            try:
                ok = ghcr_has(image, token, actor)
            except (urllib.error.URLError, TimeoutError) as e:
                print(f"retry: {image.mirror(owner())}: {e}", file=sys.stderr)
                ok = False
            if ok:
                print(f"ok - {image.mirror(owner())}@{image.digest}")
            else:
                still.append(image)
        missing = still
        if not missing or time.monotonic() >= deadline:
            break
        print(f"waiting for {len(missing)} image(s) the mirror job has not copied yet", file=sys.stderr)
        time.sleep(30)
    for image in missing:
        print(
            f"FAIL: the mirror lacks {image.upstream()} ({image.where}); "
            f"ci-images-mirror.yml copies it to {image.mirror(owner())}",
            file=sys.stderr,
        )
    return 1 if missing else 0


def mirror(images):
    crane = os.environ.get("CRANE", "crane")
    token = os.environ.get("GHCR_TOKEN") or os.environ.get("GITHUB_TOKEN", "")
    actor = os.environ.get("GITHUB_ACTOR", "ci")
    for image in images:
        if ghcr_has(image, token, actor):
            print(f"ok - {image.mirror(owner())}@{image.digest} already mirrored")
            continue
        print(f"copy {image.upstream()} -> {image.mirror(owner())}")
        subprocess.run([crane, "copy", image.upstream(), image.mirror(owner())], check=True)
    return check_mirror(images, 0)


def self_test():
    d = "sha256:" + "a" * 64
    e = "sha256:" + "b" * 64
    cases = [
        ("tests/docker-compose.yml", f"    image: ${{{VAR}:-docker.io}}/library/postgres:16-alpine@{d}\n", 1, 0),
        ("tests/docker-compose.yml", f"    image: postgres:16-alpine@{d}\n", 0, 1),
        ("tests/docker-compose.yml", "    image: redis:7.4-alpine\n", 0, 1),
        ("tests/docker-compose.yml", f"    image: cr.weaviate.io/semitechnologies/weaviate:1.32.4@{d}\n", 0, 1),
        ("tests/docker-compose.yml", "    image: ghcr.io/pinecone-io/pinecone-index:latest\n", 0, 0),
        ("tests/docker-compose.yml", f"    image: ${{{VAR}:-docker.io}}/library/redis:7.4-alpine\n", 0, 1),
        ("tests/docker-compose.yml", f"    image: ${{{VAR}:-cr.weaviate.io}}/semitechnologies/weaviate:1@{d}\n", 0, 1),
        ("tests/docker-compose.yml", f"    # image: postgres:16-alpine@{d}\n", 0, 0),
        (".github/workflows/scripts/x.sh", f'IMAGE="cr.weaviate.io/semitechnologies/weaviate:1.32.4@{d}"\n', 0, 1),
        (".github/workflows/scripts/x.sh", f'IMAGE="${{{VAR}:-docker.io}}/semitechnologies/weaviate:1.32.4@{d}"\n', 1, 0),
        (".github/workflows/scripts/x.sh", "docker run --rm -p 8080:8080 alpine:3.21\n", 0, 1),
        (".github/workflows/scripts/x.sh", 'docker run -d "$IMAGE" --port 8080\n', 0, 0),
        (".github/workflows/scripts/x.sh", "docker run -p 8080:8080 ghcr.io/txehq/bifrost:v1\n", 0, 0),
        (".github/workflows/x.yml", "      - run: docker pull postgres:16-alpine\n", 0, 1),
        ("transports/Dockerfile", f"ARG {VAR}=docker.io\nFROM ${{{VAR}}}/library/alpine:3.23.5@{d} AS base\nFROM base\n", 1, 0),
        ("transports/Dockerfile", f"FROM ${{{VAR}}}/library/alpine:3.23.5@{d}\n", 0, 1),
        ("transports/Dockerfile", f"FROM alpine:3.23.5@{d}\n", 0, 1),
        ("transports/Dockerfile", f"FROM registry.access.redhat.com/ubi9/ubi-minimal@{d}\n", 0, 0),
        (".github/workflows/scripts/validate-helm-templates.sh", "    image: busybox:1.36\n", 0, 0),
    ]
    failed = 0
    for path, text, want_images, want_problems in cases:
        images, problems = analyse({path: text})
        if len(images) != want_images or len(problems) != want_problems:
            failed += 1
            print(f"FAIL self-test {path}: {text.strip()!r}: {len(images)} image(s), problems {problems}", file=sys.stderr)
    images, problems = analyse(
        {
            "tests/a.yml": f"image: ${{{VAR}:-docker.io}}/library/redis:7@{d}\n",
            "tests/docker-compose.yml": f"image: ${{{VAR}:-docker.io}}/library/redis:7@{e}\n",
        }
    )
    if len(problems) != 1:
        failed += 1
        print(f"FAIL self-test: two digests for one tag gave {problems}", file=sys.stderr)
    wf = {
        "ok.yml": {
            "jobs": {
                "a": {
                    "permissions": {"contents": "read", "packages": "read"},
                    "steps": [
                        {"uses": "step-security/harden-runner@x", "with": {"egress-policy": "block", "allowed-endpoints": " ".join(MIRROR_HOSTS)}},
                        {"uses": ACTION},
                    ],
                }
            }
        },
        "bad.yml": {
            "permissions": {"contents": "read"},
            "jobs": {
                "b": {
                    "steps": [
                        {"uses": "step-security/harden-runner@x", "with": {"egress-policy": "block", "allowed-endpoints": "ghcr.io:443"}},
                        {"uses": ACTION},
                    ]
                }
            },
        },
    }
    got = workflow_problems(wf)
    if len(got) != 2 or not all(p.startswith("bad.yml") for p in got):
        failed += 1
        print(f"FAIL self-test workflow rules: {got}", file=sys.stderr)
    if failed:
        return 1
    print("self-test passed")
    return 0


def main(argv):
    if not argv or argv[0] in ("-h", "--help"):
        print(__doc__)
        return 0
    cmd = argv[0]
    if cmd == "--self-test":
        return self_test()
    images, problems = analyse(load_files())
    if cmd == "list":
        for image in unique(images):
            print(f"{image.upstream()} -> {image.mirror(owner())}")
        return 0
    if cmd == "lint":
        problems += workflow_problems(load_workflows())
        stale = [p for p in EXEMPT if not (REPO / p).is_file()]
        problems += [f"EXEMPT names {p}, which no longer exists" for p in stale]
        for p in problems:
            print(f"FAIL {p}", file=sys.stderr)
        if problems:
            return 1
        print(f"OK: {len(unique(images))} pinned image(s), each pulled through the GHCR mirror; no direct Docker Hub pull in a CI file.")
        return 0
    if problems:
        for p in problems:
            print(f"FAIL {p}", file=sys.stderr)
        return 1
    if cmd == "check-mirror":
        wait = int(argv[argv.index("--wait") + 1]) if "--wait" in argv else 0
        return check_mirror(unique(images), wait)
    if cmd == "mirror":
        return mirror(unique(images))
    print(f"unknown subcommand {cmd}", file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
