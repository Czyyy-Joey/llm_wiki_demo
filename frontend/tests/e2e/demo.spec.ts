import { expect, test, type Page } from '@playwright/test'

const sourceA = `# Vector Databases

Vector databases store embeddings for semantic similarity search.

They support nearest-neighbor retrieval for related concepts.
`

const sourceB = `# Vector Database Systems

Vector database systems store embedding vectors and support semantic similarity retrieval.

They add indexing strategies for efficient nearest-neighbor search.
`

async function upload(page: Page, name: string, body: string) {
  await page.locator('input[type=file]').setInputFiles({
    name,
    mimeType: 'text/markdown',
    buffer: Buffer.from(body),
  })
  await expect(page.getByText(`${name}`, { exact: true }).first()).toBeVisible()
  await expect(page.getByText('stable chunks parsed.')).toBeVisible()
}

async function compileSelected(page: Page, expectedAction: 'CREATE' | 'UPDATE') {
  await page.getByRole('button', { name: 'Compile Source' }).click()
  const openRun = page.getByRole('link', { name: 'Open run' })
  await expect(openRun).toBeVisible({ timeout: 30_000 })
  await openRun.click()
  await expect(page).toHaveURL(/\/compilation\?run=/)
  await expect(page.locator('.pipeline .done')).toHaveCount(6)
  await expect(page.locator('.plan-action strong').filter({ hasText: expectedAction })).toBeVisible()
  await expect(page.getByText('Plan passed schema and domain validation.')).toBeVisible()
}

test('complete Phase 6 demo remains observable and persistent', async ({ page, request }) => {
  test.setTimeout(120_000)
  await page.goto('/sources')
  await upload(page, 'vector-databases.md', sourceA)
  await compileSelected(page, 'CREATE')

  await page.getByRole('link', { name: /Open Vector Databases in Wiki/ }).click()
  await expect(page.getByRole('heading', { name: 'Vector Databases', level: 1 })).toBeVisible()
  await expect(page.getByText('1 sources')).toBeVisible()
  await page.locator('.claim').filter({ hasText: 'Vector databases store embeddings' }).locator('.citations button').click()
  const desktopInspector = page.locator('aside[aria-label="Citation inspector"]')
  await expect(desktopInspector).toContainText('vector-databases.md')
  await expect(desktopInspector).toContainText('Vector databases store embeddings')

  await page.getByRole('link', { name: 'Sources', exact: true }).click()
  await upload(page, 'vector-database-systems.md', sourceB)
  await compileSelected(page, 'UPDATE')

  await page.getByRole('link', { name: 'Wiki', exact: true }).click()
  const wikiPageLink = page.getByLabel('Wiki pages').getByRole('link').first()
  await expect(wikiPageLink).toHaveAttribute('href', '/wiki/vector-databases')
  const canonicalTitle = await wikiPageLink.locator('span').innerText()
  await wikiPageLink.click()
  await expect(page.getByRole('heading', { name: canonicalTitle, level: 1 })).toBeVisible()
  await expect(page.getByText('2 sources')).toBeVisible()
  await expect(page.getByLabel('Wiki pages').getByRole('link')).toHaveCount(1)

  const reindex = await request.post('/api/indexes/reindex')
  expect(reindex.ok()).toBeTruthy()

  await page.getByRole('link', { name: 'Search', exact: true }).click()
  await page.getByLabel('Search compiled Wiki').fill('semantic similarity embeddings')
  await page.getByRole('button', { name: 'Search' }).click()
  await expect(page.locator('.result-row h3').filter({ hasText: canonicalTitle })).toBeVisible()
  await expect(page.getByText('Retrieval trace')).toBeVisible()
  await expect(page.getByText('fts and vector search executed')).toBeVisible()
  await expect(page.locator('.score-line').first()).toContainText('FTS')
  await expect(page.locator('.score-line').first()).toContainText('Vector')
  await expect(page.locator('.score-line').first()).toContainText('RRF')
  await expect(page.locator('.score-line').first()).toContainText('Final')
  await expect(page.locator('.context-kind').filter({ hasText: 'source_evidence' }).first()).toBeVisible()

  await page.getByRole('link', { name: 'Query', exact: true }).click()
  await page.getByLabel('Query question').fill('What do vector databases store?')
  await page.getByRole('button', { name: 'Ask' }).click()
  const groundedClaims = [
    'Vector databases store embeddings for semantic similarity search.',
    'They support nearest-neighbor retrieval for related concepts.',
    'Vector database systems store embedding vectors and support semantic similarity retrieval.',
    'They add indexing strategies for efficient nearest-neighbor search.',
  ]
  const queryAnswer = page.locator('.answer-text')
  await expect(queryAnswer).toBeVisible()
  const queryAnswerText = (await queryAnswer.innerText()).trim()
  expect(groundedClaims).toContain(queryAnswerText)
  const queryCitation = page.locator('.answer-citations a').first()
  await expect(queryCitation).toBeVisible()
  await queryCitation.click()
  await expect(page).toHaveURL(/\/sources\?chunk=/)
  await expect(page.locator('.source-inspector blockquote')).toContainText(queryAnswerText)
  await expect(page.locator('.source-location')).toContainText('Vector Database')

  await page.getByRole('link', { name: 'Chat', exact: true }).click()
  await page.getByLabel('Chat question').fill('What do vector databases store?')
  const streamResponse = page.waitForResponse(response => response.url().includes('/messages/stream'))
  await page.getByRole('button', { name: 'Send question' }).click()
  expect((await streamResponse).headers()['content-type']).toContain('text/event-stream')
  const chatAnswer = page.locator('.chat-message.assistant:not(.streaming) p').last()
  await expect(chatAnswer).toBeVisible({ timeout: 30_000 })
  const chatAnswerText = (await chatAnswer.innerText()).trim()
  expect(groundedClaims).toContain(chatAnswerText)
  await expect(page.locator('.chat-message.assistant .chat-citation')).toBeVisible()
  await expect(page.locator('.chat-message.assistant .message-observability')).toContainText('Trace')

  await page.reload()
  await expect(page.locator('.chat-message.user')).toContainText('What do vector databases store?')
  await expect(page.locator('.chat-message.assistant')).toContainText(chatAnswerText)

  await page.setViewportSize({ width: 390, height: 844 })
  await page.goto('/wiki')
  await expect(page.getByRole('navigation', { name: 'Main navigation' })).toBeVisible()
  await expect(page.getByRole('link', { name: 'Wiki', exact: true })).toBeVisible()
  await expect(page.getByRole('heading', { name: canonicalTitle, level: 1 })).toBeVisible()
  const mobileClaim = page.locator('.claim').filter({ hasText: 'Vector databases store embeddings' }).locator('.citations button').first()
  await mobileClaim.click()
  const mobileInspector = page.locator('aside[aria-label="Citation inspector"]')
  await expect(mobileInspector).toBeVisible()
  await expect(mobileInspector).toContainText('vector-databases.md')
  await expect(mobileInspector).toContainText('Vector databases store embeddings')
  await mobileInspector.getByRole('button', { name: 'Close citation inspector' }).click()
  await expect(mobileInspector).toHaveCount(0)
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)
  expect(overflow).toBeLessThanOrEqual(1)
})
