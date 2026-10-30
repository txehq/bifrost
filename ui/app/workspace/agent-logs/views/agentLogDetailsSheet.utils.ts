import type { AgentLogOperation, AgentLogSummary } from "@/lib/types/agentLogs";

/** What the details sheet shows for the selected row. */
export interface SelectedOperationDetail {
	// The hydrated operation finished loading or failed: the sheet renders.
	ready: boolean;
	// The hydrated selected operation, when it loaded.
	detail: AgentLogOperation | undefined;
	// The record the sheet describes and Delete removes.
	displayLog: AgentLogSummary | null;
}

/**
 * Resolves the operation query for the selected row. Only currentData belongs
 * to the selected id: data keeps the previous operation while the next one
 * loads or after it fails, and showing it would describe, and put Delete on,
 * an operation other than the selected one. A failed load shows the selected
 * row's summary.
 */
export function selectedOperationDetail(
	log: AgentLogSummary | null | undefined,
	query: { currentData?: AgentLogOperation; isLoading: boolean; isError: boolean },
): SelectedOperationDetail {
	const fetched = query.currentData;
	const matches = Boolean(log && fetched && (fetched.id === log.id || (log.request_id && fetched.request_id === log.request_id)));
	const detail = log && matches ? fetched : undefined;
	const ready = Boolean(log) && (query.isError || (Boolean(detail) && !query.isLoading));
	return { ready, detail: ready ? detail : undefined, displayLog: log ? (ready && detail ? detail : log) : null };
}