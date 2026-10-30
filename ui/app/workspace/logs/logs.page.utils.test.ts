import { describe, expect, it } from "vitest";
import { fetchAllPages, pageFromQuery } from "./logs.page.utils";

const pagedSource = (total: number) => {
	const offsets: number[] = [];
	const fetchPage = async (offset: number, limit: number) => {
		offsets.push(offset);
		return { logs: Array.from({ length: Math.max(0, Math.min(limit, total - offset)) }, (_, i) => offset + i) };
	};
	return { offsets, fetchPage };
};

describe("fetchAllPages", () => {
	it("lists every session member past the first page", async () => {
		const source = pagedSource(2_001);
		const result = await fetchAllPages((offset) => source.fetchPage(offset, 1_000), 1_000);
		expect("logs" in result && result.logs.length).toBe(2_001);
		expect(source.offsets).toEqual([0, 1_000, 2_000]);
	});

	it("stops after a short page, and after an empty page when the total is a multiple of the limit", async () => {
		const short = pagedSource(10);
		await fetchAllPages((offset) => short.fetchPage(offset, 1_000), 1_000);
		expect(short.offsets).toEqual([0]);
		const exact = pagedSource(2_000);
		const result = await fetchAllPages((offset) => exact.fetchPage(offset, 1_000), 1_000);
		expect("logs" in result && result.logs.length).toBe(2_000);
		expect(exact.offsets).toEqual([0, 1_000, 2_000]);
	});

	it("reports a failed page instead of a partial list", async () => {
		let calls = 0;
		const result = await fetchAllPages(async (offset) => {
			calls++;
			return offset === 0 ? { logs: Array.from({ length: 1_000 }, (_, i) => i) } : { error: "boom" };
		}, 1_000);
		expect(result).toEqual({ error: "boom" });
		expect(calls).toBe(2);
	});
});

describe("pageFromQuery", () => {
	it("reads the rows of a successful page", () => {
		expect(pageFromQuery({ data: { logs: [1, 2] } })).toEqual({ logs: [1, 2] });
	});

	it("reports a failed refetch even though the cache still holds rows", () => {
		expect(pageFromQuery({ data: { logs: [1] }, error: "boom" })).toEqual({ error: "boom" });
	});

	it("reports a result with neither rows nor an error as a failure", () => {
		expect("error" in pageFromQuery({})).toBe(true);
	});
});
