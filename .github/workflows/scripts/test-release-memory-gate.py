"""Check that every release job honours the memory-regression gate.

test-memory fails on a deterministic memory regression, and approve-memory-regression is
its break-glass. A release job that is gated on the core test results but not on this pair
publishes a module (framework, plugins) built on code the memory gate rejected, with no
approval on record. Every job that waits on test-core must therefore also need test-memory
and approve-memory-regression and admit a failed test-memory only when it was approved.
"""
from pathlib import Path
import unittest

import yaml


PIPELINE = Path(__file__).resolve().parent.parent / "release-pipeline.yml"
MEMORY_CLAUSE = (
    "(needs.test-memory.result == 'success' || (needs.test-memory.result == 'skipped' && (needs.check-skip.outputs.skip-tests == 'true' || (needs.detect-changes.outputs.core-needs-release != 'true' && needs.detect-changes.outputs.bifrost-http-needs-release != 'true'))) || (needs.test-memory.result == 'failure' && needs.approve-memory-regression.result == 'success'))"
)


class ReleaseMemoryGateTest(unittest.TestCase):
    def test_every_core_gated_release_job_honours_the_memory_gate(self):
        jobs = yaml.safe_load(PIPELINE.read_text())["jobs"]
        # notify and the docs changelog push publish no module or image.
        gated = [
            name for name, job in jobs.items()
            if "test-core" in (job.get("needs") or [])
            and not name.startswith(("test-", "approve-"))
            and name not in ("notify", "push-mintlify-changelog")
        ]
        self.assertIn("framework-release", gated)
        self.assertIn("plugins-release", gated)
        for name in gated:
            job = jobs[name]
            with self.subTest(job=name):
                needs = job.get("needs") or []
                self.assertIn("test-memory", needs)
                self.assertIn("approve-memory-regression", needs)
                self.assertIn(MEMORY_CLAUSE, job.get("if", ""))


if __name__ == "__main__":
    unittest.main()
