#!/usr/bin/env python3
"""Fails when a shell script that sets pipefail pipes into a grep that exits early (TXE-7109).

    cmd | grep -q PATTERN        # also -qx, -Eq, --quiet, --silent

grep -q exits at the first match. A writer that is still writing is then killed by SIGPIPE
(status 141), and under `set -o pipefail` the pipeline's status is the writer's, so a line
grep FOUND reads as not found. It happens at random, more often under load:
tests/test_offline_test.sh looked names up with `printf '%s\\n' "$child" | grep -qx "$name"`
and reported HOME missing from an environment that had it, 2 times in 3,600 lookups. The same
lookup read the other way (`if ... | grep -q; then leaked`) hides a real finding.

Write the lookup so the reader takes all of its input:

    cmd | grep PATTERN >/dev/null

Checked: every tracked *.sh file that sets pipefail (`set -o pipefail`, `set -euo pipefail`,
...). A line that must keep the early exit, because the writer never ends and the pipeline's
status is not read, can end with the comment `# pipefail-grep-quiet: <reason>`.

Usage: check-pipefail-grep-quiet.py [--self-test]
"""
import re
import subprocess
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parents[3]

PIPEFAIL = re.compile(r"^\s*set\s+(-[A-Za-z]*o\s+pipefail|-o\s+pipefail)\b", re.M)
# A pipe (not ||) into grep whose options include q, --quiet or --silent.
QUIET_GREP = re.compile(r"(?<!\|)\|\s*grep\s+(?:-[A-Za-z0-9]+\s+)*(?:-[A-Za-z]*q[A-Za-z]*|--quiet|--silent)(?=\s|$)")
ALLOW = re.compile(r"#\s*pipefail-grep-quiet:\s*\S")


def findings(path, source):
    """One line per pipe into a quiet grep in a script that sets pipefail."""
    if not PIPEFAIL.search(source):
        return []
    found = []
    for number, line in enumerate(source.splitlines(), 1):
        if line.lstrip().startswith("#") or ALLOW.search(line):
            continue
        if QUIET_GREP.search(line):
            found.append(f"{path}:{number}: pipes into grep -q under pipefail; write `| grep ... >/dev/null`: {line.strip()[:120]}")
    return found


def self_test():
    head = "#!/usr/bin/env bash\nset -euo pipefail\n"
    cases = [
        ("flagged: printf into grep -qx", head + 'if printf "%s\\n" "$x" | grep -qx "$n"; then :; fi\n', 1),
        ("flagged: -Eq, --quiet and --silent", head + "a | grep -Eq x\nb | grep --quiet x\nc | grep -i --silent x\n", 3),
        ("flagged: set -o pipefail on its own", "set -e\nset -o pipefail\na | grep -q x\n", 1),
        ("flagged: -Eeuo pipefail, a second stage", "set -Eeuo pipefail\na | tr , '\\n' | grep -qx x\n", 1),
        ("clean: the reader takes all its input", head + "a | grep -x x >/dev/null\n", 0),
        ("clean: grep -q on a file or a here-string", head + 'grep -q x file\ngrep -qx "$n" <<<"$x"\n', 0),
        ("clean: || is not a pipe", head + "a || grep -q x file\n", 0),
        ("clean: a comment", head + "# a | grep -q x\n", 0),
        ("clean: no pipefail in the script", "#!/bin/sh\nset -eu\na | grep -q x\n", 0),
        ("clean: an allowed line says why", head + "tail -f log | grep -q ready # pipefail-grep-quiet: the writer never ends\n", 0),
        ("flagged: an allow comment with no reason", head + "a | grep -q x # pipefail-grep-quiet:\n", 1),
    ]
    failed = 0
    for label, source, want in cases:
        got = len(findings("sample.sh", source))
        if got != want:
            failed += 1
            print(f"  FAIL - {label}: {got} findings, want {want}")
        else:
            print(f"  ok - {label}")
    return failed


def main():
    if sys.argv[1:] == ["--self-test"]:
        failed = self_test()
        print("self-test: all passed" if failed == 0 else f"self-test: {failed} failed")
        return 1 if failed else 0
    if sys.argv[1:]:
        print(__doc__)
        return 2
    files = subprocess.run(["git", "-C", str(REPO), "ls-files", "-z", "*.sh"], check=True, capture_output=True, text=True).stdout.split("\0")
    files = [name for name in files if name]
    if len(files) < 50:
        print(f"found {len(files)} tracked shell scripts, want the whole repository: the listing no longer works")
        return 1
    found = []
    checked = 0
    for name in files:
        path = REPO / name
        if not path.is_file():
            continue
        source = path.read_text(encoding="utf-8", errors="replace")
        if PIPEFAIL.search(source):
            checked += 1
        found.extend(findings(name, source))
    if found:
        print("\n".join(found))
        print(f"{len(found)} pipes into grep -q in scripts that set pipefail (see this script's header)")
        return 1
    print(f"ok - no pipe into grep -q in the {checked} shell scripts that set pipefail ({len(files)} tracked)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
