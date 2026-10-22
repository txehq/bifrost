// The sentinel "no filter" is stored as; the charts aggregate every provider for it.
export const ALL_PROVIDERS_VALUE = "all";

// The selector emits "" when its selected row is clicked again. That is no
// filter, so it is stored as the sentinel: an empty provider name would show
// "All Providers" while the charts look up a provider called "" and draw zeros.
export function normalizeProviderFilter(provider: string): string {
	return provider || ALL_PROVIDERS_VALUE;
}