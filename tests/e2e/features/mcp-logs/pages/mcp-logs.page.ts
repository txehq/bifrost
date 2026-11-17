import { Locator, Page, expect } from '@playwright/test'
import { BasePage } from '../../../core/pages/base.page'
import { waitForNetworkIdle } from '../../../core/utils/test-helpers'

/**
 * Page object for the MCP Logs page
 */
export class MCPLogsPage extends BasePage {
  // Main elements
  readonly logsTable: Locator
  readonly filtersSection: Locator
  readonly filtersButton: Locator
  readonly statsCards: Locator

  // Filter elements
  readonly toolNameFilter: Locator
  readonly serverLabelFilter: Locator
  readonly statusFilter: Locator
  readonly searchInput: Locator
  readonly dateRangePicker: Locator
  readonly liveToggle: Locator

  // Table elements
  readonly tableRows: Locator
  readonly paginationControls: Locator
  readonly nextPageBtn: Locator
  readonly prevPageBtn: Locator

  // Log detail sheet
  readonly logDetailSheet: Locator
  readonly closeDetailSheetBtn: Locator

  constructor(page: Page) {
    super(page)

    // Main elements
    this.logsTable = page.locator('[data-testid="mcp-logs-table"]').or(page.locator('table'))
    // The filters section is the container with search input and filters button
    this.filtersSection = page.locator('input[placeholder="Search MCP logs"]').locator('..')
    // The filters live in a sidebar (mcpFilterSidebar.tsx): "Hide filters" while it is open, "Show
    // filters" on the rail when it is collapsed.
    this.filtersButton = page.getByRole('button', { name: /^(Hide|Show) filters$/ }).first()
    this.statsCards = page.locator('[data-testid="mcp-stats-cards"]').or(
      page.getByText('Total Records', { exact: true }).locator('..').locator('..')
    )

    // Filter elements - each is a collapsible section of the sidebar, opened by its title button
    this.toolNameFilter = page.getByRole('button', { name: 'Tool Names', exact: true })
    this.serverLabelFilter = page.getByRole('button', { name: 'Servers', exact: true })
    this.statusFilter = page.getByRole('button', { name: 'Status', exact: true })
    this.searchInput = page.locator('[data-testid="filter-search"]').or(
      page.getByPlaceholder('Search MCP logs')
    )
    this.dateRangePicker = page.locator('[data-testid="filter-date-range"]').or(
      page.locator('button').filter({ hasText: /Last/i })
    )
    this.liveToggle = page.getByTestId('mcp-logs-header-live-btn')

    // Table elements - exclude status message rows
    this.tableRows = this.logsTable.locator('tbody tr').filter({ hasNot: page.getByTestId('logs-table-status-row') }).filter({ hasNot: page.getByText(/Waiting for new/) }).filter({ hasNot: page.locator('text=Listening for') }).filter({ hasNot: page.locator('text=Live updates paused') }).filter({ hasNot: page.locator('text=Not connected') }).filter({ hasNot: page.locator('text=No results found') })
    // Scope pagination to the MCP logs view (avoid matching other pages when navigating)
    const paginationContainer = page.getByTestId('pagination').filter({ has: page.locator('[data-testid="next-page"]') }).first()
    this.paginationControls = paginationContainer
    this.nextPageBtn = paginationContainer.getByRole('button', { name: 'Next page' }).or(
      paginationContainer.locator('[data-testid="next-page"]')
    )
    this.prevPageBtn = paginationContainer.getByRole('button', { name: 'Previous page' }).or(
      paginationContainer.locator('[data-testid="prev-page"]')
    )

    // Log detail sheet - Sheet component with role="dialog"
    this.logDetailSheet = page.locator('[role="dialog"]')
    this.closeDetailSheetBtn = page.locator('[role="dialog"]').locator('button').filter({ has: page.locator('svg.lucide-x') })
  }

  /**
   * Navigate to the MCP logs page
   */
  async goto(): Promise<void> {
    await this.page.goto('/workspace/mcp-logs')
    // With no MCP logs the page shows a "Get Started" guide instead of the table.
    await this.logsTable.or(this.page.getByText(/Get Started/i)).first().waitFor({ state: 'visible', timeout: 10000 })
  }

  /**
   * Open the filters sidebar if it is collapsed (the collapsed state persists in localStorage).
   */
  private async openFiltersSidebar(): Promise<void> {
    const show = this.page.getByRole('button', { name: 'Show filters' }).first()
    if (await show.isVisible()) await show.click()
    await expect(this.page.getByRole('button', { name: 'Hide filters' })).toBeVisible()
  }

  /**
   * Open a sidebar section by its title button and return its checkboxes. Tool Names and Servers
   * fetch their options when they open; the caller waits for them.
   */
  private async openFilterSection(title: Locator): Promise<Locator> {
    await this.openFiltersSidebar()
    await title.waitFor({ state: 'visible' })
    if ((await title.getAttribute('aria-expanded')) !== 'true') await title.click()
    await expect(title).toHaveAttribute('aria-expanded', 'true')
    // The section is the Collapsible root that holds the title button and its content.
    return title.locator('xpath=..').getByRole('checkbox')
  }

  /**
   * Check the first option of a section that loads its options (Tool Names, Servers).
   * @returns false when the section has no options once they have loaded
   */
  private async checkFirstOption(title: Locator): Promise<boolean> {
    const options = await this.openFilterSection(title)
    // The options load after the section opens; give them time, so "none" means none (TXE-7733).
    await options.first().waitFor({ state: 'visible', timeout: 10000 }).catch(() => {})
    if ((await options.count()) === 0) return false
    await options.first().check()
    return true
  }

  /**
   * Filter by tool name: check the first option in the Tool Names section.
   * @returns true if at least one tool name option was found and checked
   */
  async filterByToolName(): Promise<boolean> {
    return this.checkFirstOption(this.toolNameFilter)
  }

  /**
   * Filter by server label: check the first option in the Servers section.
   * @returns true if at least one server label option was found and checked
   */
  async filterByServerLabel(): Promise<boolean> {
    return this.checkFirstOption(this.serverLabelFilter)
  }

  /**
   * Filter by status: check the given option in the Status section.
   * @returns true if the option was found and checked
   */
  async filterByStatus(status: 'success' | 'error' | 'processing' | 'cancelled'): Promise<boolean> {
    const options = await this.openFilterSection(this.statusFilter)
    const option = options.and(this.page.getByRole('checkbox', { name: status, exact: true }))
    if ((await option.count()) === 0) return false
    await option.check()
    return true
  }

  /**
   * Search logs by content
   */
  async searchLogs(query: string): Promise<void> {
    await this.searchInput.fill(query)
    // Wait for debounced search to trigger network request
    await waitForNetworkIdle(this.page)
  }

  /**
   * Clear search
   */
  async clearSearch(): Promise<void> {
    await this.searchInput.clear()
    await waitForNetworkIdle(this.page)
  }

  /**
   * Select time period
   */
  async selectTimePeriod(period: '1h' | '6h' | '24h' | '7d' | '30d'): Promise<void> {
    await this.dateRangePicker.first().click()
    await this.page.waitForSelector('[role="listbox"], [role="menu"]', { timeout: 5000 }).catch(() => {})

    const periodLabels: Record<string, string> = {
      '1h': 'Last hour',
      '6h': 'Last 6 hours',
      '24h': 'Last 24 hours',
      '7d': 'Last 7 days',
      '30d': 'Last 30 days',
    }

    const periodButton = this.page.getByRole('button', { name: periodLabels[period] })
    if (await periodButton.count() > 0) {
      await periodButton.click()
    } else {
      await this.page.keyboard.press('Escape')
    }

    await waitForNetworkIdle(this.page)
  }

  /**
   * Toggle live updates
   */
  async toggleLiveUpdates(): Promise<void> {
    await this.liveToggle.first().waitFor({ state: 'visible' })
    await this.liveToggle.first().click()
  }

  /**
   * Click on a log row to view details
   */
  async viewLogDetails(rowIndex: number = 0): Promise<void> {
    const rows = this.tableRows
    const count = await rows.count()

    if (count <= rowIndex) {
      throw new Error(`Row index ${rowIndex} out of bounds (${count} rows available)`)
    }
    await rows.nth(rowIndex).click()
    await expect(this.logDetailSheet).toBeVisible({ timeout: 5000 })
  }

  /**
   * Close log detail sheet
   */
  async closeLogDetails(): Promise<void> {
    if (await this.logDetailSheet.isVisible()) {
      await this.closeDetailSheetBtn.click().catch(async () => {
        await this.page.keyboard.press('Escape')
      })
      await expect(this.logDetailSheet).not.toBeVisible({ timeout: 5000 })
    }
  }

  /**
   * Get log count from table
   */
  async getLogCount(): Promise<number> {
    return await this.tableRows.count()
  }

  /**
   * Check if log exists in table
   */
  async logExists(searchText: string): Promise<boolean> {
    const row = this.tableRows.filter({ hasText: searchText })
    return await row.count() > 0
  }

  /**
   * Get current 1-based page number from URL (offset/limit).
   */
  getCurrentPageNumber(): number {
    const url = this.page.url()
    const params = new URL(url).searchParams
    const offset = Number.parseInt(params.get('offset') ?? '0', 10)
    const limit = Number.parseInt(params.get('limit') ?? '50', 10) || 50
    return Math.floor(offset / limit) + 1
  }

  /**
   * Navigate to next page (waits for URL to update)
   */
  async goToNextPage(): Promise<void> {
    const btn = this.nextPageBtn.first()
    const isEnabled = await btn.isEnabled().catch(() => false)
    if (!isEnabled) return
    await btn.scrollIntoViewIfNeeded()
    await btn.waitFor({ state: 'visible' })
    const limit = Number.parseInt(new URL(this.page.url()).searchParams.get('limit') ?? '50', 10) || 50
    const currentOffset = Number.parseInt(new URL(this.page.url()).searchParams.get('offset') ?? '0', 10)
    const expectedOffset = currentOffset + limit
    await btn.click()
    await this.page.waitForURL((url) => {
      const params = new URL(url).searchParams
      const offset = params.get('offset')
      return offset === String(expectedOffset)
    }, { timeout: 10000 })
    await waitForNetworkIdle(this.page)
  }

  /**
   * Navigate to previous page (waits for URL to update)
   */
  async goToPreviousPage(): Promise<void> {
    const btn = this.prevPageBtn.first()
    const isEnabled = await btn.isEnabled().catch(() => false)
    if (!isEnabled) return
    await btn.scrollIntoViewIfNeeded()
    await btn.waitFor({ state: 'visible' })
    const limit = Number.parseInt(new URL(this.page.url()).searchParams.get('limit') ?? '50', 10) || 50
    const currentOffset = Number.parseInt(new URL(this.page.url()).searchParams.get('offset') ?? '0', 10)
    const expectedOffset = Math.max(0, currentOffset - limit)
    await btn.click()
    await this.page.waitForURL((url) => {
      const params = new URL(url).searchParams
      const offset = params.get('offset')
      // When going back to page 1, offset param may be removed (null) or set to "0"
      if (expectedOffset === 0) return offset === null || offset === '0'
      return offset === String(expectedOffset)
    }, { timeout: 10000 })
    await waitForNetworkIdle(this.page)
  }

  /**
   * Sort table by column - clicks the sort button in the column header
   */
  async sortBy(column: 'timestamp' | 'latency'): Promise<void> {
    await this.dismissToasts()

    // Map column names to header button text
    const columnLabels: Record<string, string> = {
      'timestamp': 'Time',
      'latency': 'Latency'
    }

    const label = columnLabels[column] || column
    // The sortable column headers have a button with the column name
    const sortButton = this.logsTable.getByRole('button', { name: new RegExp(label, 'i') })

    if (await sortButton.count() > 0) {
      await sortButton.first().waitFor({ state: 'visible' })
      await sortButton.first().click()
      await waitForNetworkIdle(this.page)
    }
  }

  /**
   * Check if stats cards are visible
   */
  async areStatsVisible(): Promise<boolean> {
    const statsText = this.page.getByText('Total Records', { exact: true })
    return await statsText.isVisible().catch(() => false)
  }

  /**
   * Check if empty state is shown
   */
  async isEmptyStateVisible(): Promise<boolean> {
    const emptyState = this.page.locator('text=/No logs found/i').or(
      this.page.locator('text=/No data/i')
    )
    return await emptyState.isVisible().catch(() => false)
  }

  /**
   * Get sort state for a column from URL parameters
   * Returns 'asc', 'desc', or null if column is not the current sort column
   */
  async getSortState(column: 'timestamp' | 'latency'): Promise<string | null> {
    const url = this.page.url()
    const urlParams = new URL(url).searchParams
    const sortBy = urlParams.get('sort_by')
    const order = urlParams.get('order')

    // Check if this column is the currently sorted column
    if (sortBy === column) {
      return order || 'desc' // default is desc
    }
    return null
  }
}
