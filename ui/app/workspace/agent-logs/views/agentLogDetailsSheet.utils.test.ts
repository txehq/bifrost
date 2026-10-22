import type { AgentLogOperation, AgentLogSummary } from "@/lib/types/agentLogs";
import { describe, expect, it } from "vitest";
import { selectedOperationDetail } from "./agentLogDetailsSheet.utils";

const summary = (id: string, requestId: string): AgentLogSummary => ({
	id,
	timestamp: "2026-10-12T00:00:00Z",
	record_kind: "request",
	operation: "SendMessage",
	status: "success",
	agent_name: "test-agent",
	request_id: requestId,
});
const hydrated = (log: AgentLogSummary): AgentLogOperation => ({ ...log, events: [{ ...log, id: `${log.id}-event` }] });

describe("selectedOperationDetail", () => {
	const a = summary("a", "request-a");
	const b = summary("b", "request-b");

	it("shows the selected summary, never the previous operation, when the selected one fails to load", () => {
		// RTK Query keeps the previous argument's result in data; currentData is the selected id's.
		const failed = { data: hydrated(a), currentData: undefined, isLoading: false, isError: true };
		const next = selectedOperationDetail(b, failed);
		expect(next.displayLog?.id).toBe("b");
		expect(next.detail).toBeUndefined();
		expect(next.ready).toBe(true);
	});

	it("waits for the selected operation instead of showing the previous one while it loads", () => {
		const loading = { data: hydrated(a), currentData: undefined, isLoading: false, isError: false };
		const next = selectedOperationDetail(b, loading);
		expect(next.ready).toBe(false);
		expect(next.displayLog?.id).toBe("b");
	});

	it("shows the hydrated operation once it loads, including one found by request id", () => {
		const loaded = selectedOperationDetail(b, { currentData: hydrated(b), isLoading: false, isError: false });
		expect(loaded.detail?.events[0].id).toBe("b-event");
		expect(loaded.displayLog).toBe(loaded.detail);
		const byRequest = { ...hydrated(b), id: "b-root" };
		expect(selectedOperationDetail(b, { currentData: byRequest, isLoading: false, isError: false }).detail?.id).toBe("b-root");
	});
});