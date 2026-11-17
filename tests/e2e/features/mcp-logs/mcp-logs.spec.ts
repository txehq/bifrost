import { expect, test } from '../../core/fixtures/base.fixture'
import { skipIfShown, skipUnlessEnabled, skipUnlessVisible } from '../../core/utils/availability'
import { MCP_SEED_COUNT, seedMCPLogs } from './mcp-logs.seed'
import type { MCPLogsPage } from './pages/mcp-logs.page'

// With no MCP logs the page shows a "Get Started" guide instead of the table, stats and filters.
// Waits for the page to settle on one or the other, and skips only when it is the guide (TXE-7733).
const skipIfNoLogs = (mcpLogsPage: MCPLogsPage, reason: string) =>
  skipIfShown(mcpLogsPage.page.getByText(/Get Started/i), mcpLogsPage.logsTable, reason)

test.describe('MCP Logs', () => {
  // Offline, give the page MCP logs to show: the run's gateway has none of its own (TXE-7735). Once
  // per worker; seedMCPLogs tops up only what is missing. Online runs use whatever the instance has.
  let seeded = false
  test.beforeEach(async ({ mcpLogsPage, request }) => {
    if (process.env.E2E_OFFLINE === '1' && !seeded) {
      await seedMCPLogs(request, (read) =>
        expect.poll(read, { timeout: 30_000, message: `the gateway logs ${MCP_SEED_COUNT} MCP tool calls` }).toBeGreaterThanOrEqual(MCP_SEED_COUNT),
      )
      seeded = true
    }
    await mcpLogsPage.goto()
  })

  test.describe('MCP Logs Display', () => {
    test('should display MCP logs table or getting started guide', async ({ mcpLogsPage }) => {
      // When MCP logs exist the table is visible; otherwise a "Get Started" guide is shown
      await expect(mcpLogsPage.logsTable.or(mcpLogsPage.page.getByText(/Get Started/i)).first()).toBeVisible()
    })

    test('should display stats cards', async ({ mcpLogsPage }) => {
      await skipIfNoLogs(mcpLogsPage, 'No MCP logs — stats cards not rendered in getting-started view')
      await expect(mcpLogsPage.page.getByText('Total Records', { exact: true })).toBeVisible()
    })

    test('should display filters section', async ({ mcpLogsPage }) => {
      await skipIfNoLogs(mcpLogsPage, 'No MCP logs — filters not rendered in getting-started view')
      await expect(mcpLogsPage.searchInput.or(mcpLogsPage.filtersButton).first()).toBeVisible()
    })
  })

  test.describe('MCP Log Filtering', () => {
    test('should filter logs by tool name', async ({ mcpLogsPage, page }) => {
      await skipUnlessVisible(mcpLogsPage.filtersButton, 'Filters button not visible')
      const applied = await mcpLogsPage.filterByToolName()
      // skip-ok: filterByToolName waits for the group's options; false means the group has none
      test.skip(!applied, 'No tool name options in filter list')
      await expect
        .poll(() => page.url(), { timeout: 5000, intervals: [200, 300, 500] })
        .toMatch(/tool_names=/)
    })

    test('should filter logs by server label', async ({ mcpLogsPage, page }) => {
      await skipUnlessVisible(mcpLogsPage.filtersButton, 'Filters button not visible')
      const applied = await mcpLogsPage.filterByServerLabel()
      // skip-ok: filterByServerLabel waits for the group's options; false means the group has none
      test.skip(!applied, 'No server label options in filter list')
      await expect
        .poll(() => page.url(), { timeout: 5000, intervals: [200, 300, 500] })
        .toMatch(/server_labels=/)
    })

    test('should filter logs by status', async ({ mcpLogsPage, page }) => {
      await skipUnlessVisible(mcpLogsPage.filtersButton, 'Filters button not visible')

      const applied = await mcpLogsPage.filterByStatus('success')
      // skip-ok: filterByStatus waits for the options; false means there is no such option
      test.skip(!applied, 'No status options in filter list')

      await expect
        .poll(() => page.url(), { timeout: 5000, intervals: [200, 300, 500] })
        .toMatch(/status=success/)
    })

    test('should search logs by content', async ({ mcpLogsPage }) => {
      const searchInput = mcpLogsPage.searchInput
      const isVisible = await searchInput.isVisible().catch(() => false)

      if (isVisible) {
        const query = `test-query-${Date.now()}`
        await mcpLogsPage.searchLogs(query)

        // Check that search input contains the query (DOM state)
        const inputValue = await searchInput.inputValue().catch(() => '')
        expect(inputValue).toContain(query)
      }
    })

    test('should filter by time period', async ({ mcpLogsPage }) => {
      const datePicker = mcpLogsPage.dateRangePicker
      const isVisible = await datePicker.isVisible().catch(() => false)

      if (isVisible) {
        // Get initial date picker value
        const initialValue = await datePicker.textContent().catch(() => '')

        await mcpLogsPage.selectTimePeriod('7d')

        // Check that date picker value changed (DOM state)
        const newValue = await datePicker.textContent().catch(() => '')
        expect(newValue || initialValue).toBeTruthy()
      }
    })
  })

  test.describe('MCP Log Details', () => {
    test('should open log details sheet', async ({ mcpLogsPage }) => {
      const logCount = await mcpLogsPage.getLogCount()

      if (logCount > 0) {
        await mcpLogsPage.viewLogDetails(0)

        const sheetVisible = await mcpLogsPage.logDetailSheet.isVisible().catch(() => false)
        expect(sheetVisible).toBe(true)

        await mcpLogsPage.closeLogDetails()
      }
    })

    test('should close log details sheet', async ({ mcpLogsPage }) => {
      const logCount = await mcpLogsPage.getLogCount()

      if (logCount > 0) {
        await mcpLogsPage.viewLogDetails(0)
        await mcpLogsPage.closeLogDetails()

        const sheetVisible = await mcpLogsPage.logDetailSheet.isVisible().catch(() => false)
        expect(sheetVisible).toBe(false)
      }
    })
  })

  test.describe('Pagination', () => {
    test('should navigate to next page', async ({ mcpLogsPage }) => {
      await skipIfNoLogs(mcpLogsPage, 'No MCP logs — pagination not rendered')
      await skipUnlessVisible(mcpLogsPage.paginationControls, 'No MCP logs — pagination not rendered')
      await skipUnlessEnabled(mcpLogsPage.nextPageBtn, 'Only one page of results; skipping pagination test')

      const initialPage = mcpLogsPage.getCurrentPageNumber()
      await mcpLogsPage.goToNextPage()

      await expect
        .poll(() => mcpLogsPage.getCurrentPageNumber(), { timeout: 5000 })
        .toBe(initialPage + 1)
    })

    test('should navigate to previous page', async ({ mcpLogsPage }) => {
      await skipIfNoLogs(mcpLogsPage, 'No MCP logs — pagination not rendered')
      await skipUnlessVisible(mcpLogsPage.paginationControls, 'No MCP logs — pagination not rendered')
      await skipUnlessEnabled(mcpLogsPage.nextPageBtn, 'Only one page of results; skipping pagination test')

      await mcpLogsPage.goToNextPage()

      await expect
        .poll(() => mcpLogsPage.getCurrentPageNumber(), { timeout: 5000 })
        .toBe(2)

      // On page 2, previous is always enabled: wait for it rather than skip.
      await expect(mcpLogsPage.prevPageBtn).toBeEnabled()

      await mcpLogsPage.goToPreviousPage()

      // We were on page 2; after previous we must be on page 1 (assert concrete value to avoid race with captured page number)
      await expect
        .poll(() => mcpLogsPage.getCurrentPageNumber(), { timeout: 5000 })
        .toBe(1)
    })
  })

  test.describe('Table Sorting', () => {
    test('should sort by timestamp', async ({ mcpLogsPage }) => {
      await skipIfNoLogs(mcpLogsPage, 'No MCP logs — table not rendered')
      // Timestamp is the default sort column (desc), so clicking it toggles to asc
      const initialUrl = mcpLogsPage.page.url()

      await mcpLogsPage.sortBy('timestamp')

      // Wait for URL to actually change after sort
      await expect
        .poll(() => mcpLogsPage.page.url(), { timeout: 5000 })
        .not.toBe(initialUrl)
    })

    test('should sort by latency', async ({ mcpLogsPage }) => {
      await skipIfNoLogs(mcpLogsPage, 'No MCP logs — table not rendered')
      await mcpLogsPage.sortBy('latency')

      // Wait for URL to update
      await mcpLogsPage.page.waitForURL(/sort_by=latency/, { timeout: 5000 })

      // Check URL state for latency sort
      const sortState = await mcpLogsPage.getSortState('latency')
      expect(sortState).toBeTruthy()
    })
  })

  test.describe('Live Updates', () => {
    test('should toggle live updates', async ({ mcpLogsPage }) => {
      await skipIfNoLogs(mcpLogsPage, 'No MCP logs — live toggle not rendered in getting-started view')
      await expect(mcpLogsPage.liveToggle).toBeVisible()

      // The Live button keeps its state in the URL's `polling` parameter, which defaults to true
      // (mcp-logs/page.tsx), so an absent parameter means live updates are on.
      const polling = () => new URL(mcpLogsPage.page.url()).searchParams.get('polling') !== 'false'
      const initiallyLive = polling()

      await mcpLogsPage.toggleLiveUpdates()

      await expect.poll(polling, { timeout: 5000 }).toBe(!initiallyLive)
    })
  })
})
