import { describe, expect, it } from "vitest";
import { pollFitsBeforeDeadline, signInLifetimeSeconds } from "./githubCopilotSignIn.utils";

describe("GitHub Copilot sign-in polling bounds", () => {
	it("uses GitHub's expires_in, falls back to 15 minutes, and caps at 30", () => {
		expect(signInLifetimeSeconds(900)).toBe(900);
		expect(signInLifetimeSeconds(600)).toBe(600);
		expect(signInLifetimeSeconds(undefined)).toBe(900);
		expect(signInLifetimeSeconds(0)).toBe(900);
		expect(signInLifetimeSeconds(-5)).toBe(900);
		expect(signInLifetimeSeconds(86400)).toBe(1800);
	});

	it("stops polling once the next poll would land past the deadline", () => {
		const start = 1_000_000;
		const deadline = start + signInLifetimeSeconds(900) * 1000;
		expect(pollFitsBeforeDeadline(start, 5, deadline)).toBe(true);
		expect(pollFitsBeforeDeadline(deadline - 5000, 5, deadline)).toBe(true);
		expect(pollFitsBeforeDeadline(deadline - 4999, 5, deadline)).toBe(false);
		// slow_down raises the interval; a longer wait can no longer fit near the end.
		expect(pollFitsBeforeDeadline(deadline - 8000, 10, deadline)).toBe(false);
	});
});
