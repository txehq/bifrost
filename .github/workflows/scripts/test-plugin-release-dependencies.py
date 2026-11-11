"""Exercise the release dependency update without publishing anything.

Runs the "Update plugin dependencies" block of release-single-plugin.sh, and the line that
derives MODULE_ROOT, against a stub `go` in a scratch tree. The stub answers `go mod edit -json`
and records every `-require=` the block sets; nothing is downloaded, built or pushed.

The module root is a made-up one, so a module path written into the script by hand fails here:
the fork kept its Go module paths through a repository transfer (8a8239c45), and the script
reads the root from core/go.mod for that reason.
"""
import json
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPTS = Path(__file__).resolve().parent
MODULE_ROOT = "example.test/fork/bifrost"

HARNESS = '''
set -euo pipefail
PLUGIN_DIR=plugins/subject
PLUGIN_NAME=subject
CORE_VERSION=v1.8.5
FRAMEWORK_VERSION=v1.6.1
ROOT="$PWD"
git() { :; }
go() {
  if [[ "$*" == "mod edit -json core/go.mod" ]]; then
    cat "$ROOT/core.json"
  elif [[ "$*" == "mod edit -json" ]]; then
    cat "$ROOT/module.json"
  elif [[ "$1 $2" == "mod edit" ]]; then
    shift 2
    for arg in "$@"; do
      [[ "$arg" == -require=* ]] && printf '%s\\n' "${arg#-require=}" >> "$ROOT/requires"
    done
    return 0
  elif [[ "$*" == "mod tidy" ]]; then
    cp "$ROOT/requires" "$ROOT/before-tidy"
  elif [[ "$1" == "build" ]]; then
    : > "$ROOT/built"
  else
    echo "unexpected go call: $*" >&2
    return 1
  fi
}
'''


def script_parts():
    """The MODULE_ROOT line and the dependency block, as release-single-plugin.sh has them."""
    script = (SCRIPTS / "release-single-plugin.sh").read_text()
    root_lines = [line for line in script.splitlines() if line.startswith("MODULE_ROOT=")]
    if len(root_lines) != 1:
        raise AssertionError(f"release-single-plugin.sh sets MODULE_ROOT on {len(root_lines)} lines, want 1")
    block = script.split("# Update plugin dependencies\n", 1)[1].split("cd ../..", 1)[0]
    return root_lines[0], block


class PluginReleaseDependenciesTest(unittest.TestCase):
    def run_block(self, root, requirements):
        plugin = root / "plugins" / "subject"
        plugin.mkdir(parents=True)
        (plugin / "go.mod").touch()
        (root / "core.json").write_text(json.dumps({"Module": {"Path": MODULE_ROOT + "/core"}}))
        (root / "module.json").write_text(json.dumps({"Require": requirements}))
        root_line, block = script_parts()
        return subprocess.run(
            ["bash", "-c", HARNESS + '\nsource "$1"\n' + root_line + "\n" + block, "test", str(SCRIPTS / "go-utils.sh")],
            cwd=root, capture_output=True, text=True,
        )

    def test_updates_plugin_dependencies_before_tidy(self):
        for dependency, version in [("governance", "1.7.1"), ("mocker", "v1.6.1"), (None, None)]:
            with self.subTest(dependency=dependency), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                requirements = [{"Path": "example.com/external", "Version": "v1.0.0"}]
                expected = [MODULE_ROOT + "/core@v1.8.5", MODULE_ROOT + "/framework@v1.6.1"]
                if dependency:
                    sibling = root / "plugins" / dependency
                    sibling.mkdir(parents=True)
                    (sibling / "version").write_text(version + "\n")
                    module = MODULE_ROOT + "/plugins/" + dependency
                    requirements.append({"Path": module, "Version": "v1.0.0"})
                    expected.append(module + "@v" + version.lstrip("v"))
                result = self.run_block(root, requirements)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertEqual((root / "before-tidy").read_text().splitlines(), expected)
                self.assertTrue((root / "built").exists(), "the plugin build was not validated after the update")

    def test_a_plugin_dependency_without_a_version_file_stops_the_release(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            module = MODULE_ROOT + "/plugins/missing"
            result = self.run_block(root, [{"Path": module, "Version": "v1.0.0"}])
            self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
            self.assertIn("Version file not found for plugin dependency: " + module, result.stdout)
            self.assertFalse((root / "before-tidy").exists(), "go mod tidy ran although a dependency had no version")

    def test_a_module_of_another_root_is_not_treated_as_a_plugin_dependency(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            result = self.run_block(root, [{"Path": "github.com/maximhq/bifrost/plugins/governance", "Version": "v1.0.0"}])
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertEqual((root / "before-tidy").read_text().splitlines(), [MODULE_ROOT + "/core@v1.8.5", MODULE_ROOT + "/framework@v1.6.1"])


if __name__ == "__main__":
    unittest.main()
