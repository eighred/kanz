const { chromium } = require('playwright')
const http = require('node:http')
const fs = require('node:fs/promises')
const path = require('node:path')
const assert = require('node:assert/strict')
// Presentation qualification only: actual built Vue/Chromium, synthetic read
// responses. This does not claim BFF authentication or production history proof.
async function main() {
  const root = path.resolve(__dirname, '../dist')
  const files = new Map()
  for (const file of await fs.readdir(root, { recursive: true, withFileTypes: true })) {
    if (file.isFile()) {
      const full = path.join(file.parentPath, file.name)
      files.set('/' + path.relative(root, full).split(path.sep).join('/'), await fs.readFile(full))
    }
  }
  assert(files.has('/index.html'), 'build required')
  const server = http.createServer((req, res) => {
    const pathname = new URL(req.url, 'http://127.0.0.1').pathname
    const key = files.has(pathname) ? pathname : '/index.html'
    res.setHeader('Content-Type', key.endsWith('.js') ? 'text/javascript' : key.endsWith('.css') ? 'text/css' : 'text/html')
    res.end(files.get(key))
  })
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
  let browser, browserServer, deadline
  try {
    const base = `http://127.0.0.1:${server.address().port}`
    browserServer = await chromium.launchServer({ headless: true, host: '127.0.0.1', timeout: 10000 })
    browser = await chromium.connect(browserServer.wsEndpoint(), { timeout: 10000 })
    await Promise.race([new Promise((_, reject) => { deadline = setTimeout(() => reject(new Error('browser proof timed out')), 60000) }), (async () => {
    const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })
    page.setDefaultTimeout(10000)
    const errors = []
    page.on('pageerror', error => errors.push(error.message))
    let nodeWrites = 0
    let releaseB
    const heldB = new Promise(resolve => { releaseB = resolve })
    const qty = '900719925474099312345.0000000000001'
    await page.route('**/*', async route => {
      const url = new URL(route.request().url())
      if (url.origin !== base) return route.abort()
      const reply = body => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) })
      if (url.pathname === '/auth/me') return reply({ subject: 'fixture-reader', tenant: 'fixture-tenant', expires_at: '2099-01-01T00:00:00Z' })
      if (url.pathname === '/api/v1/control/nodes' && route.request().method() === 'GET') {
        return reply({ nodes: [{ name: 'fixture-node', schedulable: true }] })
      }
      if (url.pathname.startsWith('/api/v1/control/') && route.request().method() !== 'GET') nodeWrites++
      const match = url.pathname.match(/^\/api\/v1\/broker\/accounts\/([^/]+)\/(state|positions|orders|executions)$/)
      if (!match) {
        if (url.pathname.startsWith('/api/') || url.pathname.startsWith('/auth/')) return route.fulfill({ status: 404, body: '{}' })
        return route.continue()
      }
      const [, id, kind] = match
      if (id === 'B') { await heldB; if (route.request().failure()) return }
      const count = id === 'A' ? 10000 : id === 'D' ? 10001 : 1
      let body
      if (kind === 'state') body = { id, currency: id, balance: qty, equity: '0', realizedPl: '0', unrealizedPl: '0', openPositions: 0 }
      if (kind === 'positions') body = { positions: [] }
      if (kind === 'orders') body = { retainedFrom: '2026-01-01T00:00:00Z', orders: Array.from({ length: count }, (_, i) => ({ id: `${id}-order-${i}`, instrument: 'BTC-USD', venue: 'MOCK', parentOrderId: 'parent', side: 'buy', type: 'limit', status: 'open', qty, filledQty: '0', leavesQty: qty, updatedAt: '2026-01-02T00:00:00Z' })) }
      if (kind === 'executions') body = { retainedFrom: '2026-01-01T00:00:00Z', executions: Array.from({ length: count }, (_, i) => ({ id: `${id}-fill-${i}`, orderId: `${id}-order-${i}`, instrument: 'BTC-USD', venue: 'MOCK', side: 'buy', qty, price: qty, ...(i % 2 ? { fee: '0' } : {}), time: '2026-01-02T00:00:00Z' })) }
      try { await reply(body) } catch (error) { if (id !== 'B') throw error } // deliberately aborted old request
    })
    await page.clock.install({ time: new Date('2026-11-18T12:00:00Z') })
    let portfolioReads = 0
    await page.route('**/api/v1/portfolios', route => {
      portfolioReads++
      return route.fulfill({ json: { owner_tenant: 'fixture-tenant', portfolios: [
        { portfolio_id: 'prior', position_count: 0, as_of: '2026-11-17T12:01:00Z' },
        { portfolio_id: 'future', position_count: 0, as_of: '2026-11-19T12:01:00Z' },
      ] } })
    })
    await page.goto(base + '/portfolios')
    const priorFold = page.getByRole('row').filter({ hasText: 'prior' })
    await priorFold.getByText('23h ago', { exact: true }).waitFor()
    await page.getByRole('row').filter({ hasText: 'future' }).getByText('Unknown', { exact: true }).waitFor()
    await page.clock.fastForward(120_000)
    await priorFold.getByText('1d ago (stale)', { exact: true }).waitFor()
    assert.equal(portfolioReads, 1, 'age should advance without another portfolio read')
    const cashRequests = []
    await page.route('**/api/v1/portfolios/PF1/cash-movements/*', route => {
      const id = new URL(route.request().url()).pathname.split('/').pop()
      cashRequests.push([route.request().method(), id])
      if (id === 'restricted') return route.fulfill({ status: 403, json: {} })
      return route.fulfill({ json: { movement_id: id === 'unconfirmed' ? 'other' : id, actor: 'fixture-funder',
        status: 'journal_recorded', accepted_at: '2026-11-18T11:30:00.123456Z', command_digest: 'a'.repeat(64) } })
    })
    await page.getByText('Portfolio and risk', { exact: true }).click()
    await page.getByRole('link', { name: 'Cash movement receipts' }).click()
    await page.getByLabel('Portfolio ID').fill('PF1')
    const movement = page.getByLabel('Movement ID')
    await movement.fill('movement-1')
    await movement.press('Enter')
    const receipt = page.getByRole('region', { name: 'Receipt for movement-1' })
    await receipt.getByText('Recorded in journal').waitFor()
    await receipt.getByText('Unknown', { exact: true }).waitFor()
    await receipt.getByText('fixture-funder').waitFor()
    await movement.fill('restricted')
    await receipt.waitFor({ state: 'hidden' })
    await movement.press('Enter')
    await page.getByRole('alert').filter({ hasText: 'permission' }).waitFor()
    await movement.fill('unconfirmed')
    await movement.press('Enter')
    await page.getByRole('alert').filter({ hasText: 'could not confirm' }).waitFor()
    assert.equal(await page.getByText('fixture-funder').count(), 0)
    assert.deepEqual(cashRequests, [['GET', 'movement-1'], ['GET', 'restricted'], ['GET', 'unconfirmed']])
    const proposalWire = require('../../kanz/services/optimization/internal/server/testdata/model-proposal-review.json')
    const proposalRequests = []
    await page.route('**/api/v2/model-portfolios/**', route => {
      const request = route.request(), body = request.postDataJSON()
      proposalRequests.push([request.method(), new URL(request.url()).pathname])
      assert.equal(body.nav, '9007199254740993')
      assert.equal(body.prices.A, '0.000000000001')
      return body.portfolio_id === 'PF' ? route.fulfill({ json: proposalWire }) : route.fulfill({ status: 403, json: {} })
    })
    await page.getByText('Portfolio and risk', { exact: true }).click()
    await page.getByRole('link', { name: 'Model proposal review' }).click()
    await page.getByLabel('Portfolio ID').fill('PF')
    await page.getByLabel('Valuation currency', { exact: true }).fill('USD')
    await page.getByLabel('NAV in valuation currency').fill('9007199254740993')
    await page.getByLabel('Weight-change threshold').fill('0')
    await page.getByLabel('Instrument ID').fill('A')
    await page.getByLabel('Current weight').fill('0')
    await page.getByLabel('Price', { exact: true }).fill('0.000000000001')
    await page.getByLabel('Expected return per period').fill('0.1')
    await page.getByRole('button', { name: 'Evaluate candidate' }).press('Enter')
    const candidate = page.getByRole('region', { name: 'Candidate for PF' })
    await candidate.getByText('9007199254740993000000000000', { exact: true }).waitFor()
    await candidate.getByText('9007199254740993', { exact: true }).waitFor()
    await candidate.getByText('Unchecked.', { exact: false }).waitFor()
    await candidate.getByText('Risk estimate unavailable.', { exact: false }).waitFor()
    await page.getByLabel('Portfolio ID').fill('outside')
    await candidate.waitFor({ state: 'hidden' })
    await page.getByRole('button', { name: 'Evaluate candidate' }).press('Enter')
    await page.getByRole('alert').filter({ hasText: 'permission' }).waitFor()
    assert.deepEqual(proposalRequests, Array(2).fill(['POST', '/api/v2/model-portfolios/propose']))
    await page.goto(base + '/broker-accounts/A')
    await page.getByRole('link', { name: 'Skip to main content' }).waitFor({ state: 'attached' })
    await page.keyboard.press('Tab')
    assert.equal(await page.locator(':focus').textContent(), 'Skip to main content')
    await page.keyboard.press('Enter')
    assert.equal(await page.locator(':focus').getAttribute('id'), 'main-content')
    await page.keyboard.press('Tab')
    assert.equal(await page.locator(':focus').getAttribute('href'), '/broker-accounts')
    await page.keyboard.press('Enter')
    await page.waitForURL(base + '/broker-accounts')
    await page.locator('main:focus').waitFor()
    await page.goBack()
    await page.waitForURL(base + '/broker-accounts/A')
    const orders = page.getByRole('region', { name: 'Orders in served window', exact: true })
    const executions = page.getByRole('region', { name: 'Executions in served window', exact: true })
    await orders.locator('.data-row').first().waitFor()
    for (const region of [orders, executions]) {
      assert((await region.locator('.data-row').count()) <= 14, 'DOM must be windowed')
      assert.equal(await region.locator('table').getAttribute('aria-rowcount'), '10001')
      assert((await region.textContent()).includes(qty), 'exact decimal string lost')
      for (const height of await region.locator('.data-row').evaluateAll(rows => rows.map(row => row.getBoundingClientRect().height))) assert(Math.abs(height - 64) < 1, 'fixed row contract differs')
    }
    assert.equal(await executions.locator('.data-row').nth(0).locator('td').last().textContent(), '—')
    assert.equal(await executions.locator('.data-row').nth(1).locator('td').last().textContent(), '0')
    assert((await page.textContent('body')).includes('freshness cannot be established'))
    assert((await page.textContent('body')).includes('Earlier executions are not shown'))
    for (const region of [orders, executions]) {
      await region.evaluate(element => { element.scrollTop = 5000 * 64 })
      await region.locator('[aria-rowindex="5002"]').waitFor()
      assert((await region.locator('.data-row').count()) <= 14)
      const headerOffset = await region.evaluate(element => element.querySelector('thead').getBoundingClientRect().top - element.getBoundingClientRect().top)
      assert(Math.abs(headerOffset) <= 2, 'header must remain sticky inside viewport')
      await region.evaluate(element => { element.scrollTop = element.scrollHeight })
      await region.locator('[aria-rowindex="10001"]').waitFor()
      assert((await region.locator('.data-row').count()) <= 14)
    }
    await page.setViewportSize({ width: 720, height: 600 })
    await orders.locator('[aria-rowindex="10001"]').waitFor()
    for (const region of [orders, executions]) for (const height of await region.locator('.data-row').evaluateAll(rows => rows.map(row => row.getBoundingClientRect().height))) assert(Math.abs(height - 64) < 1, 'narrow viewport changed row height')
    await orders.focus()
    // The region is keyboard focusable; keyboard-scroll semantics are not claimed.
    assert.equal(await orders.getAttribute('tabindex'), '0')
    const navigate = id => page.evaluate(account => {
      history.pushState({ ...history.state }, '', '/broker-accounts/' + account)
      dispatchEvent(new PopStateEvent('popstate', { state: history.state }))
    }, id)
    await navigate('B')
    await page.getByText('Loading…', { exact: true }).waitFor()
    assert.equal(await page.locator('.data-row').count(), 0, 'previous account remains visible')
    await navigate('C')
    await page.getByRole('region', { name: 'Orders in served window', exact: true }).getByText('C-order-0', { exact: true }).waitFor()
    releaseB()
    await page.waitForTimeout(100)
    assert.equal(await page.locator('dd').first().textContent(), 'C')
    assert.equal(await page.getByText('B-order-0', { exact: true }).count(), 0)
    await navigate('D')
    const pages = page.getByRole('navigation', { name: 'Orders in served window pages', exact: true })
    await pages.getByRole('button', { name: 'Next page' }).click()
    const last = page.getByRole('region', { name: 'Orders in served window', exact: true })
    await last.locator('[aria-rowindex="10002"]').waitFor()
    assert.equal(await last.locator('.data-row').count(), 1)
    assert((await last.textContent()).includes('D-order-10000'))
    await pages.getByRole('button', { name: 'Previous page' }).click()
    await last.locator('[aria-rowindex="2"]').waitFor()
    await pages.getByRole('button', { name: 'Next page' }).click()
    await navigate('C')
    await page.getByRole('region', { name: 'Orders in served window', exact: true }).getByText('C-order-0', { exact: true }).waitFor()
    assert.equal(await page.getByRole('navigation', { name: 'Orders in served window pages', exact: true }).count(), 0)
    await page.goto(base + '/nodes')
    const drain = page.getByRole('button', { name: 'Drain', exact: true })
    await drain.focus()
    await page.keyboard.press('Enter')
    const dialog = page.getByRole('dialog', { name: 'Drain node' })
    await dialog.waitFor()
    const cancel = dialog.getByRole('button', { name: 'Cancel' })
    assert(await cancel.evaluate(element => element === document.activeElement))
    await page.keyboard.press('Tab')
    const nameInput = dialog.getByRole('textbox')
    assert(await nameInput.evaluate(element => element === document.activeElement))
    await page.keyboard.press('Shift+Tab')
    assert(await cancel.evaluate(element => element === document.activeElement))
    await page.keyboard.press('Escape')
    await dialog.waitFor({ state: 'hidden' })
    assert(await drain.evaluate(element => element === document.activeElement))
    assert.equal(nodeWrites, 0)
    assert.deepEqual(errors, [])
    console.log('Broker history Chromium PASS: 10000 rows, bounded DOM, exact strings, missing values, retained warnings, scrolling, resize, stale route refusal')
    })()])
  } finally {
    clearTimeout(deadline)
    let cleanupTimer
    try {
      if (browserServer) await Promise.race([browserServer.close(), new Promise((_, reject) => { cleanupTimer = setTimeout(() => { browserServer.process().kill('SIGKILL'); reject(new Error('browser cleanup timed out')) }, 10000) })])
    } finally {
      clearTimeout(cleanupTimer)
      server.closeAllConnections()
      await new Promise((resolve, reject) => server.close(error => error ? reject(error) : resolve()))
    }
  }
}
main().catch(error => { console.error('Broker history browser proof failed:', error.message); process.exitCode = 1 })
