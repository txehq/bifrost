import { describe, expect, it } from "vitest";
import { showAgentLogsOnboarding } from "./agentLogs.page.utils";

describe("showAgentLogsOnboarding", () => {
	const empty = { probeLoaded: true, probeTotal: 0, pageTotal: 0, period: "1h" };

	it("shows onboarding only when the recent probe and the selected relative period are both empty", () => {
		expect(showAgentLogsOnboarding(empty)).toBe(true);
		expect(showAgentLogsOnboarding({ ...empty, probeLoaded: false })).toBe(false);
		expect(showAgentLogsOnboarding({ ...empty, probeTotal: 1 })).toBe(false);
	});

	it("keeps the history view for logs older than the probe's 30 days", () => {
		// An absolute range reaching past the probe window returned results.
		expect(showAgentLogsOnboarding({ ...empty, period: "", pageTotal: 3 })).toBe(false);
		// This month can start more than 30 days ago.
		expect(showAgentLogsOnboarding({ ...empty, period: "this_month", pageTotal: 1 })).toBe(false);
		// An explicit range keeps its date controls even when it is empty.
		expect(showAgentLogsOnboarding({ ...empty, period: "" })).toBe(false);
	});
});