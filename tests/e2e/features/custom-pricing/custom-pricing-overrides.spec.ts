import { expect, test } from "../../core/fixtures/base.fixture";

// TXE-7306: the video audio rates are pricing override fields. An override that sets one, and
// one set to an explicit zero, is saved with both in its patch and reopens with both values.
test.describe("Custom pricing overrides - video audio rates", () => {
	const name = `e2e-video-audio-rate-${Date.now()}`;

	test.afterEach(async ({ request }) => {
		const res = await request.get("/api/governance/pricing-overrides");
		if (!res.ok()) return;
		const body = await res.json();
		for (const override of body.pricing_overrides ?? []) {
			if (override.name === name) await request.delete(`/api/governance/pricing-overrides/${override.id}`);
		}
	});

	test("saves a 720p audio rate and an explicit zero 4K audio rate, and reopens with both", async ({ page, request }) => {
		await page.goto("/workspace/custom-pricing/overrides");
		await page.getByTestId("pricing-override-create-btn").first().click();

		await page.getByTestId("pricing-override-name-input").fill(name);
		await page.getByTestId("pricing-override-pattern-input").fill("wan2.5-t2v-preview");
		await page.getByTestId("pricing-override-request-types-btn").click();
		await page.getByTestId("pricing-override-request-type-checkbox-video_generation").click();
		await page.keyboard.press("Escape");

		await page.getByTestId("pricing-field-search").fill("with audio");
		await page.getByTestId("pricing-field-activate-output_cost_per_video_per_second_720p_audio").click();
		await page.getByTestId("pricing-override-field-input-output_cost_per_video_per_second_720p_audio").fill("0.12");
		await page.getByTestId("pricing-field-activate-output_cost_per_video_per_second_4k_audio").click();
		await page.getByTestId("pricing-override-field-input-output_cost_per_video_per_second_4k_audio").fill("0");

		await page.getByTestId("pricing-override-save-btn").click();
		await expect(page.getByTestId("pricing-override-save-btn")).toBeHidden();

		// The saved patch carries both rates, the zero as a zero rather than dropped.
		const res = await request.get("/api/governance/pricing-overrides");
		expect(res.ok()).toBeTruthy();
		const saved = ((await res.json()).pricing_overrides ?? []).find((o: { name: string }) => o.name === name);
		expect(saved, "the override was saved").toBeTruthy();
		const patch = JSON.parse(saved.pricing_patch);
		expect(patch.output_cost_per_video_per_second_720p_audio).toBe(0.12);
		expect(patch.output_cost_per_video_per_second_4k_audio).toBe(0);

		// A reload reopens the override with both values in place.
		await page.reload();
		await page.getByTestId(`pricing-override-actions-btn-${saved.id}`).click();
		await page.getByTestId(`pricing-override-edit-btn-${saved.id}`).click();
		// The sheet opens with its field groups collapsed; the search lists the two rates.
		await page.getByTestId("pricing-field-search").fill("with audio");
		await expect(page.getByTestId("pricing-override-field-input-output_cost_per_video_per_second_720p_audio")).toHaveValue("0.12");
		await expect(page.getByTestId("pricing-override-field-input-output_cost_per_video_per_second_4k_audio")).toHaveValue("0");
	});
});
