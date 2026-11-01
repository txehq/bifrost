import type { Page } from '@playwright/test'
import { expect, test } from '../../core/fixtures/base.fixture'

// The dock keeps the thread, the conversation id and any pending question
// across a close, but the in-flight answer used to live in the panel and die
// with it: closing the dock mid-answer aborted the request, and reopening
// showed the question with nothing after it. The answer now belongs to the
// provider and finishes whether or not the panel is on screen.

const configuredWarp = {
  configured: true,
  enabled: true,
  provider: 'openai',
  model: 'gpt-5.6-luna',
  max_iterations: 8,
  request_timeout_seconds: 120,
  history_retention_days: 30,
  embedding_provider: 'openai',
  embedding_model: 'text-embedding-3-small',
  embedding_dimension: 1536,
  log_vector_store_namespace: 'BifrostWarpLogs',
  semantic_search_threshold: 0.5,
  semantic_search_limit: 10,
  vector_store_connected: true,
}

function sse(answer: string): string {
  const frames = [
    { type: 'start' },
    { type: 'delta', delta: answer },
    { type: 'done', finish_reason: 'stop', conversation_id: '00000000-0000-4000-8000-000000000002' },
  ]
  return frames.map((frame) => `event: ${frame.type}\ndata: ${JSON.stringify(frame)}\n\n`).join('')
}

// The warp flag is shared server state that other suites toggle in parallel,
// and it only gates the UI. Turning it on in this page's view of the flags
// leaves the server's copy alone, so no suite races another over it.
async function enableWarpFlag(page: Page) {
  await page.route('**/api/feature-flags', async (route) => {
    if (route.request().method() !== 'GET') return route.continue()
    const response = await route.fetch()
    const body = (await response.json()) as { flags: { id: string; enabled: boolean }[] }
    body.flags = body.flags.map((flag) => (flag.id === 'warp' ? { ...flag, enabled: true } : flag))
    await route.fulfill({ response, json: body })
  })
}

async function mockWarpConfig(page: Page, additionalModels: { provider: string; model: string }[] = []) {
  await page.route('**/api/warp/config', (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ ...configuredWarp, additional_models: additionalModels }),
    }),
  )
  await page.route('**/api/warp/log-index/status', (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ state: 'ready', vector_store_connected: true, embedding_configured: true }),
    }),
  )
}

test.describe('Warp dock and an in-flight answer', () => {
  test.beforeEach(async ({ dashboardPage }) => {
    await enableWarpFlag(dashboardPage.page)
    await dashboardPage.goto()
    await expect(dashboardPage.pageTitle).toBeVisible()
  })

  test('closing the dock mid-answer lets the answer finish and land in the thread', async ({ dashboardPage }) => {
    const page = dashboardPage.page
    await mockWarpConfig(page)
    // The chat request is held open until the test has closed the dock, so the
    // answer can only arrive while the panel is unmounted.
    let release!: () => void
    const dockClosed = new Promise<void>((resolve) => {
      release = resolve
    })
    await page.route('**/api/warp/chat', async (route) => {
      await dockClosed
      // Before the fix the close aborted this request; fulfilling an aborted
      // route throws, and the assertion below is what reports it.
      await route.fulfill({ status: 200, contentType: 'text/event-stream', body: sse('Two requests hit a 429.') }).catch(() => {})
    })
    await page.reload()

    await page.getByTestId('topbar-warp-btn').click()
    const composer = page.getByTestId('warp-composer-input')
    await expect(composer).toBeVisible()
    await composer.fill('why did the requests fail?')
    await composer.press('Enter')
    await expect(page.getByTestId('warp-stop-btn')).toBeVisible()

    await page.getByTestId('warp-close-btn').click()
    await expect(page.getByTestId('warp-panel')).toHaveCount(0)
    release()

    await page.getByTestId('topbar-warp-btn').click()
    await expect(page.getByTestId('warp-message-user')).toHaveText('why did the requests fail?')
    await expect(page.getByTestId('warp-message-assistant')).toContainText('Two requests hit a 429.')
    // The answer is filed, not still streaming, so the composer is ready again.
    await expect(page.getByTestId('warp-stop-btn')).toHaveCount(0)
  })
})

// What a failed turn cost and what a question offered both have to survive into the next
// exchange, or the spend goes unshown and the model cannot tell what the reply answers.
test.describe('Warp turn state across an exchange', () => {
  test.beforeEach(async ({ dashboardPage }) => {
    await enableWarpFlag(dashboardPage.page)
    await dashboardPage.goto()
    await expect(dashboardPage.pageTitle).toBeVisible()
  })

  const frames = (...list: object[]) => list.map((frame) => `data: ${JSON.stringify(frame)}\n\n`).join('')

  test('a failed turn shows what it spent and the retry continues its thread', async ({ dashboardPage }) => {
    const page = dashboardPage.page
    await mockWarpConfig(page)
    const thread = '00000000-0000-4000-8000-000000000003'
    const bodies: { conversation_id?: string }[] = []
    await page.route('**/api/warp/chat', async (route) => {
      bodies.push(route.request().postDataJSON())
      const body =
        bodies.length === 1
          ? frames({ type: 'error', code: 'upstream_error', message: 'boom', conversation_id: thread, usage: { prompt_tokens: 900, completion_tokens: 334, total_tokens: 1234 } })
          : frames({ type: 'delta', delta: 'Recovered.' }, { type: 'done', finish_reason: 'stop', conversation_id: thread })
      await route.fulfill({ status: 200, contentType: 'text/event-stream', body })
    })
    await page.reload()

    await page.getByTestId('topbar-warp-btn').click()
    const composer = page.getByTestId('warp-composer-input')
    await composer.fill('why did the requests fail?')
    await composer.press('Enter')
    await expect(page.getByTestId('warp-turn-error')).toBeVisible()
    await expect(page.getByTestId('warp-usage')).toContainText('1,234')

    await composer.fill('try again')
    await composer.press('Enter')
    await expect(page.getByTestId('warp-message-assistant').last()).toContainText('Recovered.')
    expect(bodies[1].conversation_id).toBe(thread)
  })

  test('a question asked with narration replays with its choices', async ({ dashboardPage }) => {
    const page = dashboardPage.page
    await mockWarpConfig(page)
    const question = { question: 'Which time range?', kind: 'time_range', options: [{ label: 'Last 7 days', hint: '-7d' }, { label: 'Today', hint: 'today' }] }
    const bodies: { messages: { role: string; content: string; question?: boolean }[] }[] = []
    await page.route('**/api/warp/chat', async (route) => {
      bodies.push(route.request().postDataJSON())
      const body =
        bodies.length === 1
          ? frames({ type: 'delta', delta: 'Let me narrow this down.' }, { type: 'question', question }, { type: 'done', finish_reason: 'question', conversation_id: '00000000-0000-4000-8000-000000000004' })
          : frames({ type: 'delta', delta: 'Seven failed.' }, { type: 'done', finish_reason: 'stop', conversation_id: '00000000-0000-4000-8000-000000000004' })
      await route.fulfill({ status: 200, contentType: 'text/event-stream', body })
    })
    await page.reload()

    await page.getByTestId('topbar-warp-btn').click()
    const composer = page.getByTestId('warp-composer-input')
    await composer.fill('how many failed?')
    await composer.press('Enter')
    await expect(page.getByTestId('warp-question')).toBeVisible()
    await page.getByTestId('warp-question').getByText('Last 7 days').click()
    await expect(page.getByTestId('warp-message-assistant').last()).toContainText('Seven failed.')

    const asked = bodies[1].messages[1]
    expect(asked.question).toBe(true)
    expect(asked.content).toContain('Let me narrow this down.')
    expect(asked.content).toContain('Which time range?')
    expect(asked.content).toContain('Last 7 days (-7d)')
  })

  test('settings offers a custom provider with no request restriction for embeddings', async ({ dashboardPage }) => {
    const page = dashboardPage.page
    await mockWarpConfig(page)
    await page.route('**/api/providers', (route) =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ providers: [{ name: 'my-openai', keys: [], custom_provider_config: { base_provider_type: 'openai' } }], total: 1 }),
      }),
    )
    await page.goto('/workspace/config/warp')
    await expect(page.getByTestId('warp-embedding-provider-select')).toBeVisible()
    await expect(page.getByTestId('warp-no-embedding-providers')).toHaveCount(0)
  })
})

// Closing the dock unmounts the panel, so anything the panel held dies with it unless the
// provider-owned session holds it. The chat and history answers are held open by the test, so
// the order they land in is the test's, not the network's.
test.describe('Warp state across a panel unmount', () => {
  test.beforeEach(async ({ dashboardPage }) => {
    await enableWarpFlag(dashboardPage.page)
    await dashboardPage.goto()
    await expect(dashboardPage.pageTitle).toBeVisible()
    await dashboardPage.page.context().addCookies([
      {
        name: 'bifrost_onboarding_remind_at',
        value: new Date(Date.now() + 24 * 60 * 60 * 1000).toISOString(),
        url: new URL(dashboardPage.page.url()).origin,
      },
    ])
  })

  test('a follow-up queued behind an answer survives the dock closing and sends when it reopens', async ({ dashboardPage }) => {
    const page = dashboardPage.page
    await mockWarpConfig(page)
    let release!: () => void
    const dockClosed = new Promise<void>((resolve) => {
      release = resolve
    })
    let answered!: () => void
    const firstAnswered = new Promise<void>((resolve) => {
      answered = resolve
    })
    const sent: string[] = []
    await page.route('**/api/warp/chat', async (route) => {
      const body = route.request().postData() ?? ''
      sent.push(body)
      if (sent.length === 1) {
        await dockClosed
        await route.fulfill({ status: 200, contentType: 'text/event-stream', body: sse('First answer.') })
        answered()
        return
      }
      await route.fulfill({ status: 200, contentType: 'text/event-stream', body: sse('Follow-up answer.') })
    })
    await page.reload()

    await page.getByTestId('topbar-warp-btn').click()
    const composer = page.getByTestId('warp-composer-input')
    await composer.fill('first question')
    await composer.press('Enter')
    await expect(page.getByTestId('warp-stop-btn')).toBeVisible()
    await composer.fill('and by model?')
    await composer.press('Enter')
    await expect(page.getByTestId('warp-queued-item')).toHaveCount(1)

    await page.getByTestId('warp-close-btn').click()
    await expect(page.getByTestId('warp-panel')).toHaveCount(0)
    release()
    await firstAnswered
    // The answer finished with no panel to drain the queue, so nothing is sent yet.
    expect(sent).toHaveLength(1)

    await page.getByTestId('topbar-warp-btn').click()
    await expect(page.getByTestId('warp-message-assistant')).toHaveCount(2)
    await expect(page.getByTestId('warp-message-assistant').last()).toContainText('Follow-up answer.')
    await expect(page.getByTestId('warp-queued-item')).toHaveCount(0)
    expect(sent).toHaveLength(2)
    expect(sent[1]).toContain('and by model?')
  })

  test('a history load that lands after its panel unmounted leaves the newer conversation alone', async ({ dashboardPage }) => {
    const page = dashboardPage.page
    await mockWarpConfig(page)
    const stored = {
      id: '00000000-0000-4000-8000-000000000009',
      title: 'Stored thread',
      message_count: 2,
      total_tokens: 0,
      total_cost: 0,
      created_at: '2026-10-01T00:00:00Z',
      updated_at: '2026-10-01T00:00:00Z',
    }
    await page.route('**/api/warp/conversations?**', (route) => route.fulfill({ json: { conversations: [stored] } }))
    await page.route('**/api/warp/conversations', (route) => route.fulfill({ json: { conversations: [stored] } }))
    let release!: () => void
    const newerAnswerShown = new Promise<void>((resolve) => {
      release = resolve
    })
    let landed!: () => void
    const historyLanded = new Promise<void>((resolve) => {
      landed = resolve
    })
    await page.route(`**/api/warp/conversations/${stored.id}`, async (route) => {
      await newerAnswerShown
      await route.fulfill({
        json: {
          ...stored,
          messages: [
            { role: 'user', content: 'stored question' },
            { role: 'assistant', content: 'Stored answer.' },
          ],
        },
      })
      landed()
    })
    await page.route('**/api/warp/chat', (route) =>
      route.fulfill({ status: 200, contentType: 'text/event-stream', body: sse('Newer answer.') }),
    )
    await page.reload()

    await page.getByTestId('topbar-warp-btn').click()
    await page.getByTestId('warp-history-btn').click()
    await page.getByTestId(`warp-history-item-${stored.id}`).click()
    await page.getByTestId('warp-close-btn').click()
    await expect(page.getByTestId('warp-panel')).toHaveCount(0)

    await page.getByTestId('topbar-warp-btn').click()
    const composer = page.getByTestId('warp-composer-input')
    await composer.fill('newer question')
    await composer.press('Enter')
    await expect(page.getByTestId('warp-message-assistant')).toContainText('Newer answer.')

    release()
    await historyLanded
    // Give a stale load's state updates the chance to render before asserting they did not.
    await page.waitForTimeout(250)
    await expect(page.getByTestId('warp-message-user')).toHaveText('newer question')
    await expect(page.getByTestId('warp-message-assistant')).toHaveText(/Newer answer\./)
  })
})

// Warp ran on exactly one model, and the composer's model chip was a link to
// the settings page for whoever was looking at it. An administrator can now
// expose several models: the chip switches between them, the request names the
// one picked, and the way into settings is a separate control.
test.describe('Warp model switcher', () => {
  test.beforeEach(async ({ dashboardPage }) => {
    await enableWarpFlag(dashboardPage.page)
    await dashboardPage.goto()
    await expect(dashboardPage.pageTitle).toBeVisible()
    // The first-run checklist is a fixed card in the bottom-right corner, over
    // the dock's composer, so a click on the model chip would time out against
    // it. Snoozed with its own cookie, the same way the config suite does; each
    // test reloads after setting up its routes, which is when it takes effect.
    await dashboardPage.page.context().addCookies([
      {
        name: 'bifrost_onboarding_remind_at',
        value: new Date(Date.now() + 24 * 60 * 60 * 1000).toISOString(),
        url: new URL(dashboardPage.page.url()).origin,
      },
    ])
  })

  test('asks on the model picked from the exposed list, and remembers the pick', async ({ dashboardPage }) => {
    const page = dashboardPage.page
    await mockWarpConfig(page, [{ provider: 'anthropic', model: 'claude-sonnet-5' }])
    const sent: { provider?: string; model?: string }[] = []
    await page.route('**/api/warp/chat', async (route) => {
      sent.push(route.request().postDataJSON() as { provider?: string; model?: string })
      await route.fulfill({ status: 200, contentType: 'text/event-stream', body: sse('Answered.') })
    })
    await page.reload()

    await page.getByTestId('topbar-warp-btn').click()
    const chip = page.getByTestId('warp-composer-model')
    await expect(chip).toContainText('gpt-5.6-luna')

    // With nothing picked the request names no model, so the server's default answers.
    const composer = page.getByTestId('warp-composer-input')
    await composer.fill('first question')
    await composer.press('Enter')
    await expect(page.getByTestId('warp-message-assistant')).toHaveCount(1)
    expect(sent).toHaveLength(1)
    expect(sent[0].provider).toBeUndefined()
    expect(sent[0].model).toBeUndefined()

    await chip.click()
    await expect(page.getByTestId('warp-model-option-openai-gpt-5.6-luna')).toContainText('Default')
    await page.getByTestId('warp-model-option-anthropic-claude-sonnet-5').click()
    await expect(chip).toContainText('claude-sonnet-5')

    await composer.fill('second question')
    await composer.press('Enter')
    await expect(page.getByTestId('warp-message-assistant')).toHaveCount(2)
    expect(sent).toHaveLength(2)
    expect(sent[1].provider).toBe('anthropic')
    expect(sent[1].model).toBe('claude-sonnet-5')

    // The pick is this browser's preference, so it outlives a reload.
    await page.reload()
    await page.getByTestId('topbar-warp-btn').click()
    await expect(page.getByTestId('warp-composer-model')).toContainText('claude-sonnet-5')
  })

  test('a single configured model is a label with settings beside it, not a link', async ({ dashboardPage }) => {
    const page = dashboardPage.page
    await mockWarpConfig(page)
    await page.reload()

    await page.getByTestId('topbar-warp-btn').click()
    const chip = page.getByTestId('warp-composer-model')
    await expect(chip).toContainText('gpt-5.6-luna')
    // Nothing to switch to, so the chip opens no menu and goes nowhere.
    await chip.click()
    await expect(page.getByTestId('warp-model-menu')).toHaveCount(0)
    await expect(page).not.toHaveURL(/\/workspace\/config\/warp/)

    // Settings is its own control. This suite runs as the local administrator;
    // the control is withheld from callers without Warp Update.
    await page.getByTestId('warp-composer-settings').click()
    await expect(page).toHaveURL(/\/workspace\/config\/warp/)
  })

  test('falls back to the default when the remembered model is no longer exposed', async ({ dashboardPage }) => {
    const page = dashboardPage.page
    await mockWarpConfig(page, [{ provider: 'anthropic', model: 'claude-sonnet-5' }])
    await page.reload()
    await page.getByTestId('topbar-warp-btn').click()
    await page.getByTestId('warp-composer-model').click()
    await page.getByTestId('warp-model-option-anthropic-claude-sonnet-5').click()
    await expect(page.getByTestId('warp-composer-model')).toContainText('claude-sonnet-5')

    // The administrator removes that model. A request still naming it would be
    // refused, so the panel must stop offering it and stop sending it.
    await page.unroute('**/api/warp/config')
    await mockWarpConfig(page)
    const sent: { provider?: string; model?: string }[] = []
    await page.route('**/api/warp/chat', async (route) => {
      sent.push(route.request().postDataJSON() as { provider?: string; model?: string })
      await route.fulfill({ status: 200, contentType: 'text/event-stream', body: sse('Answered.') })
    })
    await page.reload()

    await page.getByTestId('topbar-warp-btn').click()
    await expect(page.getByTestId('warp-composer-model')).toContainText('gpt-5.6-luna')
    const composer = page.getByTestId('warp-composer-input')
    await composer.fill('still works?')
    await composer.press('Enter')
    await expect(page.getByTestId('warp-message-assistant')).toHaveCount(1)
    expect(sent[0].provider).toBeUndefined()
    expect(sent[0].model).toBeUndefined()
  })
})
