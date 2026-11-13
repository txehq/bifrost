import { readFileSync } from 'fs'
import { resolve } from 'path'

/**
 * The offline quarantine (TXE-7385): tests that fail in the offline run (`run-e2e.mjs --offline`,
 * the "E2E UI (offline)" job) and are listed in quarantine.json, each under the ticket that fixes it.
 *
 * In an offline run the base fixture marks each listed test as expected to fail (testInfo.fail), so:
 *   - a listed test that fails does not fail the run;
 *   - a listed test that passes fails the run ("expected to fail, but passed"): remove its entry;
 *   - a listed test that no longer exists fails the run (reporters/quarantine.ts): remove its entry.
 * Every other test fails the run as usual. Outside an offline run the list does nothing.
 *
 * quarantine.json maps a ticket to { kind, file, tests }: file is relative to tests/e2e, and each
 * test is its describe titles and its own title joined by " › ", as the failures report prints them.
 */
export type QuarantineEntry = { kind: string; file: string; tests: string[] }

// E2E_QUARANTINE_FILE points a run at another list, to check the rules above on a sample one.
export const QUARANTINE_FILE = process.env.E2E_QUARANTINE_FILE || resolve(__dirname, 'quarantine.json')

export const isOfflineRun = () => process.env.E2E_OFFLINE === '1'

export function loadQuarantine(path = QUARANTINE_FILE): Map<string, string> {
  const raw = JSON.parse(readFileSync(path, 'utf8')) as Record<string, QuarantineEntry>
  const byTest = new Map<string, string>()
  for (const [ticket, entry] of Object.entries(raw)) {
    if (!/^TXE-\d+$/.test(ticket)) throw new Error(`quarantine.json: ${ticket} is not a ticket id`)
    if (!entry.kind || !entry.file || !Array.isArray(entry.tests) || entry.tests.length === 0) {
      throw new Error(`quarantine.json: ${ticket} needs a kind, a file and at least one test`)
    }
    for (const title of entry.tests) {
      const key = quarantineKey(entry.file, title)
      if (byTest.has(key)) throw new Error(`quarantine.json: ${key} is listed twice`)
      byTest.set(key, ticket)
    }
  }
  return byTest
}

export const quarantineKey = (file: string, title: string) => `${file} :: ${title}`
