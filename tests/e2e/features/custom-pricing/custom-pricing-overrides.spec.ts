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

// TXE-7363: the realtime text message rate is a pricing override field. A realtime turn is priced in
// the responses mode, so the override selects the Responses request type. An override that sets the
// rate is saved with it in its patch and reopens with the value.
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
		await page.getByTestId("pricing-override-request-type-checkbox-responses").click();
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

// TXE-7372: the priority image output token rate is a pricing override field. An override that
// sets it, with the standard image token rate set as an explicit zero, is saved with both in its
// patch and reopens with both values.
test.describe("Custom pricing overrides - priority image token rate", () => {
	const name = `e2e-priority-image-token-${Date.now()}`;

	test.afterEach(async ({ request }) => {
		const res = await request.get("/api/governance/pricing-overrides");
		if (!res.ok()) return;
		const body = await res.json();
		for (const override of body.pricing_overrides ?? []) {
			if (override.name === name) await request.delete(`/api/governance/pricing-overrides/${override.id}`);
		}
	});

	test("saves a priority image token rate and an explicit zero standard image token rate, and reopens with both", async ({ page, request }) => {
		await page.goto("/workspace/custom-pricing/overrides");
		await page.getByTestId("pricing-override-create-btn").first().click();

		await page.getByTestId("pricing-override-name-input").fill(name);
		await page.getByTestId("pricing-override-pattern-input").fill("gemini-2.5-flash-image");
		await page.getByTestId("pricing-override-request-types-btn").click();
		await page.getByTestId("pricing-override-request-type-checkbox-image_generation").click();
		await page.keyboard.press("Escape");

		await page.getByTestId("pricing-field-search").fill("Output / image token");
		await page.getByTestId("pricing-field-activate-output_cost_per_image_token_priority").click();
		await page.getByTestId("pricing-override-field-input-output_cost_per_image_token_priority").fill("0.000054");
		await page.getByTestId("pricing-field-activate-output_cost_per_image_token").click();
		await page.getByTestId("pricing-override-field-input-output_cost_per_image_token").fill("0");

		await page.getByTestId("pricing-override-save-btn").click();
		await expect(page.getByTestId("pricing-override-save-btn")).toBeHidden();

		const res = await request.get("/api/governance/pricing-overrides");
		expect(res.ok()).toBeTruthy();
		const saved = ((await res.json()).pricing_overrides ?? []).find((o: { name: string }) => o.name === name);
		expect(saved, "the override was saved").toBeTruthy();
		const patch = JSON.parse(saved.pricing_patch);
		expect(patch.output_cost_per_image_token_priority).toBe(0.000054);
		expect(patch.output_cost_per_image_token).toBe(0);

		await page.reload();
		await page.getByTestId(`pricing-override-actions-btn-${saved.id}`).click();
		await page.getByTestId(`pricing-override-edit-btn-${saved.id}`).click();
		await page.getByTestId("pricing-field-search").fill("Output / image token");
		await expect(page.getByTestId("pricing-override-field-input-output_cost_per_image_token_priority")).toHaveValue("0.000054");
		await expect(page.getByTestId("pricing-override-field-input-output_cost_per_image_token")).toHaveValue("0");
	});
});

// TXE-7354: the explicit batch rates are pricing override fields. An override that sets the batch
// cache-read rate, the batch 12h write above 272k (whose column name is shortened), and the batch
// image output rate as an explicit zero is saved with all three and reopens with them.
test.describe("Custom pricing overrides - explicit batch rates", () => {
	const name = `e2e-batch-rates-${Date.now()}`;

	test.afterEach(async ({ request }) => {
		const res = await request.get("/api/governance/pricing-overrides");
		if (!res.ok()) return;
		const body = await res.json();
		for (const override of body.pricing_overrides ?? []) {
			if (override.name === name) await request.delete(`/api/governance/pricing-overrides/${override.id}`);
		}
	});

	test("saves a batch cache-read rate, a batch 12h write above 272k and an explicit zero batch image rate, and reopens with all three", async ({ page, request }) => {
		await page.goto("/workspace/custom-pricing/overrides");
		await page.getByTestId("pricing-override-create-btn").first().click();

		await page.getByTestId("pricing-override-name-input").fill(name);
		await page.getByTestId("pricing-override-pattern-input").fill("gpt-5.4");
		await page.getByTestId("pricing-override-request-types-btn").click();
		await page.getByTestId("pricing-override-request-type-checkbox-chat_completion").click();
		await page.keyboard.press("Escape");

		await page.getByTestId("pricing-field-search").fill("batch");
		await page.getByTestId("pricing-field-activate-cache_read_input_token_cost_batches").click();
		await page.getByTestId("pricing-override-field-input-cache_read_input_token_cost_batches").fill("0.0000013");
		await page.getByTestId("pricing-field-activate-cache_creation_input_token_cost_above_12hr_above_272k_tokens_batches").click();
		await page.getByTestId("pricing-override-field-input-cache_creation_input_token_cost_above_12hr_above_272k_tokens_batches").fill("0.000005");
		await page.getByTestId("pricing-field-activate-output_cost_per_image_token_batches").click();
		await page.getByTestId("pricing-override-field-input-output_cost_per_image_token_batches").fill("0");

		await page.getByTestId("pricing-override-save-btn").click();
		await expect(page.getByTestId("pricing-override-save-btn")).toBeHidden();

		const res = await request.get("/api/governance/pricing-overrides");
		expect(res.ok()).toBeTruthy();
		const saved = ((await res.json()).pricing_overrides ?? []).find((o: { name: string }) => o.name === name);
		expect(saved, "the override was saved").toBeTruthy();
		const patch = JSON.parse(saved.pricing_patch);
		expect(patch.cache_read_input_token_cost_batches).toBe(0.0000013);
		expect(patch.cache_creation_input_token_cost_above_12hr_above_272k_tokens_batches).toBe(0.000005);
		expect(patch.output_cost_per_image_token_batches).toBe(0);

		await page.reload();
		await page.getByTestId(`pricing-override-actions-btn-${saved.id}`).click();
		await page.getByTestId(`pricing-override-edit-btn-${saved.id}`).click();
		await page.getByTestId("pricing-field-search").fill("batch");
		await expect(page.getByTestId("pricing-override-field-input-cache_read_input_token_cost_batches")).toHaveValue("0.0000013");
		await expect(page.getByTestId("pricing-override-field-input-cache_creation_input_token_cost_above_12hr_above_272k_tokens_batches")).toHaveValue("0.000005");
		await expect(page.getByTestId("pricing-override-field-input-output_cost_per_image_token_batches")).toHaveValue("0");
	});
});

// TXE-7356: the image size, quality, resolution-tier and step variants are pricing override
// fields. An override that sets an hd quality rate, the dotted 0.5K tier and a step rate is saved
// with all three in its patch under their datasheet keys, and reopens with all three.
test.describe("Custom pricing overrides - image variant rates", () => {
	const name = `e2e-image-variant-rate-${Date.now()}`;
	const fields: Array<[string, string, number]> = [
		["output_cost_per_image_above_1024_and_1024_pixels_hd_quality", "0.088", 0.088],
		["output_cost_per_image_0.5K", "0.02", 0.02],
		["output_cost_per_image_above_1024_and_1024_pixels_max_steps", "0.08", 0.08],
	];

	test.afterEach(async ({ request }) => {
		const res = await request.get("/api/governance/pricing-overrides");
		if (!res.ok()) return;
		const body = await res.json();
		for (const override of body.pricing_overrides ?? []) {
			if (override.name === name) await request.delete(`/api/governance/pricing-overrides/${override.id}`);
		}
	});

	test("saves an hd quality rate, the 0.5K tier rate and a step rate, and reopens with all three", async ({ page, request }) => {
		await page.goto("/workspace/custom-pricing/overrides");
		await page.getByTestId("pricing-override-create-btn").first().click();

		await page.getByTestId("pricing-override-name-input").fill(name);
		await page.getByTestId("pricing-override-pattern-input").fill("dall-e-3");
		await page.getByTestId("pricing-override-request-types-btn").click();
		await page.getByTestId("pricing-override-request-type-checkbox-image_generation").click();
		await page.keyboard.press("Escape");

		// The search matches a field's key as well as its label, so each key finds its own field.
		for (const [key, value] of fields) {
			await page.getByTestId("pricing-field-search").fill(key);
			await page.getByTestId(`pricing-field-activate-${key}`).click();
			await page.getByTestId(`pricing-override-field-input-${key}`).fill(value);
		}

		await page.getByTestId("pricing-override-save-btn").click();
		await expect(page.getByTestId("pricing-override-save-btn")).toBeHidden();

		// The saved patch carries each rate under its datasheet key, the dotted one included.
		const res = await request.get("/api/governance/pricing-overrides");
		expect(res.ok()).toBeTruthy();
		const saved = ((await res.json()).pricing_overrides ?? []).find((o: { name: string }) => o.name === name);
		expect(saved, "the override was saved").toBeTruthy();
		const patch = JSON.parse(saved.pricing_patch);
		for (const [key, , number] of fields) {
			expect(patch[key], key).toBe(number);
		}

		// A reload reopens the override with every value in place.
		await page.reload();
		await page.getByTestId(`pricing-override-actions-btn-${saved.id}`).click();
		await page.getByTestId(`pricing-override-edit-btn-${saved.id}`).click();
		for (const [key, value] of fields) {
			await page.getByTestId("pricing-field-search").fill(key);
			await expect(page.getByTestId(`pricing-override-field-input-${key}`)).toHaveValue(value);
		}
	});
});

// TXE-7365: the Bedrock guardrail unit rates are pricing override fields for chat. An override on the
// chat model sets the rate its guardrailed requests are charged; it is saved and reopens with the value.
test.describe("Custom pricing overrides - Bedrock guardrail unit rate", () => {
	const name = `e2e-guardrail-unit-rate-${Date.now()}`;

	test.afterEach(async ({ request }) => {
		const res = await request.get("/api/governance/pricing-overrides");
		if (!res.ok()) return;
		const body = await res.json();
		for (const override of body.pricing_overrides ?? []) {
			if (override.name === name) await request.delete(`/api/governance/pricing-overrides/${override.id}`);
		}
	});

	test("saves a guardrail content policy unit rate and reopens with it", async ({ page, request }) => {
		const key = "guardrail_cost_per_content_policy_unit";
		await page.goto("/workspace/custom-pricing/overrides");
		await page.getByTestId("pricing-override-create-btn").first().click();

		await page.getByTestId("pricing-override-name-input").fill(name);
		await page.getByTestId("pricing-override-pattern-input").fill("amazon.nova-lite-v1:0");
		await page.getByTestId("pricing-override-request-types-btn").click();
		await page.getByTestId("pricing-override-request-type-checkbox-chat_completion").click();
		await page.keyboard.press("Escape");

		await page.getByTestId("pricing-field-search").fill("content policy unit");
		await page.getByTestId(`pricing-field-activate-${key}`).click();
		await page.getByTestId(`pricing-override-field-input-${key}`).fill("0.001");

		await page.getByTestId("pricing-override-save-btn").click();
		await expect(page.getByTestId("pricing-override-save-btn")).toBeHidden();

		const res = await request.get("/api/governance/pricing-overrides");
		expect(res.ok()).toBeTruthy();
		const saved = ((await res.json()).pricing_overrides ?? []).find((o: { name: string }) => o.name === name);
		expect(saved, "the override was saved").toBeTruthy();
		expect(JSON.parse(saved.pricing_patch)[key]).toBe(0.001);

		await page.reload();
		await page.getByTestId(`pricing-override-actions-btn-${saved.id}`).click();
		await page.getByTestId(`pricing-override-edit-btn-${saved.id}`).click();
		await page.getByTestId("pricing-field-search").fill("content policy unit");
		await expect(page.getByTestId(`pricing-override-field-input-${key}`)).toHaveValue("0.001");
	});
});

// TXE-7357: the video token, per-clip, mode and video-input rates are pricing override fields. An
// override that sets a per-clip price, Kling's pro rate and Seedance's 720p video-input rate is
// saved with all three in its patch under their datasheet keys, and reopens with all three.
test.describe("Custom pricing overrides - video clip, mode and video-input rates", () => {
	const name = `e2e-video-clip-mode-rate-${Date.now()}`;
	const fields: Array<[string, string, number]> = [
		["output_cost_per_video_768p_6s", "0.28", 0.28],
		["output_cost_per_video_per_second_pro", "0.224", 0.224],
		["output_cost_per_video_per_second_720p_video_in", "0.22", 0.22],
	];

	test.afterEach(async ({ request }) => {
		const res = await request.get("/api/governance/pricing-overrides");
		if (!res.ok()) return;
		const body = await res.json();
		for (const override of body.pricing_overrides ?? []) {
			if (override.name === name) await request.delete(`/api/governance/pricing-overrides/${override.id}`);
		}
	});

	test("saves a per-clip price, a pro mode rate and a video-input rate, and reopens with all three", async ({ page, request }) => {
		await page.goto("/workspace/custom-pricing/overrides");
		await page.getByTestId("pricing-override-create-btn").first().click();

		await page.getByTestId("pricing-override-name-input").fill(name);
		await page.getByTestId("pricing-override-pattern-input").fill("MiniMax-Hailuo-2.3");
		await page.getByTestId("pricing-override-request-types-btn").click();
		await page.getByTestId("pricing-override-request-type-checkbox-video_generation").click();
		await page.keyboard.press("Escape");

		// The search matches a field's key as well as its label, so each key finds its own field.
		for (const [key, value] of fields) {
			await page.getByTestId("pricing-field-search").fill(key);
			await page.getByTestId(`pricing-field-activate-${key}`).click();
			await page.getByTestId(`pricing-override-field-input-${key}`).fill(value);
		}

		await page.getByTestId("pricing-override-save-btn").click();
		await expect(page.getByTestId("pricing-override-save-btn")).toBeHidden();

		const res = await request.get("/api/governance/pricing-overrides");
		expect(res.ok()).toBeTruthy();
		const saved = ((await res.json()).pricing_overrides ?? []).find((o: { name: string }) => o.name === name);
		expect(saved, "the override was saved").toBeTruthy();
		const patch = JSON.parse(saved.pricing_patch);
		for (const [key, , number] of fields) {
			expect(patch[key], key).toBe(number);
		}

		await page.reload();
		await page.getByTestId(`pricing-override-actions-btn-${saved.id}`).click();
		await page.getByTestId(`pricing-override-edit-btn-${saved.id}`).click();
		for (const [key, value] of fields) {
			await page.getByTestId("pricing-field-search").fill(key);
			await expect(page.getByTestId(`pricing-override-field-input-${key}`)).toHaveValue(value);
		}
	});
});

// TXE-7355: the 32k, 256k and 512k tiers and the 128k cache-read tier are pricing override fields.
// An override that sets the 512k input rate and the 128k cache-read rate as an explicit zero is
// saved with both and reopens with them.
test.describe("Custom pricing overrides - long-context tiers", () => {
	const name = `e2e-long-context-${Date.now()}`;

	test.afterEach(async ({ request }) => {
		const res = await request.get("/api/governance/pricing-overrides");
		if (!res.ok()) return;
		const body = await res.json();
		for (const override of body.pricing_overrides ?? []) {
			if (override.name === name) await request.delete(`/api/governance/pricing-overrides/${override.id}`);
		}
	});

	test("saves a 512k input rate and an explicit zero 128k cache-read rate, and reopens with both", async ({ page, request }) => {
		await page.goto("/workspace/custom-pricing/overrides");
		await page.getByTestId("pricing-override-create-btn").first().click();

		await page.getByTestId("pricing-override-name-input").fill(name);
		await page.getByTestId("pricing-override-pattern-input").fill("MiniMax-M3");
		await page.getByTestId("pricing-override-request-types-btn").click();
		await page.getByTestId("pricing-override-request-type-checkbox-chat_completion").click();
		await page.keyboard.press("Escape");

		await page.getByTestId("pricing-field-search").fill(">512k");
		await page.getByTestId("pricing-field-activate-input_cost_per_token_above_512k_tokens").click();
		await page.getByTestId("pricing-override-field-input-input_cost_per_token_above_512k_tokens").fill("0.000006");
		await page.getByTestId("pricing-field-search").fill("cache read / token (>128k)");
		await page.getByTestId("pricing-field-activate-cache_read_input_token_cost_above_128k_tokens").click();
		await page.getByTestId("pricing-override-field-input-cache_read_input_token_cost_above_128k_tokens").fill("0");

		await page.getByTestId("pricing-override-save-btn").click();
		await expect(page.getByTestId("pricing-override-save-btn")).toBeHidden();

		const res = await request.get("/api/governance/pricing-overrides");
		expect(res.ok()).toBeTruthy();
		const saved = ((await res.json()).pricing_overrides ?? []).find((o: { name: string }) => o.name === name);
		expect(saved, "the override was saved").toBeTruthy();
		const patch = JSON.parse(saved.pricing_patch);
		expect(patch.input_cost_per_token_above_512k_tokens).toBe(0.000006);
		expect(patch.cache_read_input_token_cost_above_128k_tokens).toBe(0);

		await page.reload();
		await page.getByTestId(`pricing-override-actions-btn-${saved.id}`).click();
		await page.getByTestId(`pricing-override-edit-btn-${saved.id}`).click();
		await page.getByTestId("pricing-field-search").fill(">512k");
		await expect(page.getByTestId("pricing-override-field-input-input_cost_per_token_above_512k_tokens")).toHaveValue("0.000006");
		await page.getByTestId("pricing-field-search").fill("cache read / token (>128k)");
		await expect(page.getByTestId("pricing-override-field-input-cache_read_input_token_cost_above_128k_tokens")).toHaveValue("0");
	});
});

// TXE-7401: the bound an unmeasurable rendered video is charged up to is a pricing override field
// for video. An override that sets it is saved with it in its patch and reopens with the value.
test.describe("Custom pricing overrides - unmeasured video seconds", () => {
	const name = `e2e-unmeasured-video-seconds-${Date.now()}`;

	test.afterEach(async ({ request }) => {
		const res = await request.get("/api/governance/pricing-overrides");
		if (!res.ok()) return;
		const body = await res.json();
		for (const override of body.pricing_overrides ?? []) {
			if (override.name === name) await request.delete(`/api/governance/pricing-overrides/${override.id}`);
		}
	});

	test("saves an unmeasured video bound and reopens with it", async ({ page, request }) => {
		const key = "unmeasured_video_seconds";
		await page.goto("/workspace/custom-pricing/overrides");
		await page.getByTestId("pricing-override-create-btn").first().click();

		await page.getByTestId("pricing-override-name-input").fill(name);
		await page.getByTestId("pricing-override-pattern-input").fill("wan2.5-t2v-preview");
		await page.getByTestId("pricing-override-request-types-btn").click();
		await page.getByTestId("pricing-override-request-type-checkbox-video_generation").click();
		await page.keyboard.press("Escape");

		await page.getByTestId("pricing-field-search").fill("unmeasurable");
		await page.getByTestId(`pricing-field-activate-${key}`).click();
		await page.getByTestId(`pricing-override-field-input-${key}`).fill("15");

		await page.getByTestId("pricing-override-save-btn").click();
		await expect(page.getByTestId("pricing-override-save-btn")).toBeHidden();

		const res = await request.get("/api/governance/pricing-overrides");
		expect(res.ok()).toBeTruthy();
		const saved = ((await res.json()).pricing_overrides ?? []).find((o: { name: string }) => o.name === name);
		expect(saved, "the override was saved").toBeTruthy();
		expect(JSON.parse(saved.pricing_patch)[key]).toBe(15);

		await page.reload();
		await page.getByTestId(`pricing-override-actions-btn-${saved.id}`).click();
		await page.getByTestId(`pricing-override-edit-btn-${saved.id}`).click();
		await page.getByTestId("pricing-field-search").fill("unmeasurable");
		await expect(page.getByTestId(`pricing-override-field-input-${key}`)).toHaveValue("15");
	});
});

// TXE-7361: the cached-audio rates are pricing override fields. An override that sets the
// cached-audio rate and its priority rate as an explicit zero is saved with both in its patch and
// reopens with both values.
test.describe("Custom pricing overrides - cached audio rates", () => {
	const name = `e2e-cached-audio-${Date.now()}`;

	test.afterEach(async ({ request }) => {
		const res = await request.get("/api/governance/pricing-overrides");
		if (!res.ok()) return;
		const body = await res.json();
		for (const override of body.pricing_overrides ?? []) {
			if (override.name === name) await request.delete(`/api/governance/pricing-overrides/${override.id}`);
		}
	});

	test("saves a cached-audio rate and an explicit zero priority cached-audio rate, and reopens with both", async ({ page, request }) => {
		await page.goto("/workspace/custom-pricing/overrides");
		await page.getByTestId("pricing-override-create-btn").first().click();

		await page.getByTestId("pricing-override-name-input").fill(name);
		await page.getByTestId("pricing-override-pattern-input").fill("gpt-realtime-mini");
		await page.getByTestId("pricing-override-request-types-btn").click();
		await page.getByTestId("pricing-override-request-type-checkbox-live").click();
		await page.keyboard.press("Escape");

		await page.getByTestId("pricing-field-search").fill("Cache read / audio token");
		await page.getByTestId("pricing-field-activate-cache_read_input_audio_token_cost").click();
		await page.getByTestId("pricing-override-field-input-cache_read_input_audio_token_cost").fill("0.000003");
		await page.getByTestId("pricing-field-activate-cache_read_input_audio_token_cost_priority").click();
		await page.getByTestId("pricing-override-field-input-cache_read_input_audio_token_cost_priority").fill("0");

		await page.getByTestId("pricing-override-save-btn").click();
		await expect(page.getByTestId("pricing-override-save-btn")).toBeHidden();

		const res = await request.get("/api/governance/pricing-overrides");
		expect(res.ok()).toBeTruthy();
		const saved = ((await res.json()).pricing_overrides ?? []).find((o: { name: string }) => o.name === name);
		expect(saved, "the override was saved").toBeTruthy();
		const patch = JSON.parse(saved.pricing_patch);
		expect(patch.cache_read_input_audio_token_cost).toBe(0.000003);
		expect(patch.cache_read_input_audio_token_cost_priority).toBe(0);

		await page.reload();
		await page.getByTestId(`pricing-override-actions-btn-${saved.id}`).click();
		await page.getByTestId(`pricing-override-edit-btn-${saved.id}`).click();
		await page.getByTestId("pricing-field-search").fill("Cache read / audio token");
		await expect(page.getByTestId("pricing-override-field-input-cache_read_input_audio_token_cost")).toHaveValue("0.000003");
		await expect(page.getByTestId("pricing-override-field-input-cache_read_input_audio_token_cost_priority")).toHaveValue("0");
	});
});
