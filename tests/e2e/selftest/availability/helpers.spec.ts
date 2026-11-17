import type { Page } from "@playwright/test";
import { expect, test } from "../../core/fixtures/base.fixture";
import { skipIfShown, skipUnlessCount, skipUnlessEnabled, skipUnlessVisible } from "../../core/utils/availability";

// The sample suite scripts/availability.test.mjs runs, to pin what the skip helpers in
// core/utils/availability.ts do on a real page (TXE-7733): a control that renders late is waited
// for, not skipped; one that never renders skips; a locator matching several elements is judged by
// its first match instead of skipping on a strict-mode violation. Each title says the outcome the
// runner expects. It lives outside features/, so the UI suite never runs it.
const LATE_MS = 1500;

// Renders `html` into #root after LATE_MS, so an instant probe made on arrival misses it.
async function renderLate(page: Page, html: string) {
	await page.setContent('<div id="root"></div>');
	await page.evaluate(
		([markup, delay]) => {
			window.setTimeout(() => { // sleep-ok: the late render is what the helpers must wait for
				document.getElementById("root")!.innerHTML = markup as string;
			}, delay as number);
		},
		[html, LATE_MS],
	);
}

test.describe("Availability helpers", () => {
	test("runs: a control that renders late is waited for", async ({ page }) => {
		await renderLate(page, '<button id="late">Create</button>');
		// The probe the suite used to skip on answers false at this moment.
		expect(await page.locator("#late").isVisible()).toBe(false);
		await skipUnlessVisible(page.locator("#late"), "late control not visible");
		await expect(page.locator("#late")).toBeVisible();
	});

	test("runs: a control that enables late is waited for", async ({ page }) => {
		await page.setContent('<button id="save" disabled>Save</button>');
		await page.evaluate((delay) => {
			window.setTimeout(() => document.getElementById("save")!.removeAttribute("disabled"), delay); // sleep-ok: the late enable is what the helper must wait for
		}, LATE_MS);
		expect(await page.locator("#save").isEnabled()).toBe(false);
		await skipUnlessEnabled(page.locator("#save"), "save never enabled");
		await expect(page.locator("#save")).toBeEnabled();
	});

	test("runs: rows that render late are counted once they arrive", async ({ page }) => {
		await renderLate(page, "<ul><li class=row>a</li><li class=row>b</li></ul>");
		expect(await page.locator(".row").count()).toBe(0);
		await skipUnlessCount(page.locator(".row"), 2, "rows never rendered");
		await expect(page.locator(".row")).toHaveCount(2);
	});

	test("runs: a union of two fields that render together does not skip", async ({ page }) => {
		// The vLLM key form: url and model name render together, so .or() matches both.
		await renderLate(page, '<input data-testid="url"><input data-testid="model">');
		const union = page.getByTestId("url").or(page.getByTestId("model"));
		await skipUnlessVisible(union, "neither field rendered");
		await expect(union).toHaveCount(2);
	});

	test("runs: skipIfShown waits for the present state", async ({ page }) => {
		await renderLate(page, '<div id="table">a row</div>');
		await skipIfShown(page.getByText("Get Started"), page.locator("#table"), "only the guide rendered");
		await expect(page.locator("#table")).toBeVisible();
	});

	test("skips: a control that never renders", async ({ page }) => {
		await page.setContent('<div id="root"></div>');
		await skipUnlessVisible(page.locator("#never"), "the control never rendered", 1000);
		throw new Error("reached past a skip that should have happened");
	});

	test("skips: skipIfShown on the absent state", async ({ page }) => {
		await renderLate(page, "<p>Get Started</p>");
		await skipIfShown(page.getByText("Get Started"), page.locator("#table"), "only the guide rendered");
		throw new Error("reached past a skip that should have happened");
	});
});
