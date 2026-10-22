type PageResult<T> = { logs: T[] } | { error: unknown };

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