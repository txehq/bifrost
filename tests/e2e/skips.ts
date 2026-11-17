import { readFileSync } from 'fs'
import { resolve } from 'path'

/**
 * The offline skip list (TXE-7733): the tests allowed to skip in an offline run (`run-e2e.mjs
 * --offline`, the "E2E UI (offline)" job), each under the ticket that records why.
 *
 * A skip hides a test as completely as a deleted one, and the suite used to decide many skips with a
 * probe that does not wait (`isVisible()`, `isEnabled()`): a slow render made the test skip, and the
 * run stayed green. routing-rules skipped 22 of its 28 tests on one run and 67 of 85 on another, on
 * the same tree. So in an offline run (reporters/skips.ts):
 *   - a test that skips and is not listed here fails the run, naming the test and its skip reason;
 *   - a listed test that runs without skipping fails the run: remove its entry;
 *   - a listed test, or a listed spec file, that no longer exists fails the run: remove its entry.
 * scripts/skips.test.mjs pins these rules on the sample suite in selftest/skips. Outside an offline
 * run the list does nothing. A runtime skip waits for the state it tests (core/utils/availability.ts),
 * and scripts/no-instant-skip.test.mjs fails a spec that skips on anything else without saying why.
 *
 * offline-skips.json maps a ticket to { reason, file, tests }, or to a list of them when one ticket
 * covers several spec files: file is relative to tests/e2e, and each test is its describe titles and
 * its own title joined by " › ", as quarantine.json names them.
 */
export type SkipEntry = { reason: string; file: string; tests: string[] }

// E2E_SKIPS_FILE points a run at another list, to check the rules above on a sample one.
export const SKIPS_FILE = process.env.E2E_SKIPS_FILE || resolve(__dirname, 'offline-skips.json')

export function loadSkips(path = SKIPS_FILE): Map<string, string> {
  const raw = JSON.parse(readFileSync(path, 'utf8')) as Record<string, SkipEntry | SkipEntry[]>
  const byTest = new Map<string, string>()
  for (const [ticket, value] of Object.entries(raw)) {
    if (!/^TXE-\d+$/.test(ticket)) throw new Error(`offline-skips.json: ${ticket} is not a ticket id`)
    const entries = Array.isArray(value) ? value : [value]
    if (entries.length === 0) throw new Error(`offline-skips.json: ${ticket} lists nothing`)
    for (const entry of entries) {
      if (!entry.reason || !entry.file || !Array.isArray(entry.tests) || entry.tests.length === 0) {
        throw new Error(`offline-skips.json: each entry of ${ticket} needs a reason, a file and at least one test`)
      }
      for (const title of entry.tests) {
        const key = skipKey(entry.file, title)
        if (byTest.has(key)) throw new Error(`offline-skips.json: ${key} is listed twice`)
        byTest.set(key, ticket)
      }
    }
  }
  return byTest
}

export const skipKey = (file: string, title: string) => `${file} :: ${title}`
