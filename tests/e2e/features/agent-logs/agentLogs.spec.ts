import { expect, test } from "../../core/fixtures/base.fixture";

// History is mocked so grouping tests do not require creating gateway traffic.
const root = {
  id: "context-root",
  timestamp: "2026-10-12T00:00:00Z",
  record_kind: "request",
  operation: "SendMessage",
  status: "success",
  agent_name: "group-test-agent",
  request_id: "request-root",
  context_id: "context-group-test",
  input: "group root",
  session_child_count: 2,
};
const firstMember = {
  ...root,
  id: "context-member-1",
  request_id: "request-member-1",
  operation: "GetTask",
  input: "first member",
  session_child_count: 0,
};
const secondMember = {
  ...root,
  id: "context-member-2",
  request_id: "request-member-2",
  operation: "CancelTask",
  input: "second member",
  session_child_count: 0,
};

test.describe("Agent log context grouping", () => {
  test.beforeEach(async ({ page }) => {
    await page.route("**/api/agents/history**", async (route) => {
      const url = new URL(route.request().url());
      if (url.pathname.endsWith("/stats")) {
        return route.fulfill({
          json: {
            total_entries: 3,
            success_count: 3,
            error_count: 0,
            success_rate: 100,
            average_latency: 0,
          },
        });
      }
      if (url.pathname.endsWith("/histogram"))
        return route.fulfill({ json: { buckets: [], bucket_size_seconds: 60 } });
      if (url.pathname.endsWith("/filterdata")) return route.fulfill({ json: {} });
      if (!url.pathname.endsWith("/history")) return route.continue();
      const params = url.searchParams;
      const offset = Number(params.get("offset") ?? 0);
      const grouped = params.get("group_sessions") === "true";
      const expanding = params.has("context_id");
      const logs = expanding
        ? offset === 0
          ? [root, firstMember]
          : [secondMember]
        : grouped
          ? [root]
          : [root, firstMember, secondMember];
      await route.fulfill({
        json: {
          logs,
          pagination: {
            limit: Number(params.get("limit")),
            offset,
            sort_by: "timestamp",
            order: params.get("order"),
            total_count: grouped ? 1 : 3,
          },
        },
      });
    });
    await page.goto("/workspace/agent-logs?polling=false&period=30d&status=success");
    await expect(page.getByTestId("agent-logs-header-group-btn")).toBeVisible();
  });

  test("toggles grouping, expands matching operations, and loads further members", async ({
    page,
  }) => {
    const group = page.getByTestId("agent-logs-header-group-btn");
    await expect(page.getByText("first member", { exact: true })).toBeVisible();
    await group.click();
    await expect(group).toHaveAttribute("aria-pressed", "true");
    await expect(page).toHaveURL(/grouped=true/);
    await expect(page.getByText("first member", { exact: true })).toBeHidden();
    await expect(page.getByText("3 ops", { exact: true })).toBeVisible();

    const expansionRequest = page.waitForRequest((request) =>
      new URL(request.url()).searchParams.has("context_id"),
    );
    await page.getByTestId("agent-logs-session-expand-btn").click();
    const params = new URL((await expansionRequest).url()).searchParams;
    expect(params.get("context_id")).toBe(root.context_id);
    expect(params.get("status")).toBe("success");
    expect(params.has("group_sessions")).toBe(false);
    await expect(page.getByText("first member", { exact: true })).toBeVisible();
    await expect(page.getByTestId("agent-log-row-kind-session")).toHaveText("operation 2");
    await expect(page.getByText("second member", { exact: true })).toBeHidden();
    await page.getByTestId("agent-logs-session-load-more-btn").click();
    await expect(page.getByText("second member", { exact: true })).toBeVisible();
    await expect(page.getByTestId("agent-logs-session-load-more-btn")).toBeHidden();

    await page.getByTestId("agent-logs-session-expand-btn").click();
    await expect(page.getByText("first member", { exact: true })).toBeHidden();
    await group.click();
    await expect(group).toHaveAttribute("aria-pressed", "false");
    await expect(page.getByText("first member", { exact: true })).toBeVisible();
  });
  test("refresh retains cached expanded members", async ({ page }) => {
    await page.getByTestId("agent-logs-header-group-btn").click();
    await page.getByTestId("agent-logs-session-expand-btn").click();
    await expect(page.getByText("first member", { exact: true })).toBeVisible();
    await page.route("**/api/agents/history?**", async route => {
      const params = new URL(route.request().url()).searchParams;
      if (!params.has("context_id")) return route.fallback();
      await route.fulfill({json: {
        logs: [root, {...secondMember, input: "fresh member"}],
        pagination: {limit: 100, offset: 0, sort_by: "timestamp", order: "asc", total_count: 2},
      }});
    });
    await page.getByTestId("agent-logs-header-refresh-btn").click();
    await expect(page.getByText("fresh member", {exact: true})).toBeHidden();
    await expect(page.getByText("first member", {exact: true})).toBeVisible();
    await page.getByTestId("agent-logs-session-expand-btn").click();
    await page.getByTestId("agent-logs-session-expand-btn").click();
    await expect(page.getByText("fresh member", {exact: true})).toBeHidden();
  });

});

// The history is mocked: the recent probe, the selected range and each hydrated operation are
// answered independently, so an empty probe can sit beside a range that has rows.
test.describe("Agent log history state", () => {
  const log = (id: string, input: string) => ({
    id,
    timestamp: "2026-10-12T00:00:00Z",
    record_kind: "request",
    operation: "SendMessage",
    status: "success",
    agent_name: "state-test-agent",
    request_id: `request-${id}`,
    input,
    session_child_count: 0,
  });
  const alpha = log("op-alpha", "alpha input");
  const beta = log("op-beta", "beta input");

  async function mockHistory(
    page: import("@playwright/test").Page,
    rows: Record<string, unknown>[],
    {
      probeRows = rows,
      operation = (id) => ({ status: 200, json: { ...alpha, id, events: [] } }),
    }: {
      probeRows?: Record<string, unknown>[];
      operation?: (id: string) => { status: number; json: unknown };
    } = {},
  ) {
    const deleted: string[][] = [];
    await page.route("**/api/agents/history**", async (route) => {
      const request = route.request();
      const url = new URL(request.url());
      if (request.method() === "DELETE") {
        deleted.push((request.postDataJSON() as { ids: string[] }).ids);
        return route.fulfill({ json: { message: "deleted" } });
      }
      if (url.pathname.endsWith("/stats"))
        return route.fulfill({ json: { total_entries: rows.length, success_count: rows.length, error_count: 0, success_rate: 100, average_latency: 0 } });
      if (url.pathname.endsWith("/histogram")) return route.fulfill({ json: { buckets: [], bucket_size_seconds: 60 } });
      if (url.pathname.endsWith("/filterdata") || url.pathname.endsWith("/top-agents")) return route.fulfill({ json: {} });
      if (!url.pathname.endsWith("/history")) {
        const { status, json } = operation(decodeURIComponent(url.pathname.split("/").pop() ?? ""));
        return route.fulfill({ status, json });
      }
      const params = url.searchParams;
      // The probe is the only one-row request; a period reaches the server as timestamps.
      const probe = params.get("limit") === "1";
      const logs = probe ? probeRows : rows;
      await route.fulfill({
        json: {
          logs,
          pagination: { limit: Number(params.get("limit")), offset: 0, sort_by: "timestamp", order: "desc", total_count: logs.length },
        },
      });
    });
    return deleted;
  }

  test("keeps the history view when the 30-day probe is empty but the selected range has rows", async ({ page }) => {
    await mockHistory(page, [alpha], { probeRows: [] });
    await page.goto("/workspace/agent-logs?polling=false&period=this_month");
    await expect(page.getByTestId("agent-logs-header-group-btn")).toBeVisible();
    await expect(page.getByText("alpha input")).toBeVisible();
  });

  test("an empty absolute range keeps its date controls", async ({ page }) => {
    await mockHistory(page, []);
    await page.goto("/workspace/agent-logs?polling=false&period=&start_time=1700000000&end_time=1700003600");
    await expect(page.getByTestId("agent-logs-header-group-btn")).toBeVisible();
  });

  test("a failed detail fetch shows the selected operation and Delete removes that one", async ({ page }) => {
    const deleted = await mockHistory(page, [alpha, beta], {
      operation: (id) =>
        id === alpha.id ? { status: 200, json: { ...alpha, events: [] } } : { status: 500, json: { error: { message: "boom" } } },
    });
    await page.goto(`/workspace/agent-logs?polling=false&period=30d&selected_log=${alpha.id}`);
    await expect(page.getByRole("button", { name: "Log actions" })).toBeVisible();
    await page.keyboard.press("ArrowDown");
    await expect(page).toHaveURL(new RegExp(`selected_log=${beta.id}`));

    await page.getByRole("button", { name: "Log actions" }).click();
    await page.getByRole("menuitem", { name: "Delete log" }).click();
    await page.getByRole("alertdialog").getByRole("button", { name: /delete/i }).click();

    await expect.poll(() => deleted).toEqual([[beta.id]]);
  });

  test("assembles the artifacts of a streamed task snapshot in the session view", async ({ page }) => {
    const operation = {
      ...alpha,
      id: "op-stream",
      operation: "SendStreamingMessage",
      task_id: "task-1",
      context_id: "context-1",
      events: [
        {
          ...alpha,
          id: "event-1",
          record_kind: "event",
          task_id: "task-1",
          context_id: "context-1",
          event_sequence: 0,
          event_body: JSON.stringify({
            id: "task-1",
            contextId: "context-1",
            status: { state: "completed" },
            artifacts: [{ artifactId: "report", name: "Report", parts: [{ text: "final answer" }] }],
          }),
        },
      ],
    };
    await mockHistory(page, [operation]);
    await page.goto("/workspace/agent-logs?polling=false&period=30d&selected_session=context-1");
    await expect(page.getByText("final answer")).toBeVisible();
  });
});
