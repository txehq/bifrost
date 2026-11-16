#!/usr/bin/env python3
"""Fails when a workflow, action or workflow script installs or runs a tool at `@latest`.

A tool fetched at `@latest` changes under the workflow without a commit: the same commit
can pass one day and fail the next, and a failure cannot be reproduced locally at the
version CI used. Docs Validation ran `npx mintlify@latest` and load-test.sh installed
vegeta `@latest` (TXE-7592). Name an exact version instead.

A comment line (`#` after optional whitespace) is not checked.

Usage: check-pinned-tools.py [file ...]
  With no arguments, checks .github/workflows/*.y*ml, .github/workflows/scripts/* and
  .github/actions/**.
"""
import pathlib
import re
import sys

LATEST = re.compile(r"@latest\b")
ROOT = pathlib.Path(__file__).resolve().parents[3]


def default_files():
    gh = ROOT / ".github"
    yield from sorted((gh / "workflows").glob("*.y*ml"))
    yield from sorted(p for p in (gh / "workflows" / "scripts").rglob("*") if p.is_file())
    yield from sorted(p for p in (gh / "actions").rglob("*") if p.is_file())


def problems(paths):
    for path in paths:
        if path.name == pathlib.Path(__file__).name:
            continue  # this file names the pattern it looks for
        try:
            text = path.read_text()
        except UnicodeDecodeError:
            continue
        for number, line in enumerate(text.splitlines(), 1):
            if line.lstrip().startswith("#"):
                continue
            if LATEST.search(line):
                yield f"{path}:{number}: a tool at @latest; pin an exact version: {line.strip()}"


def main(argv):
    paths = [pathlib.Path(a) for a in argv] or list(default_files())
    found = list(problems(paths))
    for line in found:
        print(f"FAIL: {line}")
    print(f"{len(paths)} files checked, {len(found)} unpinned tool(s)")
    return 1 if found else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
