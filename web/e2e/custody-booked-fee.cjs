// An approved book amendment changes the comparison of the same signed close.
const assert = require('node:assert/strict')
const fs = require('node:fs/promises')
const { chromium } = require(process.env.TEST_MFA_PLAYWRIGHT)
const { comparisonDownload } = require('./custody-comparison-receipt.cjs')

let stage = 'input'
async function main() {
  let raw = ''
  for await (const chunk of process.stdin) {
    raw += chunk
    assert(raw.length < 64 * 1024)
  }
  const { base, cookies, expected } = JSON.parse(raw)
  const { phase, date, statement_id: statement, original_run: original, run_id: corrected } = expected
  const origin = new URL(base)
  assert(origin.origin === base && origin.protocol === 'http:' && origin.hostname === '127.0.0.1' && origin.port)
  assert(['original', 'pending', 'corrected'].includes(phase))
  assert.match(statement, /^custody-batch\.v1\.synthetic-source\.r1\.[0-9a-f]{64}$/)
  assert(original && (phase !== 'corrected' || (corrected && corrected !== original)))
  const path = process.env.CUSTODY_PILOT_RETAINED_DOWNLOADS
  assert(path)
  const endpoint = '/api/v1/portfolios/PF1/custody/comparison'
  const query = id => {
    const q = new URLSearchParams({ custodian_id: 'CUST-A', business_date: date })
    if (id) q.set('run_id', id)
    return endpoint + '?' + q
  }

  stage = 'launch'
  const browser = await chromium.launch({ headless: true, timeout: 10000 })
  async function contextFor(name) {
    const ctx = await browser.newContext()
    await ctx.route('**/*', route => new URL(route.request().url()).origin === base ? route.continue() : route.abort())
    await ctx.addCookies([{ name: 'kanz_session', value: cookies[name], url: base, httpOnly: true, sameSite: 'Lax' }])
    return ctx
  }
  try {
    const ctx = await contextFor('authorized')
    const page = await ctx.newPage()
    page.setDefaultTimeout(7000)
    const region = page.getByRole('region', { name: 'Dated custody result' })
    async function lookup(id, openCurrent = false) {
      if (!openCurrent) {
        await region.getByLabel('Portfolio ID', { exact: true }).fill('PF1')
        await region.getByLabel('Custodian ID', { exact: true }).fill('CUST-A')
        await region.getByLabel('Business date', { exact: true }).fill(date)
        await region.getByLabel('Saved run ID (optional)', { exact: true }).fill(id || '')
      }
      const ready = page.waitForResponse(r => r.request().method() === 'GET' && r.url() === base + query(id))
      await region.getByRole('button', { name: openCurrent ? 'View original comparison receipt' : 'View dated result' }).click()
      const response = await ready
      assert.equal(response.status(), 200)
      const body = await response.json()
      assert.deepEqual([body.tenant_id, body.portfolio_id, body.custodian_id, body.business_date, body.statement_id],
        ['synthetic-custody-pilot', 'PF1', 'CUST-A', date, statement])
      return body
    }
    function summary(body, amended) {
      assert.equal(body.state, 'recorded')
      assert.equal(body.run_id, amended ? corrected : original)
      assert.equal(body.outcome, amended ? 'breaks' : 'clean')
      assert.equal(body.execution_economics_status, amended ? 'equal' : 'mismatch')
      assert.equal(body.account_assignment_verified, true)
      assert.equal(body.journal_position, amended ? '2' : '1')
    }
    async function capture(body, amended) {
      summary(body, amended)
      assert.equal(body.signed_statement_sha256, statement.slice(-64))
      assert.equal(body.execution_economics.rows.length, 1)
      const row = body.execution_economics.rows[0]
      assert.deepEqual([row.external_ref, row.instrument_id, row.currency_code, row.status],
        ['external-7', 'SYN', 'USD', amended ? 'equal' : 'mismatch'])
      assert.equal(body.breaks.length, amended ? 1 : 0)
      if (amended) {
        const cash = body.breaks[0]
        assert.deepEqual([cash.kind, cash.key, cash.ibor, cash.custodian, cash.difference], ['cash', 'USD', '-11', '-10.75', '-0.25'])
        const cashRow = region.getByRole('table', { name: 'Differences at comparison' }).getByRole('row').last()
        await cashRow.getByRole('cell', { name: 'USD', exact: true }).waitFor()
        assert.deepEqual(await cashRow.getByRole('cell').allTextContents(), ['cash', 'USD', '-11', '-10.75', '-0.25'])
      }
      const table = region.getByRole('table', { name: 'Execution economics for external-7' })
      for (const [measure, ...values] of [
        ['Signed quantity', 'SYN', '10.5', '10.5', '0'],
        ['Unit price', 'USD per SYN', '1', '1', '0'],
        ['Signed gross cash', 'USD', '-10.5', '-10.5', '0'],
        ['Commission', 'USD', amended ? '0.5' : '0.25', '0.5', amended ? '0' : '-0.25'],
        ['Signed net cash', 'USD', amended ? '-11' : '-10.75', '-11', amended ? '0' : '0.25'],
        ['Settled cash', 'USD', amended ? '-11' : '-10.75', '-11', amended ? '0' : '0.25'],
      ]) {
        const line = table.getByRole('row').filter({ has: page.getByRole('rowheader', { name: measure, exact: true }) })
        await line.waitFor()
        assert.deepEqual(await line.getByRole('cell').allTextContents(), values)
      }
      return comparisonDownload(page, region, body, row.status)
    }
    stage = 'authorized-open-response'
    await page.goto(base + '/custody-breaks')
    const current = await lookup()
    let retained
    if (phase === 'pending') {
      stage = 'authorized-open-row'
      assert.deepEqual([current.state, current.reason], ['pending', 'journal_changed'])
      for (const field of ['run_id', 'outcome', 'journal_position', 'execution_economics_status', 'execution_economics']) assert.equal(current[field], undefined)
      await region.getByText('Journal changes awaiting comparison', { exact: true }).waitFor()
      assert.equal(await region.getByRole('button', { name: 'Download comparison receipt' }).count(), 0)
    } else {
      const amended = phase === 'corrected'
      summary(current, amended)
      assert.equal(current.execution_economics, undefined)
      await region.getByText(amended ? 'Execution economics agree' : 'Execution economics differ', { exact: true }).waitFor()
      stage = 'authorized-open-details'
      retained = await capture(await lookup(current.run_id, true), amended)
    }
    stage = 'authorized-reload-details'
    await page.reload()
    let originalReceipt
    if (phase === 'corrected') {
      const latest = await lookup()
      summary(latest, true)
      assert.equal(await region.getByLabel('Saved run ID (optional)', { exact: true }).inputValue(), '')
      const ready = page.waitForResponse(r => r.request().method() === 'GET' &&
        r.url() === base + query(latest.run_id) + '&include=previous_book_comparison')
      await region.getByRole('button', { name: 'Find earlier book comparison' }).click()
      const response = await ready
      assert.equal(response.status(), 200)
      originalReceipt = await response.json()
      assert.deepEqual([originalReceipt.previous_of_run_id, originalReceipt.previous_of_statement_id, originalReceipt.previous_of_journal_position],
        [latest.run_id, latest.statement_id, latest.journal_position])
      assert.equal(originalReceipt.statement_id, latest.statement_id)
    } else originalReceipt = await lookup(original)
    const reopened = await capture(originalReceipt, false)
    if (phase === 'original') {
      assert.deepEqual(reopened, retained)
      await fs.writeFile(path + '.fee-original', retained, { flag: 'wx', mode: 0o600 })
    } else assert.deepEqual(reopened, await fs.readFile(path + '.fee-original'))
    if (phase !== 'corrected') return
    assert.deepEqual(await capture(await lookup(corrected), true), retained)
    await ctx.close()
    for (const [name, status] of Object.entries({ foreign: 404, excluded: 404, unfunded: 403 })) {
      stage = name + '-open-response'
      const denied = await contextFor(name)
      try {
        for (const path of [query(''), query(original), query(corrected), query(corrected) + '&include=previous_book_comparison']) {
          const response = await denied.request.get(base + path, { maxRedirects: 0 })
          assert.equal(response.status(), status)
          const text = await response.text()
          for (const privateValue of [original, corrected, statement, 'external-7', 'execution_economics']) assert(!text.includes(privateValue))
        }
      } finally { await denied.close() }
    }
    await fs.writeFile(path + '.fee-corrected', retained, { flag: 'wx', mode: 0o600 })
  } finally { await browser.close() }
}
main().catch(() => { process.stderr.write('custody-browser-stage=' + stage + '\n'); process.exitCode = 1 })
