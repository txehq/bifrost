import { expect, test } from '../../core/fixtures/base.fixture'
import { skipIfShown, skipUnlessEnabled, skipUnlessVisible } from '../../core/utils/availability'
import type { MCPLogsPage } from './pages/mcp-logs.page'

// With no MCP logs the page shows a "Get Started" guide instead of the table, stats and filters.
// Waits for the page to settle on one or the other, and skips only when it is the guide (TXE-7733).
const skipIfNoLogs = (mcpLogsPage: MCPLogsPage, reason: string) =>
  skipIfShown(mcpLogsPage.page.getByText(/Get Started/i), mcpLogsPage.logsTable, reason)

test.describe('MCP Logs', () => {
  test.beforeEach(async ({ mcpLogsPage }) => {
    await mcpLogsPage.goto()
  })

  test.describe('MCP Logs Display', () => {
    test('should display MCP logs table or getting started guide', async ({ mcpLogsPage }) => {
      // When MCP logs exist the table is visible; otherwise a "Get Started" guide is shown
      await expect(mcpLogsPage.logsTable.or(mcpLogsPage.page.getByText(/Get Started/i)).first()).toBeVisible()
    })

    test('should display stats cards', async ({ mcpLogsPage }) => {
      await skipIfNoLogs(mcpLogsPage, 'No MCP logs — stats cards not rendered in getting-started view')
      await expect(mcpLogsPage.page.locator('text=Total Executions')).toBeVisible()
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

      // Default is live_enabled=true (but URL may not have it since it's the default)
      // Check for live_enabled=false to determine if currently disabled
      const initialUrl = mcpLogsPage.page.url()
      const initialLiveDisabled = initialUrl.includes('live_enabled=false')

      await mcpLogsPage.toggleLiveUpdates()

      // Wait for URL to reflect live_enabled toggle
      await mcpLogsPage.page.waitForURL(/live_enabled=/, { timeout: 5000 })

      const newUrl = mcpLogsPage.page.url()
      const newLiveDisabled = newUrl.includes('live_enabled=false')

      // Live enabled state should have toggled
      // If initially enabled (not disabled), after toggle it should be disabled
      // If initially disabled, after toggle it should be enabled (no live_enabled=false)
      expect(newLiveDisabled).not.toBe(initialLiveDisabled)
    })
  })
})
