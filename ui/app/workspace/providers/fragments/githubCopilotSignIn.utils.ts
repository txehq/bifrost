// Polling bounds for "Sign in with GitHub" (GitHub's OAuth device flow).

// GitHub issues device codes for 15 minutes. A missing or non-positive expires_in falls back to
// that, and a value above 30 minutes is capped, so a bad reply cannot keep the form polling.
export function signInLifetimeSeconds(expiresIn: number | undefined): number {
	return expiresIn && expiresIn > 0 ? Math.min(expiresIn, 1800) : 900;
}

// pollFitsBeforeDeadline reports whether the next poll, intervalSeconds from now, still lands
// before the code's deadline (epoch milliseconds). Past it the form stops and asks to start again.
export function pollFitsBeforeDeadline(nowMs: number, intervalSeconds: number, deadlineMs: number): boolean {
	return nowMs + intervalSeconds * 1000 <= deadlineMs;
}
