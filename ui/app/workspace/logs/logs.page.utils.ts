type PageResult<T> = { logs: T[] } | { error: unknown };

/**
 * One page from a lazy query's result. A failed refetch keeps the cached rows
 * in data, so the error decides: reading them would pass off an old page as
 * this one.
 */
export function pageFromQuery<T>(result: { data?: { logs: T[] }; error?: unknown }): PageResult<T> {
	return result.data && !result.error ? { logs: result.data.logs } : { error: result.error };
}

/**
 * Reads a list endpoint page by page until a short page, so an expansion lists
 * every row instead of silently stopping at the endpoint's page limit. Any
 * failed page fails the whole read rather than returning a partial list.
 */
export async function fetchAllPages<T>(fetchPage: (offset: number) => Promise<PageResult<T>>, pageLimit: number): Promise<PageResult<T>> {
	const logs: T[] = [];
	for (let offset = 0; ; offset += pageLimit) {
		const page = await fetchPage(offset);
		if ("error" in page) return page;
		logs.push(...page.logs);
		if (page.logs.length < pageLimit) return { logs };
	}
}