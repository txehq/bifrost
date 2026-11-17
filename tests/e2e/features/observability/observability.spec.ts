import { expect, test } from '../../core/fixtures/base.fixture'
import type { APIRequestContext } from '@playwright/test'
import { ObservabilityState } from './pages/observability.page'

// Delete wiring (TXE-7746). The Prometheus tab is backed by the "telemetry" plugin.
type DeletableConnector = 'otel' | 'prometheus'
const PLUGIN_NAME: Record<DeletableConnector, string> = { otel: 'otel', prometheus: 'telemetry' }
// Configs that dial nothing: OTel is stored disabled, and telemetry has no push gateway.
const CONNECTOR_CONFIG: Record<DeletableConnector, { enabled: boolean; config: Record<string, unknown> }> = {
  otel: {
    enabled: false,
    config: {
      profiles: [
        {
          enabled: true,
          traces_enabled: true,
          service_name: 'bifrost-e2e-delete',
          collector_url: 'http://127.0.0.1:4318',
          trace_type: 'genai_extension',
          protocol: 'http',
        },
      ],
    },
  },
  prometheus: { enabled: true, config: { metrics_enabled: true, push_gateway: { enabled: false } } },
}

type StoredPlugin = { name: string; enabled: boolean; config: Record<string, unknown> | null }

async function storedPlugins(request: APIRequestContext): Promise<StoredPlugin[]> {
  const res = await request.get('/api/plugins')
  expect(res.ok()).toBe(true)
  const body = (await res.json()) as { plugins?: StoredPlugin[] }
  return body.plugins ?? []
}

async function storedPluginNames(request: APIRequestContext): Promise<string[]> {
  return (await storedPlugins(request)).map((p) => p.name)
}

// Deleted means: OTel has no row; telemetry keeps a disabled row with an empty config, because
// the server loads telemetry at startup when it has no row (TXE-7746).
async function expectDeleted(request: APIRequestContext, connector: DeletableConnector): Promise<void> {
  const row = (await storedPlugins(request)).find((p) => p.name === PLUGIN_NAME[connector])
  if (connector === 'otel') {
    expect(row).toBeUndefined()
    return
  }
  expect(row, 'telemetry keeps a disabled row').toBeDefined()
  expect(row!.enabled).toBe(false)
  expect(Object.keys(row!.config ?? {})).toHaveLength(0)
}

test.describe('Observability', () => {
  let originalState: ObservabilityState

  test.beforeEach(async ({ observabilityPage }) => {
    await observabilityPage.goto()
    // Capture original state for restoration
    originalState = await observabilityPage.getCurrentState()
  })

  test.afterEach(async ({ observabilityPage }) => {
    // Restore original state - disable all connectors that weren't enabled before
    await observabilityPage.disableAllConnectors()
  })

  test.describe('Navigation', () => {
    test('should display observability page', async ({ observabilityPage }) => {
      // Check for the sidebar section header "Providers" (exact match to avoid strict mode)
      const providersHeader = observabilityPage.page.locator('.text-muted-foreground').filter({ hasText: 'Providers' }).first()
      await expect(providersHeader).toBeVisible()
    })

    test('should display OTel connector tab', async ({ observabilityPage }) => {
      await expect(observabilityPage.getConnectorTab('otel')).toBeVisible()
    })

    test('should display Maxim connector tab', async ({ observabilityPage }) => {
      await expect(observabilityPage.getConnectorTab('maxim')).toBeVisible()
    })

    test('should display Datadog connector tab', async ({ observabilityPage }) => {
      await expect(observabilityPage.getConnectorTab('datadog')).toBeVisible()
    })
  })

  test.describe('OTel Connector', () => {
    test('should select OTel connector', async ({ observabilityPage }) => {
      await observabilityPage.selectConnector('otel')

      // Should see OTel-specific content - check for metrics label or input
      const metricsVisible = await observabilityPage.isMetricsEndpointVisible()
      expect(metricsVisible).toBe(true)
    })

    test('should display metrics endpoint', async ({ observabilityPage }) => {
      await observabilityPage.selectConnector('otel')

      // The metrics endpoint is in an input field with value containing /metrics
      await observabilityPage.enableMetricsExport()
      const metricsValue = await observabilityPage.getMetricsEndpointValue()
      const metricsInput = observabilityPage.page.getByPlaceholder(/v1\/metrics|otel-collector.*metrics/i)
      const placeholder = await metricsInput.getAttribute('placeholder').catch(() => null)
      const hasMetrics =
        (metricsValue != null && metricsValue.includes('/metrics')) ||
        (placeholder != null && placeholder.includes('/metrics'))
      expect(hasMetrics).toBe(true)
    })

    test('should toggle OTel connector', async ({ observabilityPage }) => {
      await observabilityPage.selectConnector('otel')

      // Check if toggle is enabled (not disabled)
      const isToggleEnabled = await observabilityPage.isToggleEnabled('otel')
      // skip-ok: isToggleEnabled waits for the toggle to render and enable
      test.skip(!isToggleEnabled, 'OTel toggle is disabled (requires configuration)')

      const initialState = await observabilityPage.isConnectorEnabled('otel')

      const toggled = await observabilityPage.toggleConnector('otel')
      expect(toggled).toBe(true)

      // Verify toggle state flipped; poll briefly in case UI updates async (form can reset from refetch)
      await expect
        .poll(async () => observabilityPage.isConnectorEnabled('otel'), { timeout: 3000 })
        .toBe(!initialState)
    })

    test('should configure OTel endpoint', async ({ observabilityPage }) => {
      await observabilityPage.selectConnector('otel')

      // The form always holds at least one profile, rendered collapsed: open it to reach its endpoint (TXE-7737).
      await observabilityPage.expandOtelProfile(0)
      const endpointInput = observabilityPage.page.getByTestId('otel-profile-0').getByPlaceholder(/otel-collector/i)
      await expect(endpointInput).toBeVisible()

      const testEndpoint = 'http://test-otel-collector:4317'
      await endpointInput.clear()
      await endpointInput.fill(testEndpoint)

      const value = await endpointInput.inputValue()
      expect(value).toBe(testEndpoint)
    })
  })

  test.describe('Maxim Connector', () => {
    test('should select Maxim connector', async ({ observabilityPage }) => {
      await observabilityPage.selectConnector('maxim')

      // Verify Maxim is selected by checking aria-current
      const selected = await observabilityPage.getSelectedConnector()
      expect(selected).toContain('Maxim')
    })

    test('should toggle Maxim connector', async ({ observabilityPage }) => {
      await observabilityPage.selectConnector('maxim')

      // Check if toggle is enabled
      const isToggleEnabled = await observabilityPage.isToggleEnabled('maxim')
      // skip-ok: isToggleEnabled waits for the toggle to render and enable
      test.skip(!isToggleEnabled, 'Maxim toggle is disabled (requires configuration)')

      const initialState = await observabilityPage.isConnectorEnabled('maxim')

      const toggled = await observabilityPage.toggleConnector('maxim')
      expect(toggled).toBe(true)

      const newState = await observabilityPage.isConnectorEnabled('maxim')
      expect(newState).toBe(!initialState)
    })

    test('should display Maxim configuration form', async ({ observabilityPage }) => {
      await observabilityPage.selectConnector('maxim')

      // Should see a form with configuration elements
      const form = observabilityPage.page.locator('form')
      const formVisible = await form.isVisible().catch(() => false)
      if (formVisible) {
        const hasInputs = await form.locator('input').first().isVisible().catch(() => false)
        const hasSwitches = await form.locator('button[role="switch"]').first().isVisible().catch(() => false)
        expect(hasInputs || hasSwitches).toBe(true)
      } else {
        // Fallback: at minimum expect some configuration inputs
        const inputsVisible = await observabilityPage.page.locator('input').first().isVisible().catch(() => false)
        expect(inputsVisible).toBe(true)
      }
    })
  })

  test.describe('Prometheus Connector', () => {
    test('should select Prometheus connector', async ({ observabilityPage }) => {
      const isAvailable = await observabilityPage.isConnectorAvailable('prometheus')

      // skip-ok: isConnectorAvailable waits for the tab to render and enable
      test.skip(!isAvailable, 'Prometheus connector not available')

      await observabilityPage.selectConnector('prometheus')

      const selected = await observabilityPage.getSelectedConnector()
      expect(selected).toContain('Prometheus')
    })

    test('should display Prometheus configuration when available', async ({ observabilityPage }) => {
      const isAvailable = await observabilityPage.isConnectorAvailable('prometheus')

      // skip-ok: isConnectorAvailable waits for the tab to render and enable
      test.skip(!isAvailable, 'Prometheus connector not available')

      await observabilityPage.selectConnector('prometheus')

      await expect(observabilityPage.getConnectorToggle('prometheus')).toBeVisible()
    })

    test('should toggle Prometheus connector when available', async ({ observabilityPage }) => {
      const isAvailable = await observabilityPage.isConnectorAvailable('prometheus')

      // skip-ok: isConnectorAvailable waits for the tab to render and enable
      test.skip(!isAvailable, 'Prometheus connector not available')

      await observabilityPage.selectConnector('prometheus')

      const isToggleEnabled = await observabilityPage.isToggleEnabled('prometheus')
      // skip-ok: isToggleEnabled waits for the toggle to render and enable
      test.skip(!isToggleEnabled, 'Prometheus toggle is disabled')

      const initialState = await observabilityPage.isConnectorEnabled('prometheus')
      const toggled = await observabilityPage.toggleConnector('prometheus')
      expect(toggled).toBe(true)

      const newState = await observabilityPage.isConnectorEnabled('prometheus')
      expect(newState).toBe(!initialState)
    })
  })

  test.describe('Connector delete', () => {
    for (const connector of ['otel', 'prometheus'] as const) {
      test(`should delete a configured ${connector} connector`, async ({ observabilityPage, page }) => {
        const pluginName = PLUGIN_NAME[connector]
        // Configure the connector through the API, then load the page so it reads the stored row.
        const put = await page.request.put(`/api/plugins/${pluginName}`, { data: CONNECTOR_CONFIG[connector] })
        expect(put.ok(), await put.text()).toBe(true)
        expect(await storedPluginNames(page.request)).toContain(pluginName)

        await observabilityPage.goto()
        await observabilityPage.selectConnector(connector)
        const otelServiceName = observabilityPage.page.getByTestId('otel-profile-0').getByPlaceholder('bifrost', { exact: true })
        if (connector === 'otel') {
          await observabilityPage.expandOtelProfile(0)
          await expect(otelServiceName).toHaveValue('bifrost-e2e-delete')
        }
        const deleteBtn = observabilityPage.getConnectorDeleteBtn(connector)
        await expect(deleteBtn).toBeVisible()
        await deleteBtn.click()

        const dialog = observabilityPage.deleteDialog
        await expect(dialog).toBeVisible()
        if (connector === 'prometheus') {
          // Deleting Prometheus deletes the telemetry plugin, which also serves /metrics.
          await expect(dialog).toContainText('/metrics')
        }

        // Cancel leaves the connector in place.
        await dialog.getByTestId('observability-connector-delete-cancel').click()
        await expect(dialog).toBeHidden()
        expect(await storedPluginNames(page.request)).toContain(pluginName)

        await deleteBtn.click()
        const deleted = page.waitForResponse(
          (r) => r.request().method() === 'DELETE' && new URL(r.url()).pathname === `/api/plugins/${pluginName}`,
        )
        await observabilityPage.deleteDialog.getByTestId('observability-connector-delete-confirm').click()
        expect((await deleted).ok()).toBe(true)
        await expect(observabilityPage.deleteDialog).toBeHidden()

        // Deleted on the server, and the page no longer offers a delete for it.
        await expectDeleted(page.request, connector)
        await expect(deleteBtn).toBeHidden()
        if (connector === 'otel') {
          // The form no longer holds the deleted profile, so a save cannot recreate it.
          await observabilityPage.expandOtelProfile(0)
          await expect(otelServiceName).not.toHaveValue('bifrost-e2e-delete')
        }
        if (connector === 'prometheus') {
          // The stored telemetry row is off, so the form no longer shows /metrics as served.
          await expect(observabilityPage.getConnectorToggle('prometheus')).toHaveAttribute('data-state', 'unchecked')
        }
      })
    }

    test('should not offer delete for an unconfigured otel connector', async ({ observabilityPage, page }) => {
      if ((await storedPluginNames(page.request)).includes('otel')) {
        const res = await page.request.delete('/api/plugins/otel')
        expect(res.ok(), await res.text()).toBe(true)
      }
      await observabilityPage.goto()
      await observabilityPage.selectConnector('otel')
      await expect(observabilityPage.page.getByTestId('otel-connector-enable-toggle')).toBeVisible()
      await expect(observabilityPage.getConnectorDeleteBtn('otel')).toHaveCount(0)
    })
  })

  test.describe('BigQuery Connector', () => {
    test('should select BigQuery connector', async ({ observabilityPage }) => {
      const isAvailable = await observabilityPage.isConnectorAvailable('bigquery')

      // skip-ok: isConnectorAvailable waits for the tab to render and enable
      test.skip(!isAvailable, 'BigQuery connector not available')

      await observabilityPage.selectConnector('bigquery')

      const selected = await observabilityPage.getSelectedConnector()
      expect(selected).toContain('BigQuery')
    })
  })

  test.describe('Datadog Connector', () => {
    test('should select Datadog connector if available', async ({ observabilityPage }) => {
      const isAvailable = await observabilityPage.isConnectorAvailable('datadog')

      // skip-ok: isConnectorAvailable waits for the tab to render and enable
      test.skip(!isAvailable, 'Datadog connector not available (enterprise feature)')

      await observabilityPage.selectConnector('datadog')

      // Datadog view should be displayed
      const selected = await observabilityPage.getSelectedConnector()
      expect(selected).toContain('Datadog')
    })

    test('should toggle Datadog connector if available', async ({ observabilityPage }) => {
      const isAvailable = await observabilityPage.isConnectorAvailable('datadog')

      // skip-ok: isConnectorAvailable waits for the tab to render and enable
      test.skip(!isAvailable, 'Datadog connector not available (enterprise feature)')

      await observabilityPage.selectConnector('datadog')

      const isToggleEnabled = await observabilityPage.isToggleEnabled('datadog')
      // skip-ok: isToggleEnabled waits for the toggle to render and enable
      test.skip(!isToggleEnabled, 'Datadog toggle is disabled')

      const initialState = await observabilityPage.isConnectorEnabled('datadog')
      const toggled = await observabilityPage.toggleConnector('datadog')

      // A toggle that isToggleEnabled found enabled must toggle: fail rather than skip if it does not.
      expect(toggled, 'Datadog toggle could not be toggled').toBe(true)

      const newState = await observabilityPage.isConnectorEnabled('datadog')
      expect(newState).toBe(!initialState)
    })
  })

  test.describe('Multiple Connectors', () => {
    test('should switch between connectors', async ({ observabilityPage }) => {
      // Start with OTel
      await observabilityPage.selectConnector('otel')
      let selected = await observabilityPage.getSelectedConnector()
      expect(selected).toContain('Open Telemetry')

      // Switch to Maxim
      await observabilityPage.selectConnector('maxim')
      selected = await observabilityPage.getSelectedConnector()
      expect(selected).toContain('Maxim')

      // Switch back to OTel
      await observabilityPage.selectConnector('otel')
      selected = await observabilityPage.getSelectedConnector()
      expect(selected).toContain('Open Telemetry')
    })

    test('should persist connector selection via URL', async ({ observabilityPage }) => {
      // Select Maxim (URL update via nuqs is async)
      await observabilityPage.selectConnector('maxim')
      // Wait for URL to reflect selection before asserting
      await expect(observabilityPage.page).toHaveURL(/plugin=maxim/, { timeout: 5000 })
    })
  })
})
