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
    let releaseB
    const heldB = new Promise(resolve => { releaseB = resolve })
    const qty = '900719925474099312345.0000000000001'
    await page.route('**/*', async route => {
      const url = new URL(route.request().url())
      if (url.origin !== base) return route.abort()
      const reply = body => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) })
      if (url.pathname === '/auth/me') return reply({ subject: 'fixture-reader', tenant: 'fixture-tenant', expires_at: '2099-01-01T00:00:00Z' })
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
    await page.goto(base + '/broker-accounts/A')
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
