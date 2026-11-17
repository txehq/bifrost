import { expect, test } from '../../core/fixtures/base.fixture'
import { skipUnlessEnabled } from '../../core/utils/availability'
import { ConfigSettingsState } from './pages/config-settings.page'

test.describe('Pricing Config', () => {
  // Run all config tests serially to avoid parallel writes to the same config/store
  test.describe.configure({ mode: 'serial' })

  let originalPricingUrl: string | null = null

  test.beforeEach(async ({ configSettingsPage }) => {
    await configSettingsPage.goto('pricing-config')
    originalPricingUrl = await configSettingsPage.pricingDatasheetUrlInput.inputValue()
  })

  test.afterEach(async ({ configSettingsPage }) => {
    await configSettingsPage.goto('pricing-config')
    const canEdit = await configSettingsPage.pricingDatasheetUrlInput.isEditable().catch(() => false)
    if (!canEdit || originalPricingUrl === null) return
    await configSettingsPage.setPricingDatasheetUrl(originalPricingUrl)
    const isSaveEnabled = await configSettingsPage.pricingSaveBtn.isDisabled().then((d) => !d)
    if (isSaveEnabled) {
      await configSettingsPage.savePricingConfig()
      await configSettingsPage.dismissToasts()
    }
  })

  test('should display pricing config view', async ({ configSettingsPage }) => {
    await expect(configSettingsPage.pricingConfigView).toBeVisible()
    await expect(configSettingsPage.pricingDatasheetUrlInput).toBeVisible()
    await expect(configSettingsPage.pricingForceSyncBtn).toBeVisible()
    await expect(configSettingsPage.pricingSaveBtn).toBeVisible()
  })

  test('should set and save datasheet URL', async ({ configSettingsPage, page }) => {
    const testUrl = 'https://getbifrost.ai/datasheet?source=e2e'
    // Saving a new datasheet URL makes the gateway fetch it first, through an SSRF-safe dialer that
    // refuses loopback, so an offline run has no URL the gateway would accept (TXE-7395). Offline,
    // the save is answered here, after the test checks the form sent the URL it was given; the
    // gateway's own check is pinned by TestCheckURLAccessibility_* in handlers/config_test.go.
    // Nothing is stored, so afterEach finds the form unchanged and saves nothing.
    const sent: unknown[] = []
    const configApi = (url: URL) => url.pathname === '/api/config'
    if (process.env.E2E_OFFLINE === '1') {
      await page.route(
        configApi,
        async (route) => {
          if (route.request().method() !== 'PUT') return route.continue()
          sent.push(route.request().postDataJSON())
          await route.fulfill({ json: { status: 'success', message: 'configuration updated successfully' } })
        },
      )
    }
    await configSettingsPage.setPricingDatasheetUrl(testUrl)

    await skipUnlessEnabled(configSettingsPage.pricingSaveBtn, 'Save button disabled (no changes detected or RBAC)')

    await configSettingsPage.savePricingConfig()
    await configSettingsPage.dismissToasts()
    if (process.env.E2E_OFFLINE === '1') {
      expect(sent).toEqual([expect.objectContaining({ framework_config: expect.objectContaining({ pricing_url: testUrl }) })])
      await page.unroute(configApi)
    }
  })

  test('should trigger force sync', async ({ configSettingsPage }) => {
    await skipUnlessEnabled(configSettingsPage.pricingForceSyncBtn, 'Force sync button disabled (RBAC or no datasheet URL)')

    await configSettingsPage.triggerForceSync()
    await configSettingsPage.dismissToasts()
  })

  test('should validate URL format', async ({ configSettingsPage }) => {
    await configSettingsPage.pricingDatasheetUrlInput.fill('invalid-url-no-http')
    await skipUnlessEnabled(configSettingsPage.pricingSaveBtn, 'Save button disabled (RBAC)')
    await configSettingsPage.pricingSaveBtn.click()

    await expect(configSettingsPage.page.getByText(/URL must start with http|valid URL/i)).toBeVisible()
  })
})

test.describe('Pricing Config Settings', () => {
  test.describe.configure({ mode: 'serial' })

  let originalState: ConfigSettingsState

  test.beforeEach(async ({ configSettingsPage }) => {
    await configSettingsPage.goto('pricing-config')
    originalState = await configSettingsPage.getCurrentSettings('pricing-config')
  })

  test.afterEach(async ({ configSettingsPage }) => {
    if (originalState) {
      await configSettingsPage.restoreSettings(originalState)
    }
  })

  test('should display pricing config settings', async ({ configSettingsPage }) => {
    await expect(configSettingsPage.page.getByRole('heading', { name: /Model Settings/i })).toBeVisible()
  })
})
