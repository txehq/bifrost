# Fails when core's, framework's or any plugin's changelog is not empty.
#
# The fork publishes only the transport and the Helm chart. detect-all-changes.sh releases a
# module whenever its version has no tag in its track and its changelog is not empty, and a
# module release cannot succeed here: release-all-plugins.sh resolves
# github.com/capsohq/bifrost/core and framework from proxy.golang.org, which has no fork
# versions (404). TXE-5261 emptied the plugin changelogs after Release Pipeline 40381837308
# tried all twelve plugins; nothing kept them empty, so #381 wrote its BREAKING line into
# plugins/governance/changelog.md, and sync.13's run 44223574926 failed in plugins-release
# with nothing tagged. drop_unpublishable_plugins (TXE-7907) now leaves such a plugin out of
# the release, so a plugin entry is never published at all; a core or framework entry still
# starts core-release or framework-release, which this fork does not publish. A note for a
# module goes in transports/changelog.md, which the transport release publishes.
#
# The rule is has_nonempty_changelog's in detect-all-changes.sh: lines that start with
# "<!--" or "-->" are dropped, then any non-whitespace character makes the file non-empty.
#
# Usage: check-module-changelogs-empty.py [--root DIR] | --self-test
import argparse
import glob
import os
import sys
import tempfile


def module_changelogs(root):
    paths = [os.path.join(root, "core", "changelog.md"), os.path.join(root, "framework", "changelog.md")]
    paths += sorted(glob.glob(os.path.join(root, "plugins", "*", "changelog.md")))
    return [p for p in paths if os.path.isfile(p)]


def is_nonempty(path):
    with open(path, encoding="utf-8") as f:
        kept = [line for line in f if not line.startswith("<!--") and not line.startswith("-->")]
    return any(not c.isspace() for line in kept for c in line)


def check(root, report=True):
    found = [os.path.relpath(p, root) for p in module_changelogs(root) if is_nonempty(p)]
    for rel in found if report else []:
        print(
            f"::error file={rel}::{rel} is not empty, so the release pipeline would try to release "
            f"this module and fail on a module version the fork never publishes. Move the entry "
            f"to transports/changelog.md and leave {rel} empty."
        )
    return found


def self_test():
    # The fixture tree sits under the repository's ignored tmp/ directory.
    base = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "..", "tmp")
    os.makedirs(base, exist_ok=True)
    cases = [
        ("empty files", {"core": "", "framework": "\n", "plugins/a": "  \n\n"}, []),
        ("comment markers only", {"plugins/a": "<!-- kept empty -->\n-->\n"}, []),
        ("a plugin line", {"core": "", "plugins/governance": "- **BREAKING** a line\n"}, ["plugins/governance/changelog.md"]),
        ("core and framework", {"core": "x", "framework": "<!-- c -->\ny\n"}, ["core/changelog.md", "framework/changelog.md"]),
    ]
    failed = 0
    for name, files, want in cases:
        with tempfile.TemporaryDirectory(dir=base) as root:
            for module, text in files.items():
                os.makedirs(os.path.join(root, module), exist_ok=True)
                with open(os.path.join(root, module, "changelog.md"), "w", encoding="utf-8") as f:
                    f.write(text)
            got = check(root, report=False)
        if got != want:
            print(f"self-test {name!r}: got {got}, want {want}")
            failed += 1
    print(f"self-test: {len(cases) - failed} of {len(cases)} cases pass")
    return failed


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", default=".")
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    if args.self_test:
        sys.exit(1 if self_test() else 0)
    found = check(args.root)
    count = len(module_changelogs(args.root))
    if found:
        print(f"{len(found)} of {count} module changelogs are not empty")
        sys.exit(1)
    print(f"ok: {count} module changelogs are empty")


main()
