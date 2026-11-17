import type { APIRequestContext } from "@playwright/test";

/**
 * MCP logs for the MCP logs spec (TXE-7735). Offline the page had none, so it showed its
 * getting-started guide and ten tests skipped. Every UI run starts the local MCP test servers
 * (global-setup.ts: http-no-ping-server on :3001, with echo, add and greet), so the spec registers a
 * client for that server and executes its echo tool through the gateway, which logs each call.
 *
 * The page shows 50 logs a page (mcp-logs/page.tsx), so 60 give the pagination tests a second page.
 */
export const MCP_SEED_CLIENT = "e2e_mcp_logs_seed";
export const MCP_SEED_TOOL = `${MCP_SEED_CLIENT}-echo`;
export const MCP_SEED_COUNT = 60;
const MCP_TEST_SERVER = "http://localhost:3001/";

async function ok(response: { ok(): boolean; status(): number; text(): Promise<string> }, what: string) {
	if (!response.ok()) throw new Error(`${what} failed: ${response.status()} ${(await response.text()).slice(0, 300)}`);
}

type SeedClient = { config?: { client_id?: string; name?: string } };

async function findSeedClient(request: APIRequestContext): Promise<SeedClient | undefined> {
	const list = await request.get(`/api/mcp/clients?search=${MCP_SEED_CLIENT}`);
	await ok(list, "list MCP clients");
	const body = await list.json();
	const clients: SeedClient[] = Array.isArray(body) ? body : (body?.clients ?? []);
	return clients.find((c) => c.config?.name === MCP_SEED_CLIENT);
}

async function registerSeedClient(request: APIRequestContext) {
	if (await findSeedClient(request)) return;
	await ok(
		await request.post("/api/mcp/client", {
			data: {
				name: MCP_SEED_CLIENT,
				connection_type: "http",
				connection_string: { value: MCP_TEST_SERVER },
				auth_type: "none",
				is_ping_available: false,
				tools_to_execute: ["*"],
			},
		}),
		`register the ${MCP_SEED_CLIENT} MCP client`,
	);
}

// The logs outlive the client. Removing it keeps its tools out of the requests other specs on this
// worker's gateway make, and leaves the MCP registry as those specs expect it.
async function removeSeedClient(request: APIRequestContext) {
	const id = (await findSeedClient(request))?.config?.client_id;
	if (id) await ok(await request.delete(`/api/mcp/client/${id}`), `remove the ${MCP_SEED_CLIENT} MCP client`);
}

async function mcpLogTotal(request: APIRequestContext): Promise<number> {
	const response = await request.get(`/api/mcp-logs?limit=1&offset=0`);
	await ok(response, "read MCP logs");
	const body = await response.json();
	return Number(body?.pagination?.total_count ?? body?.total ?? body?.stats?.total_executions ?? 0);
}

/**
 * Makes sure the gateway has at least MCP_SEED_COUNT MCP logs: registers the seed client, executes
 * its echo tool for the shortfall, waits for the log writes to land (they are written
 * asynchronously), and removes the client again. `poll` waits on a condition; the spec passes expect.poll.
 */
export async function seedMCPLogs(request: APIRequestContext, poll: (read: () => Promise<number>) => Promise<void>) {
	const have = await mcpLogTotal(request);
	if (have >= MCP_SEED_COUNT) return;
	await registerSeedClient(request);
	const missing = MCP_SEED_COUNT - have;
	for (let start = 0; start < missing; start += 10) {
		await Promise.all(
			Array.from({ length: Math.min(10, missing - start) }, async (_, j) => {
				const i = start + j;
				await ok(
					await request.post("/v1/mcp/tool/execute", {
						data: {
							id: `e2e_mcp_seed_${i}`,
							type: "function",
							function: { name: MCP_SEED_TOOL, arguments: JSON.stringify({ message: `E2E MCP seed ${i + 1}` }) },
						},
					}),
					`execute ${MCP_SEED_TOOL}`,
				);
			}),
		);
	}
	await poll(() => mcpLogTotal(request));
	await removeSeedClient(request);
}
