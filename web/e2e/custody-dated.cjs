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
  const prior = expected.prior_detection
  assert.equal(prior.portfolio_id, portfolio)
  assert.equal(prior.custodian_id, custodian)
  assert.match(prior.detection.statement_id, /^custody-batch\.v1\.synthetic-source\.r1\.[0-9a-f]{64}$/)
  assert.equal(prior.detection.business_date, clean_date)
  assert.deepEqual([prior.ibor, prior.custodian, prior.difference], ['10.5', '9.5', '1'])
  const oldRun = prior.detection.run_id
  assert(oldRun && oldRun !== clean_run)
  const endpoint = '/api/v1/portfolios/PF1/custody/comparison'
  const lookup = (date, saved) => {
    const query = new URLSearchParams({ custodian_id: custodian, business_date: date })
    if (saved) query.set('run_id', saved)
    return endpoint + '?' + query
  }
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
          for (const [date, saved] of [[clean_date], [unobserved_date], [clean_date, oldRun], [clean_date, clean_run]]) {
            const reply = await page.evaluate(async path => {
              const response = await fetch(path)
              return { status: response.status, body: await response.text() }
            }, lookup(date, saved))
            assert.equal(reply.status, status)
            for (const secret of [clean_run, statement, missing_run, oldRun, prior.detection.statement_id, prior.break_id]) assert(!reply.body.includes(secret))
          }
          assert.equal(await page.getByRole('button', { name: 'Download comparison receipt' }).count(), 0)
          continue
        }
        let downloadedHistory
        for (const [date, state, outcome, label, run, saved, reload] of [
          [clean_date, 'recorded', 'clean', 'Clean comparison', clean_run],
          [missing_date, 'recorded', 'no_statement', 'No statement received', missing_run],
          [unobserved_date, 'not_compared', undefined, 'No comparison recorded', undefined],
          [clean_date, 'recorded', 'breaks', 'Differences found', oldRun, oldRun],
          [clean_date, 'recorded', 'breaks', 'Differences found', oldRun, oldRun, true],
          [clean_date, 'recorded', 'clean', 'Clean comparison', clean_run, clean_run],
          [missing_date, 'unknown', undefined, 'Result unknown', undefined, oldRun],
          [clean_date, 'unknown', undefined, 'Result unknown', undefined, 'synthetic-absent-run'],
        ]) {
          if (reload) await page.reload()
          const region = page.getByRole('region', { name: 'Dated custody result' })
          await region.getByLabel('Portfolio ID', { exact: true }).fill(portfolio)
          await region.getByLabel('Custodian ID', { exact: true }).fill(custodian)
          await region.getByLabel('Business date', { exact: true }).fill(date)
          await region.getByLabel('Saved run ID (optional)', { exact: true }).fill(saved || '')
          const pending = page.waitForResponse(response => {
            const url = new URL(response.url())
            return url.pathname === endpoint && url.searchParams.get('business_date') === date && url.searchParams.get('run_id') === (saved || null) && response.request().method() === 'GET'
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
          assert.equal(receipt.statement_id, outcome === 'clean' ? statement : outcome === 'breaks' ? prior.detection.statement_id : undefined)
          stage = name + '-open-row'
          const result = region.locator('output[data-state="' + state + '"]')
          await region.getByText(label, { exact: false }).first().waitFor()
          await result.waitFor()
          const text = await region.innerText()
          if (run) assert(text.includes(run))
          if (outcome === 'clean' || outcome === 'breaks') {
            assert.equal(receipt.journal_position, '1')
            assert.equal(receipt.transaction_pass, 'ran')
            assert.equal(receipt.book_reference_count, '1')
            assert.equal(receipt.statement_reference_count, '1')
            assert.equal(receipt.unreferenced_book_trades, '0')
            assert(text.includes('Independent reference comparison'))
            assert(text.includes(receipt.statement_id))
          } else {
            assert.equal(receipt.transaction_pass, undefined)
            assert(!text.includes(statement))
          }
          if (saved) {
            assert(text.includes('Historical comparison'))
            if (run) {
              assert.equal(receipt.signed_statement_sha256, receipt.statement_id.slice(-64))
              if (outcome === 'breaks') {
                assert.equal(receipt.breaks.length, 1)
                const row = receipt.breaks[0]
                for (const k of ['break_id', 'key', 'ibor', 'custodian', 'difference']) assert.equal(row[k], prior[k])
                assert.equal(row.values_state, 'exact')
                const history = region.getByRole('table', { name: 'Differences at comparison' })
                const values = history.getByRole('row').filter({ has: page.getByRole('rowheader', { name: prior.break_id, exact: true }) })
                assert.deepEqual(await values.locator('td').allTextContents(), [row.kind, prior.key, prior.ibor, prior.custodian, prior.difference])
              } else assert.deepEqual(receipt.breaks, [])
            } else {
              assert.equal(receipt.signed_statement_sha256, undefined)
              assert.equal(receipt.breaks, undefined)
            }
          }
          if (run) {
            const bytes = await comparisonDownload(page, region, receipt)
            if (saved === oldRun) {
              if (reload) assert.deepEqual(bytes, downloadedHistory)
              else downloadedHistory = bytes
            }
          }
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
