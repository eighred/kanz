// Run only against the isolated custody pilot with its issued read-only sessions.
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
  const endpoint = '/api/v1/portfolios/PF1/custody/comparison'
  const prior = expected.prior
  const currentQuery = new URLSearchParams({ custodian_id: 'CUST-A', business_date: expected.date })
  const previousQuery = new URLSearchParams(currentQuery)
  previousQuery.set('run_id', expected.current_run)
  previousQuery.set('include', 'previous_comparison')
  const previous = endpoint + '?' + previousQuery
  stage = 'launch'
  const browser = await chromium.launch({ headless: true, timeout: 10_000 })
  const contextFor = async name => {
    const context = await browser.newContext()
    await context.route('**/*', route => new URL(route.request().url()).origin === base ? route.continue() : route.abort())
    await context.addCookies([{ name: 'kanz_session', value: cookies[name], url: base, httpOnly: true, sameSite: 'Lax' }])
    return context
  }
  try {
    stage = 'authorized'
    const context = await contextFor('authorized')
    const page = await context.newPage()
    page.setDefaultTimeout(6000)
    let original
    for (const reopen of [false, true]) {
      if (reopen) await page.reload()
      else await page.goto(base + '/custody-breaks')
      const region = page.getByRole('region', { name: 'Dated custody result' })
      await region.getByLabel('Portfolio ID', { exact: true }).fill('PF1')
      await region.getByLabel('Custodian ID', { exact: true }).fill('CUST-A')
      await region.getByLabel('Business date', { exact: true }).fill(expected.date)
      assert.equal(await region.getByLabel('Saved run ID (optional)', { exact: true }).inputValue(), '')
      const result = page.waitForResponse(r => new URL(r.url()).pathname === endpoint && !new URL(r.url()).searchParams.has('run_id'))
      await region.getByRole('button', { name: 'View dated result' }).click()
      const current = await (await result).json()
      assert.equal(current.outcome, 'breaks')
      assert.equal(current.run_id, expected.current_run)
      assert.equal(current.statement_id, expected.current_statement)
      await region.getByText('Differences found', { exact: false }).waitFor()
      const lookup = page.waitForResponse(r => new URL(r.url()).searchParams.get('include') === 'previous_comparison')
      await region.getByRole('button', { name: 'View previous statement comparison' }).click()
      const response = await lookup
      assert.equal(response.status(), 200)
      const receipt = await response.json()
      assert.equal(receipt.previous_of_run_id, expected.current_run)
      assert.equal(receipt.previous_of_statement_id, expected.current_statement)
      assert.equal(receipt.run_id, prior.detection.run_id)
      assert.equal(receipt.statement_id, prior.detection.statement_id)
      assert.equal(receipt.signed_statement_sha256, prior.detection.statement_id.slice(-64))
      assert.equal(receipt.breaks.length, 1)
      for (const key of ['break_id', 'kind', 'key', 'ibor', 'custodian', 'difference']) assert.equal(receipt.breaks[0][key], prior[key])
      assert.deepEqual([prior.ibor, prior.custodian, prior.difference], ['10.5', '9.5', '1'])
      const bytes = await comparisonDownload(page, region, receipt)
      if (reopen) assert.deepEqual(bytes, original)
      else original = bytes
    }
    await context.close()
    for (const [name, status] of Object.entries({ foreign: 404, excluded: 404, unfunded: 403 })) {
      stage = name
      const context = await contextFor(name)
      try {
        const response = await context.request.get(base + previous)
        assert.equal(response.status(), status)
        const body = await response.text()
        for (const id of [expected.current_run, expected.current_statement, prior.detection.run_id, prior.detection.statement_id, prior.break_id]) {
          assert(!body.includes(id))
        }
      } finally { await context.close() }
    }
  } finally { await browser.close() }
}

main().catch(() => { process.stderr.write('custody-previous-browser-stage=' + stage + '\n'); process.exitCode = 1 })
