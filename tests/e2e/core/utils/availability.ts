import { expect, test, type Locator } from "@playwright/test";

/**
 * Skips that wait (TXE-7733). A test that may legitimately not apply (an enterprise-only control, a
 * permission the session lacks) skips through these, never through `isVisible()` / `isEnabled()`:
 * those answer at once, so a control that has not rendered yet read as "unavailable" and the test
 * skipped. These wait for the state first, and skip only when it never arrives. In an offline run a
 * skip must also be listed in offline-skips.json (reporters/skips.ts), so a control that should be
 * there and is not fails the run instead of hiding.
 *
 * scripts/no-instant-skip.test.mjs fails a spec that calls test.skip() on a condition without
 * `// skip-ok: <reason>`; these helpers are the way to skip on the page's state.
 */
export const AVAILABILITY_TIMEOUT = 10_000;

async function reaches(check: () => Promise<void>): Promise<boolean> {
	try {
		await check();
		return true;
	} catch {
		return false;
	}
}

/** Waits for `target` to be visible; skips the test with `reason` if it never is. */
export async function skipUnlessVisible(target: Locator, reason: string, timeout = AVAILABILITY_TIMEOUT): Promise<void> {
	const visible = await reaches(() => expect(target).toBeVisible({ timeout }));
	test.skip(!visible, reason); // skip-ok: decided by the wait above
}

/** Waits for `target` to be visible and enabled; skips the test with `reason` if it never is. */
export async function skipUnlessEnabled(target: Locator, reason: string, timeout = AVAILABILITY_TIMEOUT): Promise<void> {
	const enabled = await reaches(() => expect(target).toBeEnabled({ timeout }));
	test.skip(!enabled, reason); // skip-ok: decided by the wait above
}

/**
 * Waits until the page settles on `present` or on `absent` (an empty state, a getting-started guide),
 * whichever renders first, and skips the test with `reason` when it is `absent`. Neither rendering
 * fails the test: the page never settled.
 */
export async function skipIfShown(absent: Locator, present: Locator, reason: string, timeout = AVAILABILITY_TIMEOUT): Promise<void> {
	const shown = await new Promise<"present" | "absent">((resolve, reject) => {
		let pending = 2;
		const settle = (target: Locator, name: "present" | "absent") =>
			target
				.first()
				.waitFor({ state: "visible", timeout })
				.then(
					() => resolve(name),
					// The loser keeps waiting until its timeout; only both missing is an error.
					() => {
						pending -= 1;
						if (pending === 0) reject(new Error(`the page showed neither ${present} nor ${absent} within ${timeout}ms`));
					},
				);
		settle(present, "present");
		settle(absent, "absent");
	});
	test.skip(shown === "absent", reason); // skip-ok: decided by the wait above
}

/** Waits for `target` to match at least `min` elements; skips the test with `reason` if it never does. */
export async function skipUnlessCount(target: Locator, min: number, reason: string, timeout = AVAILABILITY_TIMEOUT): Promise<void> {
	const enough = await reaches(() => expect.poll(() => target.count(), { timeout }).toBeGreaterThanOrEqual(min));
	test.skip(!enough, reason); // skip-ok: decided by the wait above
}
