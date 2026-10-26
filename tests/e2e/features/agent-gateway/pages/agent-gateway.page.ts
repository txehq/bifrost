import { Locator, Page, expect } from "@playwright/test";
import { BasePage } from "../../../core/pages/base.page";
import { waitForNetworkIdle } from "../../../core/utils/test-helpers";

/**
 * Page object for the Agent Gateway page and its registration sheet.
 */
export class AgentGatewayPage extends BasePage {
  readonly createBtn: Locator;
  readonly table: Locator;
  readonly sheet: Locator;
  readonly nameInput: Locator;
  readonly cardUrlInput: Locator;
  readonly fetchCardBtn: Locator;
  readonly cardOptionsToggle: Locator;
  readonly inspection: Locator;
  readonly inspectionError: Locator;
  readonly cardAuthTypeSelect: Locator;
  readonly cardExtraHeaders: Locator;
  readonly runtimeRequiredHeaders: Locator;
  readonly runtimeTlsTrigger: Locator;
  readonly runtimeCaCertInput: Locator;
  readonly allowByDefaultSwitch: Locator;
  readonly addVirtualKeyBtn: Locator;
  readonly virtualKeysList: Locator;
  readonly virtualKeysEmpty: Locator;
  readonly saveBtn: Locator;
  readonly cancelBtn: Locator;

  constructor(page: Page) {
    super(page);
    this.createBtn = page.getByTestId("create-agent-btn");
    this.table = page.getByTestId("agents-table");
    this.sheet = page.getByTestId("agent-sheet");
    this.nameInput = page.getByTestId("agent-name-input");
    this.cardUrlInput = page.getByTestId("agent-card-url-input");
    this.fetchCardBtn = page.getByTestId("agent-card-fetch-button");
    this.cardOptionsToggle = page.getByTestId("agent-card-options-toggle");
    this.inspection = page.getByTestId("agent-card-inspection");
    this.inspectionError = page.getByTestId("agent-card-fetch-error");
    this.cardAuthTypeSelect = page.getByTestId("agent-card-auth-type-select");
    this.cardExtraHeaders = page.getByTestId("agent-card-auth-extra-headers");
    this.runtimeRequiredHeaders = page.getByTestId("agent-runtime-auth-required-headers");
    this.runtimeTlsTrigger = page.getByTestId("agent-runtime-tls-toggle");
    this.runtimeCaCertInput = page.getByTestId("agent-runtime-tls-ca-cert-input");
    this.allowByDefaultSwitch = page.getByTestId("agent-allow-by-default-switch");
    this.addVirtualKeyBtn = page.getByTestId("agent-add-virtual-key-button");
    this.virtualKeysList = page.getByTestId("agent-virtual-keys-list");
    this.virtualKeysEmpty = page.getByTestId("agent-virtual-keys-empty");
    this.saveBtn = page.getByTestId("agent-save-button");
    this.cancelBtn = page.getByTestId("agent-cancel-button");
  }

  async goto(): Promise<void> {
    await this.page.goto("/workspace/agent-gateway");
    await waitForNetworkIdle(this.page);
  }

  /** The create button is rendered by the table header or, with no agents yet, by the empty state. */
  async openCreateSheet(): Promise<void> {
    await this.createBtn.click();
    await expect(this.sheet).toBeVisible();
  }

  async openEditSheet(name: string): Promise<void> {
    await this.page.getByTestId(`agent-actions-${name}-btn`).click();
    await this.page.getByTestId(`agent-edit-${name}-menu-item`).click();
    await expect(this.sheet).toBeVisible();
    await expect(this.nameInput).toHaveValue(name);
  }

  statusCell(name: string): Locator {
    return this.page.getByTestId(`agent-status-${name}`);
  }

  accessCell(name: string): Locator {
    return this.page.getByTestId(`agent-access-${name}`);
  }

  /** Select the authentication used for the server-side card fetch, under the collapsed "Card fetch options". */
  async selectCardAuthType(label: "None" | "Headers" | "OAuth 2.0 client credentials"): Promise<void> {
    if (!(await this.cardAuthTypeSelect.isVisible())) {
      await this.cardOptionsToggle.click();
    }
    await this.cardAuthTypeSelect.click();
    await this.page.getByRole("option", { name: label, exact: true }).click();
  }

  /** Type a header into the card-fetch "Extra headers" table; row 0 is the first (initially empty) row. */
  async setCardHeader(row: number, name: string, value: string): Promise<void> {
    await this.cardExtraHeaders.locator(`input[data-row="${row}"][data-column="key"]`).fill(name);
    await this.cardExtraHeaders.locator(`input[data-row="${row}"][data-column="value"]`).fill(value);
  }

  async fetchCard(): Promise<void> {
    await this.fetchCardBtn.click();
    await expect(this.inspection).toBeVisible();
  }

  /** Fill the credential for a header the fetched card advertises; its key is fixed by the card. */
  async fillRequiredRuntimeHeader(name: string, value: string): Promise<void> {
    await expect(this.runtimeRequiredHeaders.locator('input[data-column="key"]')).toHaveValue(name);
    await this.runtimeRequiredHeaders.locator('input[data-column="value"]').fill(value);
  }

  async setRuntimeCaCert(pem: string): Promise<void> {
    await this.runtimeTlsTrigger.click();
    await expect(this.runtimeCaCertInput).toBeVisible();
    await this.runtimeCaCertInput.fill(pem);
  }

  /** Grant a virtual key through the "Add key" selector popover. */
  async grantVirtualKey(name: string): Promise<void> {
    await this.addVirtualKeyBtn.click();
    const option = this.page.getByRole("option", { name: new RegExp(name.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")) });
    await expect(option).toBeVisible();
    await option.click();
    await expect(this.virtualKeysList).toContainText(name);
  }

  async revokeVirtualKey(id: string): Promise<void> {
    await this.page.getByTestId(`agent-revoke-virtual-key-${id}`).click();
  }

  async save(): Promise<void> {
    await this.saveBtn.click();
  }

  async expectSheetClosed(): Promise<void> {
    await expect(this.sheet).toBeHidden();
  }
}
