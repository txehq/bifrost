import type { AgentLogOperation } from "@/lib/types/agentLogs";

export interface SessionContent {
	id?: string;
	key: string;
	role: "user" | "agent";
	kind: "message" | "status" | "artifact";
	parts: unknown[];
	state?: string;
	artifactName?: string;
	artifactDescription?: string;
	artifactFields?: Record<string, unknown>;
}

export function asRecord(value: unknown): Record<string, unknown> | null {
	return typeof value === "object" && value !== null && !Array.isArray(value) ? (value as Record<string, unknown>) : null;
}

export function parseProtocolBody(body: string | undefined): Record<string, unknown> | null {
	if (!body) return null;
	try {
		const parsed = asRecord(JSON.parse(body));
		return asRecord(parsed?.result) ?? parsed;
	} catch {
		return null;
	}
}

export function partsFromMessage(value: unknown): unknown[] | null {
	const message = asRecord(value);
	return message && Array.isArray(message.parts) ? message.parts : null;
}

interface AssembledArtifact {
	id?: string;
	key: string;
	parts: unknown[];
	name?: string;
	description?: string;
	fields?: Record<string, unknown>;
}

export function messageIdentity(message: Record<string, unknown>): string | null {
	return typeof message.messageId === "string" ? message.messageId : null;
}

function taskIdentity(task: Record<string, unknown>, fallback?: string): string | undefined {
	return typeof task.id === "string" ? task.id : typeof task.taskId === "string" ? task.taskId : fallback;
}

function artifactIdentity(taskId: string | undefined, artifact: Record<string, unknown>, fallback: string) {
	const artifactId = typeof artifact.artifactId === "string" ? artifact.artifactId : fallback;
	return `${taskId ?? "unknown-task"}:${artifactId}`;
}

export function buildSessionContent(log: AgentLogOperation): SessionContent[] {
	const content: SessionContent[] = [];
	const seenMessageIds = new Set<string>();
	const assembledArtifacts = new Map<string, AssembledArtifact>();
	const artifactOrder: string[] = [];

	const addMessage = (messageValue: unknown, key: string, role: "user" | "agent", kind: SessionContent["kind"], state?: string) => {
		const message = asRecord(messageValue);
		const parts = partsFromMessage(message);
		if (!message || !parts) return;
		const messageId = messageIdentity(message);
		if (messageId && seenMessageIds.has(messageId)) return;
		if (messageId) seenMessageIds.add(messageId);
		content.push({ id: messageId ?? undefined, key, role, kind, parts, state });
	};

	const setArtifact = (taskId: string | undefined, artifact: Record<string, unknown>, fallback: string, append: boolean) => {
		const identity = artifactIdentity(taskId, artifact, fallback);
		const incoming = Array.isArray(artifact.parts) ? artifact.parts : [];
		const existing = assembledArtifacts.get(identity);
		if (!existing) artifactOrder.push(identity);
		assembledArtifacts.set(identity, {
			key: identity,
			id: typeof artifact.artifactId === "string" ? artifact.artifactId : existing?.id,
			parts: append ? [...(existing?.parts ?? []), ...incoming] : incoming,
			name: typeof artifact.name === "string" ? artifact.name : existing?.name,
			description: typeof artifact.description === "string" ? artifact.description : existing?.description,
			fields: {
				...existing?.fields,
				...Object.fromEntries(Object.entries(artifact).filter(([key]) => !["artifactId", "name", "description", "parts"].includes(key))),
			},
		});
	};

	const addTaskSnapshot = (task: Record<string, unknown>, keyPrefix: string, fallbackTaskId?: string) => {
		const status = asRecord(task.status);
		addMessage(status?.message, `${keyPrefix}-status`, "agent", "status", typeof status?.state === "string" ? status.state : undefined);
		const taskId = taskIdentity(task, fallbackTaskId);
		if (Array.isArray(task.artifacts)) {
			task.artifacts.forEach((value, index) => {
				const artifact = asRecord(value);
				if (artifact) setArtifact(taskId, artifact, `${keyPrefix}-${index}`, false);
			});
		}
	};

	const processUpdate = (update: Record<string, unknown>, keyPrefix: string, fallbackTaskId?: string, fallbackArtifactId?: string) => {
		if (partsFromMessage(update)) {
			addMessage(update, `${keyPrefix}-message`, "agent", "message");
			return;
		}

		// A task snapshot carries a status and a contextId too, so it is told
		// apart by its own task id (a status update names its task in taskId)
		// before the status-update check, which would drop its artifacts.
		const task =
			asRecord(update.task) ?? (update.kind === "task" || (typeof update.id === "string" && update.taskId === undefined) ? update : null);
		if (task && (asRecord(task.status) || Array.isArray(task.artifacts))) {
			addTaskSnapshot(task, `${keyPrefix}-task`, fallbackTaskId);
			return;
		}

		const statusUpdate = asRecord(update.statusUpdate) ?? (asRecord(update.status) && (update.taskId || update.contextId) ? update : null);
		if (statusUpdate) {
			const status = asRecord(statusUpdate.status);
			addMessage(status?.message, `${keyPrefix}-status`, "agent", "status", typeof status?.state === "string" ? status.state : undefined);
			return;
		}

		const artifactUpdate = asRecord(update.artifactUpdate) ?? (asRecord(update.artifact) ? update : null);
		const artifact = asRecord(artifactUpdate?.artifact);
		if (artifactUpdate && artifact) {
			const taskId = typeof artifactUpdate.taskId === "string" ? artifactUpdate.taskId : fallbackTaskId;
			setArtifact(taskId, artifact, fallbackArtifactId ?? keyPrefix, artifactUpdate.append === true);
			return;
		}

		if (asRecord(update.status) || Array.isArray(update.artifacts)) addTaskSnapshot(update, `${keyPrefix}-task`, fallbackTaskId);
	};

	const isPushRelay = log.operation === "push_notification" || log.operation === "push_delivery";
	const request = parseProtocolBody(log.request_body);
	if (request) {
		if (isPushRelay) processUpdate(request, "push-update", log.task_id, log.artifact_id);
		else {
			const requestMessage = asRecord(request.message) ?? asRecord(asRecord(request.params)?.message);
			addMessage(requestMessage, "request-message", "user", "message");
		}
	}

	const response = parseProtocolBody(log.response_body);
	if (response && !isPushRelay) {
		if (partsFromMessage(response)) addMessage(response, "response-message", "agent", "message");
		else addTaskSnapshot(response, "response-task", log.task_id);
	}

	const event = parseProtocolBody(log.event_body);
	if (event) processUpdate(event, `event-${log.event_sequence ?? log.id}`, log.task_id, log.artifact_id);
	log.events.forEach((eventLog) => {
		const eventBody = parseProtocolBody(eventLog.event_body);
		if (eventBody) {
			processUpdate(eventBody, `event-${eventLog.event_sequence ?? eventLog.id}`, eventLog.task_id, eventLog.artifact_id);
		}
	});

	artifactOrder.forEach((identity) => {
		const artifact = assembledArtifacts.get(identity);
		if (!artifact) return;
		content.push({
			id: artifact.id,
			key: `artifact-${artifact.key}`,
			role: "agent",
			kind: "artifact",
			parts: artifact.parts,
			artifactName: artifact.name,
			artifactDescription: artifact.description,
			artifactFields: artifact.fields,
		});
	});

	return content;
}