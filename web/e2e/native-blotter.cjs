// Run by the disposable BFF probe. Session ids stay in this process.
const { chromium } = require(process.env.TEST_MFA_PLAYWRIGHT)
const assert = require('node:assert/strict')

function decimal(value) {
  if (!value) return '—'
  const digits = BigInt(value.coefficient).toString()
  const sign = digits.startsWith('-') ? '-' : ''
  const absolute = digits.replace(/^-/, '')
  if (value.exponent >= 0) return sign + absolute + '0'.repeat(value.exponent)
  const scaled = absolute.padStart(1 - value.exponent, '0')
  return sign + scaled.slice(0, value.exponent) + '.' + scaled.slice(value.exponent)
}

async function main() {
  let raw = ''
  for await (const part of process.stdin) {
    raw += part
    assert(raw.length <= 2 * 1024 * 1024)
  }
  const { base, authorized, foreign, state } = JSON.parse(raw)
  const route = '/portfolios/' + encodeURIComponent(state.portfolio_id) + '/orders'
  const browser = await chromium.launch({ headless: true, timeout: 10_000 })
  try {
    for (const [cookie, expectedStatus] of [[authorized, 200], [foreign, 404]]) {
      const ctx = await browser.newContext()
      try {
        await ctx.addCookies([{ name: 'kanz_session', value: cookie, url: base, httpOnly: true, sameSite: 'Lax' }])
        const page = await ctx.newPage()
        await page.clock.install()
        const read = () => page.waitForResponse(resp => resp.url() === base + '/api/v1' + route + '?limit=200')
        const response = read()
        await page.goto(base + route)
        assert.equal((await response).status(), expectedStatus)
        if (expectedStatus === 404) {
          await page.getByRole('alert').waitFor()
          assert.equal(await page.getByRole('table').count(), 0)
          assert(!(await page.locator('body').innerText()).includes(state.order_id))
          continue
        }
        const row = page.getByRole('row').filter({ has: page.getByText(state.order_id, { exact: true }) })
        await row.waitFor()
        const cells = await row.getByRole('cell').allTextContents()
        assert.equal(cells[1].trim(), state.instrument_id)
        assert.equal(cells[2].trim(), state.side.replace('SIDE_', '').toLowerCase())
        const status = state.status.replace('ORDER_STATUS_', '').replaceAll('_', ' ').toLowerCase()
        assert(cells[3].includes(status))
        assert.equal(cells[3].includes('Remote outcome unknown'), Boolean(state.quarantine))
        for (const [index, field] of ['ordered_quantity', 'filled_quantity', 'limit_price', 'average_fill_price', 'leaves_quantity'].entries()) {
          assert.equal(cells[index + 4].trim(), decimal(state[field]))
        }
        if (state.arrival_at && state.arrival_price) {
          assert(cells[9].includes(decimal(state.arrival_price)))
          assert(cells[9].includes(state.arrival_at))
        } else {
          assert.equal(cells[9].trim(), 'Unavailable')
        }
        await page.clock.fastForward(61_000)
        await page.getByRole('status').filter({ hasText: 'snapshot is stale' }).waitFor()
        const refreshed = read()
        await page.getByRole('button', { name: 'Refresh order history' }).click()
        assert.equal((await refreshed).status(), 200)
        await row.waitFor()
        assert.equal(await page.getByText('This snapshot is stale.', { exact: false }).count(), 0)
      } finally {
        await ctx.close()
      }
    }
  } finally {
    await browser.close()
  }
}
main().catch(() => { process.exitCode = 1 })
