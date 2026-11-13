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

// TXE-7353: the 12h cache-write and service-tier rates are pricing override fields. An override
// that sets the 12h write rate, the priority 12h rate as an explicit zero, and the flex rate above
// 200k is saved with all three in its patch (under their datasheet names) and reopens with them.
test.describe("Custom pricing overrides - 12h cache write and service tier rates", () => {
	const name = `e2e-12h-cache-write-${Date.now()}`;

	test.afterEach(async ({ request }) => {
		const res = await request.get("/api/governance/pricing-overrides");
		if (!res.ok()) return;
		const body = await res.json();
		for (const override of body.pricing_overrides ?? []) {
			if (override.name === name) await request.delete(`/api/governance/pricing-overrides/${override.id}`);
		}
	});

	test("saves a 12h write rate, an explicit zero priority 12h rate and a flex rate above 200k, and reopens with all three", async ({ page, request }) => {
		await page.goto("/workspace/custom-pricing/overrides");
		await page.getByTestId("pricing-override-create-btn").first().click();

		await page.getByTestId("pricing-override-name-input").fill(name);
		await page.getByTestId("pricing-override-pattern-input").fill("gpt-6-sol");
		await page.getByTestId("pricing-override-request-types-btn").click();
		await page.getByTestId("pricing-override-request-type-checkbox-chat_completion").click();
		await page.keyboard.press("Escape");

		await page.getByTestId("pricing-field-search").fill("12hr");
		await page.getByTestId("pricing-field-activate-cache_creation_input_token_cost_above_12hr").click();
		await page.getByTestId("pricing-override-field-input-cache_creation_input_token_cost_above_12hr").fill("0.000005");
		await page.getByTestId("pricing-field-activate-cache_creation_input_token_cost_above_12hr_above_272k_tokens_priority").click();
		await page.getByTestId("pricing-override-field-input-cache_creation_input_token_cost_above_12hr_above_272k_tokens_priority").fill("0");
		await page.getByTestId("pricing-field-search").fill(">200k, flex");
		await page.getByTestId("pricing-field-activate-input_cost_per_token_above_200k_tokens_flex").click();
		await page.getByTestId("pricing-override-field-input-input_cost_per_token_above_200k_tokens_flex").fill("0.000002");

		await page.getByTestId("pricing-override-save-btn").click();
		await expect(page.getByTestId("pricing-override-save-btn")).toBeHidden();

		const res = await request.get("/api/governance/pricing-overrides");
		expect(res.ok()).toBeTruthy();
		const saved = ((await res.json()).pricing_overrides ?? []).find((o: { name: string }) => o.name === name);
		expect(saved, "the override was saved").toBeTruthy();
		const patch = JSON.parse(saved.pricing_patch);
		expect(patch.cache_creation_input_token_cost_above_12hr).toBe(0.000005);
		// The datasheet name is kept on the wire although its database column is shorter.
		expect(patch.cache_creation_input_token_cost_above_12hr_above_272k_tokens_priority).toBe(0);
		expect(patch.input_cost_per_token_above_200k_tokens_flex).toBe(0.000002);

		await page.reload();
		await page.getByTestId(`pricing-override-actions-btn-${saved.id}`).click();
		await page.getByTestId(`pricing-override-edit-btn-${saved.id}`).click();
		await page.getByTestId("pricing-field-search").fill("12hr");
		await expect(page.getByTestId("pricing-override-field-input-cache_creation_input_token_cost_above_12hr")).toHaveValue("0.000005");
		await expect(page.getByTestId("pricing-override-field-input-cache_creation_input_token_cost_above_12hr_above_272k_tokens_priority")).toHaveValue("0");
		await page.getByTestId("pricing-field-search").fill(">200k, flex");
		await expect(page.getByTestId("pricing-override-field-input-input_cost_per_token_above_200k_tokens_flex")).toHaveValue("0.000002");
	});
});

// TXE-7363: the realtime text message rate is a pricing override field in the audio group. An
// override that sets it is saved with it in its patch and reopens with the value.
test.describe("Custom pricing overrides - realtime text message rate", () => {
	const name = `e2e-text-message-rate-${Date.now()}`;

	test.afterEach(async ({ request }) => {
		const res = await request.get("/api/governance/pricing-overrides");
		if (!res.ok()) return;
		const body = await res.json();
		for (const override of body.pricing_overrides ?? []) {
			if (override.name === name) await request.delete(`/api/governance/pricing-overrides/${override.id}`);
		}
	});

	test("saves a realtime text message rate and reopens with it", async ({ page, request }) => {
		const key = "input_cost_per_text_message";
		await page.goto("/workspace/custom-pricing/overrides");
		await page.getByTestId("pricing-override-create-btn").first().click();

		await page.getByTestId("pricing-override-name-input").fill(name);
		await page.getByTestId("pricing-override-pattern-input").fill("grok-voice-3.0");
		await page.getByTestId("pricing-override-request-types-btn").click();
		await page.getByTestId("pricing-override-request-type-checkbox-live").click();
		await page.keyboard.press("Escape");

		await page.getByTestId("pricing-field-search").fill("text message");
		await page.getByTestId(`pricing-field-activate-${key}`).click();
		await page.getByTestId(`pricing-override-field-input-${key}`).fill("0.004");

		await page.getByTestId("pricing-override-save-btn").click();
		await expect(page.getByTestId("pricing-override-save-btn")).toBeHidden();

		const res = await request.get("/api/governance/pricing-overrides");
		expect(res.ok()).toBeTruthy();
		const saved = ((await res.json()).pricing_overrides ?? []).find((o: { name: string }) => o.name === name);
		expect(saved, "the override was saved").toBeTruthy();
		expect(JSON.parse(saved.pricing_patch)[key]).toBe(0.004);

		await page.reload();
		await page.getByTestId(`pricing-override-actions-btn-${saved.id}`).click();
		await page.getByTestId(`pricing-override-edit-btn-${saved.id}`).click();
		await page.getByTestId("pricing-field-search").fill("text message");
		await expect(page.getByTestId(`pricing-override-field-input-${key}`)).toHaveValue("0.004");
	});
});
