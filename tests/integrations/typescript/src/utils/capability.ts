/**
 * Decision-matrix capability rejections.
 *
 * Split providers (Perplexity's sonar) reject some decision schemas outright.
 * Only that documented limit, a 400 schema rejection carrying an "invalid
 * request" marker, is a capability SKIP; any other status or error from the same
 * provider is a defect. Mirrors the Python suite's _is_capability_rejection.
 */

export const CAPABILITY_REJECTION_MARKERS = ["invalid request", "invalid_request"];

export function isCapabilityRejection(splitProviders: ReadonlySet<string>, provider: string, status: number, text: string): boolean {
	if (!splitProviders.has(provider) || status !== 400) return false;
	const low = text.toLowerCase();
	return CAPABILITY_REJECTION_MARKERS.some((marker) => low.includes(marker));
}
