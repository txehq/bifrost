/**
 * Decision-matrix capability rejections.
 *
 * Split providers (Perplexity's sonar) reject some decision schemas outright:
 * choice and score questions, answered one at a time, come back 400 "invalid
 * request". Only that demonstrated limit is a capability SKIP: the rejection of a
 * choice or score question in the one-question-at-a-time split, carrying the
 * marker. The marker alone is a generic invalid-request error, which a request we
 * built wrong also carries, so any other status, question kind or call is a
 * defect. Mirrors the Python suite's _is_capability_rejection.
 */

export const CAPABILITY_REJECTION_MARKERS = ["invalid request", "invalid_request"];

/** The question kinds the split providers are documented to reject. */
export const CAPABILITY_REJECTED_KINDS = ["choice", "score"];

export function isCapabilityRejection(
	splitProviders: ReadonlySet<string>,
	provider: string,
	status: number,
	text: string,
	rejectedKind?: string,
): boolean {
	if (!splitProviders.has(provider) || status !== 400) return false;
	if (!rejectedKind || !CAPABILITY_REJECTED_KINDS.includes(rejectedKind)) return false;
	const low = text.toLowerCase();
	return CAPABILITY_REJECTION_MARKERS.some((marker) => low.includes(marker));
}
