import type { APIRequestContext } from '@playwright/test'
import { expect, test } from '../../core/fixtures/base.fixture'

// TXE-7349: the six output_cost_per_second_<band> rates are editable overrides. A rate saved
// from the form must reach the stored patch under its own key and read back into the form,
// or an operator's per-resolution price is silently dropped and the band bills at the
// datasheet rate.

interface StoredOverride {
  id: string
  name: string
  // The stored patch, as the JSON string the list API returns.
  pricing_patch?: string
  patch?: Record<string, unknown>
}

async function findOverride(request: APIRequestContext, name: string): Promise<StoredOverride | undefined> {
  const res = await request.get(`/api/governance/pricing-overrides?search=${encodeURIComponent(name)}`)
  expect(res.ok(), await res.text()).toBe(true)
  const body = await res.json()
  const rows: StoredOverride[] = body.pricing_overrides ?? []
  const row = rows.find((r) => r.name === name)
  if (row) row.patch = row.pricing_patch ? JSON.parse(row.pricing_patch) : {}
  return row
}

test.describe('Custom pricing overrides: video resolution bands', () => {
  const created: string[] = []

  test.afterEach(async ({ page }) => {
    for (const name of created.splice(0)) {
      const row = await findOverride(page.request, name).catch(() => undefined)
      if (row) await page.request.delete(`/api/governance/pricing-overrides/${row.id}`)
    }
  })

  test('saves an output_cost_per_second_4k override and reads it back into the form', async ({ page }) => {
    const name = `e2e video 4k band ${Date.now()}`
    created.push(name)

    await page.goto('/workspace/custom-pricing/overrides')
    await page.getByTestId('pricing-override-create-btn').first().click()

    await page.getByTestId('pricing-override-name-input').fill(name)
    await page.getByTestId('pricing-override-pattern-input').fill('veo-3.1-generate-001')
    await page.getByTestId('pricing-override-request-types-btn').click()
    await page.getByTestId('pricing-override-request-type-checkbox-video_generation').click()
    await page.keyboard.press('Escape')

    await page.getByTestId('pricing-field-search').fill('output_cost_per_second_4k')
    await page.getByTestId('pricing-field-activate-output_cost_per_second_4k').click()
    await page.getByTestId('pricing-override-field-input-output_cost_per_second_4k').fill('0.6')

    await page.getByTestId('pricing-override-save-btn').click()

    await expect.poll(async () => (await findOverride(page.request, name))?.patch?.output_cost_per_second_4k).toBe(0.6)
    const stored = await findOverride(page.request, name)
    expect(stored?.patch, 'only the band the form set is stored').toEqual({ output_cost_per_second_4k: 0.6 })

    await page.reload()
    await page.getByTestId('pricing-overrides-search-input').fill(name)
    await page.getByTestId(`pricing-override-actions-btn-${stored!.id}`).click()
    await page.getByTestId(`pricing-override-edit-btn-${stored!.id}`).click()
    // The editor opens with its groups collapsed; the search shows the field with its saved value.
    await page.getByTestId('pricing-field-search').fill('output_cost_per_second_4k')
    await expect(page.getByTestId('pricing-override-field-input-output_cost_per_second_4k')).toHaveValue('0.6')
  })
})
