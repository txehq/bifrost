import type { AgentLogDetail, AgentLogOperation } from "@/lib/types/agentLogs";
import { describe, expect, it } from "vitest";
import { buildSessionContent } from "./agentContextConversation";

const base: AgentLogDetail = {
	id: "op-1",
	timestamp: "2026-10-12T00:00:00Z",
	record_kind: "request",
	operation: "SendStreamingMessage",
	status: "success",
	agent_name: "writer",
	request_id: "request-1",
	task_id: "task-1",
	context_id: "context-1",
};

const operation = (events: Record<string, unknown>[]): AgentLogOperation => ({
	...base,
	events: events.map((body, index) => ({
		...base,
		id: `event-${index}`,
		record_kind: "event",
		event_sequence: index,
		event_body: JSON.stringify(body),
	})),
});

describe("buildSessionContent", () => {
	it("assembles the artifacts of a streamed task snapshot that also carries a status", () => {
		const content = buildSessionContent(
			operation([
				{
					id: "task-1",
					contextId: "context-1",
					status: { state: "completed" },
					artifacts: [{ artifactId: "report", name: "Report", parts: [{ text: "final answer" }] }],
				},
			]),
		);
		const artifact = content.find((item) => item.kind === "artifact");
		expect(artifact?.parts).toEqual([{ text: "final answer" }]);
		expect(artifact?.artifactName).toBe("Report");
	});

	it("assembles a task snapshot wrapped in a stream response", () => {
		const content = buildSessionContent(
			operation([
				{
					task: {
						id: "task-1",
						contextId: "context-1",
						status: { state: "completed" },
						artifacts: [{ artifactId: "a", parts: [{ text: "x" }] }],
					},
				},
			]),
		);
		expect(content.find((item) => item.kind === "artifact")?.parts).toEqual([{ text: "x" }]);
	});

	it("still treats a status update as status only", () => {
		const content = buildSessionContent(
			operation([
				{
					taskId: "task-1",
					contextId: "context-1",
					status: { state: "working", message: { messageId: "m-1", role: "agent", parts: [{ text: "thinking" }] } },
				},
			]),
		);
		expect(content.map((item) => item.kind)).toEqual(["status"]);
		expect(content[0].state).toBe("working");
	});
});