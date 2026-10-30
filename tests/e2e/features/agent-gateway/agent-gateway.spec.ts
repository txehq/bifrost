import type { APIRequestContext } from "@playwright/test";
import { expect, test } from "../../core/fixtures/base.fixture";
import {
  FIXTURE_CA_CERT_PEM,
  FIXTURE_CARD_HEADER,
  FIXTURE_RUNTIME_HEADER,
  FixtureAgentServer,
  uniqueAgentName,
} from "./agent-gateway.data";

/**
 * Agent Gateway management flows, driven through the UI against a loopback
 * fixture agent the test itself serves (agent-gateway.data.ts). The fixture
 * records the headers of every card fetch, so the tests can tell which
 * credential the gateway actually used: the secret typed into the form, or the
 * masked placeholder every management read serves and an edit hands back.
 *
 * The tests build on one registration, so they run in order.
 */

interface SecretVar {
  value: string;
  ref?: string;
}

interface AgentView {
  name: string;
  agent_card_url: string;
  enabled: boolean;
  allow_by_default: boolean;
  discovery_auth?: { type: string; headers?: Record<string, SecretVar>; tls?: { ca_cert_pem?: SecretVar } };
  runtime_auth?: {
    type: string;
    headers?: Record<string, SecretVar>;
    security_schemes?: string[];
    tls?: { ca_cert_pem?: SecretVar };
  };
  virtual_key_ids?: string[];
}

const CARD_SECRET = "card-fetch-secret-1";
const RUNTIME_SECRET = "agent-request-secret-1";

async function getAgent(request: APIRequestContext, name: string): Promise<AgentView> {
  const response = await request.get(`/api/agents/${encodeURIComponent(name)}`);
  expect(response.ok(), `GET /api/agents/${name}: ${response.status()}`).toBe(true);
  return (await response.json()) as AgentView;
}

async function createVirtualKey(request: APIRequestContext, name: string): Promise<{ id: string; name: string }> {
  const response = await request.post("/api/governance/virtual-keys", { data: { name } });
  expect(response.ok(), `POST /api/governance/virtual-keys: ${response.status()}`).toBe(true);
  const body = (await response.json()) as { virtual_key: { id: string; name: string } };
  return { id: body.virtual_key.id, name: body.virtual_key.name };
}

test.describe("Agent Gateway", () => {
  test.describe.configure({ mode: "serial" });

  const fixture = new FixtureAgentServer();
  const agentName = uniqueAgentName();
  const virtualKeys: { id: string; name: string }[] = [];

  test.beforeAll(async () => {
    await fixture.start();
  });

  // The first-run onboarding checklist is a fixed card over the bottom-right
  // corner, where the sheet's Save button sits. It snoozes itself with this
  // cookie (ui/hooks/useOnboardingChecklist.ts), decided at render time.
  test.beforeEach(async ({ context, baseURL }) => {
    await context.addCookies([
      {
        name: "bifrost_onboarding_remind_at",
        value: new Date(Date.now() + 24 * 60 * 60 * 1000).toISOString(),
        url: baseURL ?? "http://localhost:3000",
      },
    ]);
  });

  test.afterAll(async ({ playwright }) => {
    const { baseURL, extraHTTPHeaders } = test.info().project.use;
    const request = await playwright.request.newContext({ baseURL, extraHTTPHeaders });
    try {
      await request.delete(`/api/agents/${encodeURIComponent(agentName)}`);
      for (const virtualKey of virtualKeys) {
        await request.delete(`/api/governance/virtual-keys/${virtualKey.id}`);
      }
    } finally {
      await request.dispose();
      await fixture.stop();
    }
  });

  test("registers an agent from its fetched card with its credentials", async ({ agentGatewayPage, request }) => {
    const grantee = await createVirtualKey(request, `e2e agent grant ${Date.now()}`);
    virtualKeys.push(grantee);

    await agentGatewayPage.goto();
    await agentGatewayPage.openCreateSheet();
    await agentGatewayPage.nameInput.fill(agentName);
    await agentGatewayPage.cardUrlInput.fill(fixture.cardUrl);

    // The card fetch carries its own credential, never the agent-request one.
    await agentGatewayPage.selectCardAuthType("Headers");
    await agentGatewayPage.setCardHeader(0, FIXTURE_CARD_HEADER, CARD_SECRET);
    await agentGatewayPage.fetchCard();
    await expect(agentGatewayPage.inspection).toContainText("e2e-fixture-agent");
    await expect(agentGatewayPage.inspection).toContainText("Fetched");
    expect(fixture.cardHeaderValues(FIXTURE_CARD_HEADER).at(-1)).toBe(CARD_SECRET);

    // The card advertises an apiKey header; the form asks for exactly that header.
    await agentGatewayPage.fillRequiredRuntimeHeader(FIXTURE_RUNTIME_HEADER, RUNTIME_SECRET);
    await agentGatewayPage.setRuntimeCaCert(FIXTURE_CA_CERT_PEM);
    await agentGatewayPage.grantVirtualKey(grantee.name);

    await agentGatewayPage.save();
    await agentGatewayPage.waitForSuccessToast("Agent registered successfully");
    await agentGatewayPage.expectSheetClosed();
    await expect(agentGatewayPage.statusCell(agentName)).toBeVisible();
    await expect(agentGatewayPage.accessCell(agentName)).toContainText("1 VK");

    // Building the runtime fetched the card again, with the typed secret.
    expect(fixture.cardHeaderValues(FIXTURE_CARD_HEADER).at(-1)).toBe(CARD_SECRET);

    const agent = await getAgent(request, agentName);
    expect(agent.enabled).toBe(true);
    expect(agent.agent_card_url).toBe(fixture.cardUrl);
    expect(agent.discovery_auth?.type).toBe("headers");
    expect(agent.discovery_auth?.headers?.[FIXTURE_CARD_HEADER]?.value).toBe("<REDACTED>");
    expect(agent.runtime_auth?.type).toBe("headers");
    expect(agent.runtime_auth?.security_schemes).toEqual(["apiKey"]);
    expect(agent.runtime_auth?.headers?.[FIXTURE_RUNTIME_HEADER]?.value).toBe("<REDACTED>");
    expect(agent.runtime_auth?.tls?.ca_cert_pem?.value).toBe("<REDACTED>");
    expect(agent.virtual_key_ids).toEqual([grantee.id]);
  });

  test("edits a registration without re-entering its masked credentials", async ({ agentGatewayPage, request }) => {
    const fetchesBefore = fixture.requests.length;

    await agentGatewayPage.goto();
    await agentGatewayPage.openEditSheet(agentName);
    await expect(agentGatewayPage.nameInput).toBeDisabled();
    await expect(agentGatewayPage.allowByDefaultSwitch).toHaveAttribute("data-state", "unchecked");
    await agentGatewayPage.allowByDefaultSwitch.click();
    await expect(agentGatewayPage.allowByDefaultSwitch).toHaveAttribute("data-state", "checked");

    // The form hands back every masked credential and certificate untouched.
    await agentGatewayPage.save();
    await agentGatewayPage.waitForSuccessToast("Agent updated successfully");
    await agentGatewayPage.expectSheetClosed();
    await expect(agentGatewayPage.accessCell(agentName)).toContainText("Allowed by default");

    // The rebuild fetched the card with the stored secret, not the placeholder.
    expect(fixture.requests.length).toBeGreaterThan(fetchesBefore);
    expect(fixture.cardHeaderValues(FIXTURE_CARD_HEADER).at(-1)).toBe(CARD_SECRET);

    const agent = await getAgent(request, agentName);
    expect(agent.allow_by_default).toBe(true);
    expect(agent.discovery_auth?.headers?.[FIXTURE_CARD_HEADER]?.value).toBe("<REDACTED>");
    expect(agent.runtime_auth?.headers?.[FIXTURE_RUNTIME_HEADER]?.value).toBe("<REDACTED>");
    expect(agent.runtime_auth?.tls?.ca_cert_pem?.value).toBe("<REDACTED>");
    expect(agent.virtual_key_ids).toEqual([virtualKeys[0].id]);
  });

  test("grants and revokes virtual key access", async ({ agentGatewayPage, request }) => {
    const second = await createVirtualKey(request, `e2e agent grant ${Date.now()} b`);
    virtualKeys.push(second);

    await agentGatewayPage.goto();
    await agentGatewayPage.openEditSheet(agentName);
    // The previous test allowed every key; with that on the sheet hides the grant
    // list and disables adding, so switch it back off first.
    await agentGatewayPage.allowByDefaultSwitch.click();
    await expect(agentGatewayPage.allowByDefaultSwitch).toHaveAttribute("data-state", "unchecked");
    await expect(agentGatewayPage.virtualKeysList).toContainText(virtualKeys[0].name);
    await agentGatewayPage.grantVirtualKey(second.name);
    await agentGatewayPage.save();
    await agentGatewayPage.waitForSuccessToast("Agent updated successfully");
    await agentGatewayPage.expectSheetClosed();

    let agent = await getAgent(request, agentName);
    expect([...(agent.virtual_key_ids ?? [])].sort()).toEqual([virtualKeys[0].id, second.id].sort());

    await agentGatewayPage.openEditSheet(agentName);
    await agentGatewayPage.revokeVirtualKey(virtualKeys[0].id);
    await agentGatewayPage.revokeVirtualKey(second.id);
    await expect(agentGatewayPage.virtualKeysEmpty).toBeVisible();
    await agentGatewayPage.save();
    await agentGatewayPage.waitForSuccessToast("Agent updated successfully");
    await agentGatewayPage.expectSheetClosed();

    agent = await getAgent(request, agentName);
    expect(agent.virtual_key_ids ?? []).toEqual([]);
    await agentGatewayPage.openEditSheet(agentName);
    await expect(agentGatewayPage.virtualKeysEmpty).toBeVisible();
    await agentGatewayPage.cancelBtn.click();
    await agentGatewayPage.expectSheetClosed();
  });
});

// What the card describes follows the card, and what only the operator can supply survives a
// refetch. The registration and the card are mocked, so these need no running upstream or
// identity provider: the PUT body is what the gateway would have stored.
test.describe("Agent Gateway settings the operator owns", () => {
  const name = "oauth-agent";
  const registration = (advanced: boolean) => ({
    name,
    agent_card_url: "https://agent.example.test/.well-known/agent-card.json",
    enabled: true,
    allow_by_default: false,
    forward_accepted_credential: false,
    forward_accepted_credential_overrides_auth: false,
    runtime_auth: {
      type: "oauth",
      advanced: advanced || undefined,
      security_schemes: ["oidc"],
      oauth: {
        token_url: "https://operator.example.test/token",
        client_id: { value: "<REDACTED>", ref: "" },
        client_secret: { value: "<REDACTED>", ref: "" },
        scopes: ["operator.scope"],
        resource: "https://api.example.test/agents",
        discovery_url: "https://idp.example.test/.well-known/openid-configuration",
      },
    },
    credential_origins: ["https://runtime.example.test"],
    virtual_key_ids: [],
    created_at: "2026-10-01T00:00:00Z",
    updated_at: "2026-10-01T00:00:00Z",
  });
  const inspectedCard = {
    name: "mock-agent",
    capabilities: {},
    signature_count: 0,
    security_schemes: [{ name: "oidc", type: "oauth2", token_url: "https://card.example.test/token", scopes: ["agent.read"] }],
    security_requirements: [["oidc"]],
  };

  async function mockRegistration(page: import("@playwright/test").Page, agent: ReturnType<typeof registration>) {
    const saved: Record<string, any>[] = [];
    await page.route("**/api/agents", (route) =>
      route.request().method() === "GET" ? route.fulfill({ json: { agents: [agent], count: 1, total_count: 1 } }) : route.continue(),
    );
    await page.route("**/api/agents/inspect", (route) => route.fulfill({ json: inspectedCard }));
    await page.route(`**/api/agents/${name}`, async (route) => {
      if (route.request().method() !== "PUT") return route.continue();
      saved.push(route.request().postDataJSON());
      await route.fulfill({ json: agent });
    });
    return saved;
  }

  test.beforeEach(async ({ context, baseURL }) => {
    await context.addCookies([
      {
        name: "bifrost_onboarding_remind_at",
        value: new Date(Date.now() + 24 * 60 * 60 * 1000).toISOString(),
        url: baseURL ?? "http://localhost:3000",
      },
    ]);
  });

  test("a card refetch takes the token URL and scopes from the card and keeps the operator's OAuth settings", async ({
    agentGatewayPage,
  }) => {
    const saved = await mockRegistration(agentGatewayPage.page, registration(false));
    await agentGatewayPage.goto();
    await agentGatewayPage.openEditSheet(name);
    await agentGatewayPage.fetchCardBtn.first().click();
    await expect(agentGatewayPage.inspection).toContainText("mock-agent");
    await agentGatewayPage.save();

    await expect.poll(() => saved.length).toBe(1);
    const oauth = saved[0].runtime_auth.oauth;
    expect(oauth.token_url).toBe("https://card.example.test/token");
    expect(oauth.scopes).toEqual(["agent.read"]);
    expect(oauth.resource).toBe("https://api.example.test/agents");
    expect(oauth.discovery_url).toBe("https://idp.example.test/.well-known/openid-configuration");
    expect(oauth.client_id.value).toBe("<REDACTED>");
    expect(oauth.client_secret.value).toBe("<REDACTED>");
  });

  test("a card refetch shows the card without replacing a manual configuration", async ({ agentGatewayPage }) => {
    const agent = registration(true);
    const saved = await mockRegistration(agentGatewayPage.page, agent);
    await agentGatewayPage.goto();
    await agentGatewayPage.openEditSheet(name);
    await agentGatewayPage.fetchCardBtn.first().click();
    await expect(agentGatewayPage.inspection).toContainText("mock-agent");
    await agentGatewayPage.save();

    await expect.poll(() => saved.length).toBe(1);
    expect(saved[0].runtime_auth.advanced).toBe(true);
    expect(saved[0].runtime_auth.oauth).toEqual(agent.runtime_auth.oauth);
  });

  test("toggling an agent keeps its approved credential origins", async ({ agentGatewayPage }) => {
    const saved = await mockRegistration(agentGatewayPage.page, registration(false));
    await agentGatewayPage.goto();
    await agentGatewayPage.page.getByTestId(`agent-enabled-switch-${name}`).click();

    await expect.poll(() => saved.length).toBe(1);
    expect(saved[0].enabled).toBe(false);
    expect(saved[0].credential_origins).toEqual(["https://runtime.example.test"]);
  });

  // The picker's pages belong to the search they were read under: 45 keys, 20 to a page.
  test("clearing a search reads the first page again, not the offset scrolled to before it", async ({ agentGatewayPage }) => {
    const page = agentGatewayPage.page;
    const keys = Array.from({ length: 45 }, (_, i) => ({ id: `vk-${i}`, name: `key-${String(i).padStart(2, "0")}`, value: "<REDACTED>" }));
    const requested: { offset: number; search: string }[] = [];
    await page.route("**/api/governance/virtual-keys?**", async (route) => {
      const params = new URL(route.request().url()).searchParams;
      const offset = Number(params.get("offset") ?? 0);
      const limit = Number(params.get("limit") ?? 20);
      const search = params.get("search") ?? "";
      if (limit === 20) requested.push({ offset, search });
      const matching = search ? [{ id: "vk-acme", name: "acme-key", value: "<REDACTED>" }] : keys;
      await route.fulfill({
        json: { virtual_keys: matching.slice(offset, offset + limit), count: matching.length, total_count: matching.length, limit, offset },
      });
    });
    await agentGatewayPage.goto();
    await agentGatewayPage.openCreateSheet();
    await agentGatewayPage.addVirtualKeyBtn.click();

    const list = page.getByRole("listbox");
    await expect(list.getByRole("option", { name: "key-00" })).toBeVisible();
    await list.evaluate((node) => {
      node.scrollTop = node.scrollHeight;
    });
    await expect(list.getByRole("option", { name: "key-20" })).toBeAttached();

    const search = page.getByPlaceholder(/search/i).last();
    await search.fill("acme");
    await expect(list.getByRole("option", { name: "acme-key" })).toBeVisible();
    await search.fill("");

    await expect(list.getByRole("option", { name: "key-00" })).toBeVisible();
    await expect(list.getByRole("option", { name: "acme-key" })).toHaveCount(0);
    expect(requested.filter((request) => request.search === "acme").every((request) => request.offset === 0)).toBe(true);
  });
});
