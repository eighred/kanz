// All principals and dates come from the isolated signed pilot fixture.
const assert = require('node:assert/strict')
const { comparisonDownload } = require('./custody-comparison-receipt.cjs')
const { chromium } = require(process.env.TEST_MFA_PLAYWRIGHT)

let stage = 'input'
async function main() {
  let raw = ''
  for await (const chunk of process.stdin) {
    raw += chunk
    assert(raw.length < 64 * 1024)
  }
  const { base, cookies, expected } = JSON.parse(raw)
  const origin = new URL(base)
  assert.equal(origin.origin, base)
  assert.equal(origin.protocol, 'http:')
  assert.equal(origin.hostname, '127.0.0.1')
  assert(origin.port)
  const { portfolio, custodian, clean_date, clean_run, statement, missing_date, missing_run, unobserved_date } = expected
  assert.equal(portfolio, 'PF1')
  assert.equal(custodian, 'CUST-A')
  assert.match(statement, /^custody-batch\.v1\.synthetic-source\.r2\.[0-9a-f]{64}$/)
  const endpoint = '/api/v1/portfolios/PF1/custody/comparison'
  const lookup = date => endpoint + '?' + new URLSearchParams({ custodian_id: custodian, business_date: date })
  stage = 'launch'
  const browser = await chromium.launch({ headless: true, timeout: 10_000 })
  try {
    for (const [name, status] of Object.entries({ authorized: 200, foreign: 404, excluded: 404, unfunded: 403 })) {
      const context = await browser.newContext()
      try {
        await context.route('**/*', route => new URL(route.request().url()).origin === base ? route.continue() : route.abort())
        await context.addCookies([{ name: 'kanz_session', value: cookies[name], url: base, httpOnly: true, sameSite: 'Lax' }])
        const page = await context.newPage()
        page.setDefaultTimeout(6000)
        stage = name + '-open-response'
        await page.goto(base + '/custody-breaks')
        if (name !== 'authorized') {
          for (const date of [clean_date, unobserved_date]) {
            const reply = await page.evaluate(async path => {
              const response = await fetch(path)
              return { status: response.status, body: await response.text() }
            }, lookup(date))
            assert.equal(reply.status, status)
            for (const secret of [clean_run, statement, missing_run]) assert(!reply.body.includes(secret))
          }
          assert.equal(await page.getByRole('button', { name: 'Download comparison receipt' }).count(), 0)
          continue
        }
        for (const [date, state, outcome, label, run] of [
          [clean_date, 'recorded', 'clean', 'Clean comparison', clean_run],
          [missing_date, 'recorded', 'no_statement', 'No statement received', missing_run],
          [unobserved_date, 'not_compared', undefined, 'No comparison recorded', undefined],
        ]) {
          const region = page.getByRole('region', { name: 'Dated custody result' })
          await region.getByLabel('Portfolio ID', { exact: true }).fill(portfolio)
          await region.getByLabel('Custodian ID', { exact: true }).fill(custodian)
          await region.getByLabel('Business date', { exact: true }).fill(date)
          const pending = page.waitForResponse(response => {
            const url = new URL(response.url())
            return url.pathname === endpoint && url.searchParams.get('business_date') === date && response.request().method() === 'GET'
          })
          await region.getByRole('button', { name: 'View dated result' }).click()
          const response = await pending
          assert.equal(response.status(), 200)
          stage = name + '-open-payload'
          const receipt = await response.json()
          assert.equal(receipt.tenant_id, 'synthetic-custody-pilot')
          assert.equal(receipt.state, state)
          assert.equal(receipt.outcome, outcome)
          assert.equal(receipt.run_id, run)
          assert.equal(receipt.business_date, date)
          assert.equal(receipt.portfolio_id, portfolio)
          assert.equal(receipt.custodian_id, custodian)
          assert.equal(receipt.statement_id, outcome === 'clean' ? statement : undefined)
          stage = name + '-open-row'
          const result = region.locator('output[data-state="' + state + '"]')
          await region.getByText(label, { exact: false }).first().waitFor()
          await result.waitFor()
          const text = await region.innerText()
          if (run) assert(text.includes(run))
          if (outcome === 'clean') {
            assert.equal(receipt.journal_position, '1')
            assert.equal(receipt.transaction_pass, 'ran')
            assert.equal(receipt.book_reference_count, '1')
            assert.equal(receipt.statement_reference_count, '1')
            assert.equal(receipt.unreferenced_book_trades, '0')
            assert(text.includes('Independent reference comparison'))
            assert(text.includes(statement))
          } else {
            assert.equal(receipt.transaction_pass, undefined)
            assert(!text.includes(statement))
          }
          if (run) await comparisonDownload(page, region, receipt)
          else assert.equal(await region.getByRole('button', { name: 'Download comparison receipt' }).count(), 0)
        }
        // Even on a gateway with registered financial/action routes, the very
        // same customer session must be refused before entering those handlers.
        stage = name + '-reload-response'
        const posts = await page.evaluate(async () => {
          const paths = ['/api/v1/portfolios/PF1/cash-movements', '/api/v1/portfolios/PF1/custody/reruns',
            '/api/v1/custody/breaks/synthetic/actions']
          return Promise.all(paths.map(async path => (await fetch(path, {
            method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}',
          })).status))
        })
        assert.deepEqual(posts, [403, 403, 403])
      } finally { await context.close() }
    }
  } finally { await browser.close() }
}

main().catch(() => { process.stderr.write('custody-browser-stage=' + stage + '\n'); process.exitCode = 1 })
