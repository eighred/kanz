const assert = require('node:assert/strict')
const { comparisonDownload } = require('./custody-comparison-receipt.cjs')
const { chromium } = require(process.env.TEST_MFA_PLAYWRIGHT)

let stage = 'input'

async function operator(page, e) {
  const path = '/api/v1/portfolios/PF1/custody/reruns'
  const form = page.getByRole('region', { name: 'Rerun custody comparison' })
  await form.getByLabel('Portfolio ID').fill(e.portfolio_id)
  await form.getByLabel('Custodian ID').fill(e.custodian_id)
  await form.getByLabel('UTC business date').fill(e.date)
  await form.getByRole('button', { name: 'Review comparison' }).click()
  const key = await form.locator('code').innerText()
  assert.match(key, /^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$/)
  const command = { request_id: key, custodian_id: e.custodian_id, business_date: e.date }
  const pending = page.waitForResponse(r => new URL(r.url()).pathname === path && r.request().method() === 'POST')
  await form.getByRole('button', { name: 'Run comparison' }).click()
  const response = await pending
  stage = 'authorized-open-payload'
  assert.equal(response.status(), 200)
  assert.deepEqual(response.request().postDataJSON(), command)
  const receipt = await response.json()
  for (const [field, value] of Object.entries({ ...command, actor: 'custody-pilot-rerun-authorized',
    portfolio_id: e.portfolio_id, outcome: 'historical_clean', statement_id: e.statement_id, scope_mode: 'declared', journal_position: '1' })) {
    assert.equal(receipt[field], value)
  }
  assert(receipt.run_id && ![e.original_run, e.following_run].includes(receipt.run_id))
  const status = page.getByRole('status', { name: 'Recorded custody comparison' })
  await status.getByText('Balances agree at the historical close', { exact: true }).waitFor()
  await status.getByText(receipt.run_id, { exact: true }).waitFor()
  const replay = await page.evaluate(async ({ path, command }) => {
    const reply = await fetch(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(command) })
    return { status: reply.status, body: await reply.json() }
  }, { path, command })
  assert.equal(replay.status, 200)
  assert.deepEqual(replay.body, receipt)
}

async function customer(page, e) {
  const region = page.getByRole('region', { name: 'Dated custody result' })
  const row = page.getByRole('row').filter({ has: page.getByRole('rowheader', { name: e.break_id, exact: true }) })
  await row.getByText('View detection evidence', { exact: true }).click()
  await row.getByText(e.original_run, { exact: true }).waitFor()
  assert.equal(await page.getByRole('region', { name: 'Rerun custody comparison' }).count(), 0)
  const refused = await page.evaluate(async () => (await fetch('/api/v1/portfolios/PF1/custody/reruns', {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}',
  })).status)
  assert.equal(refused, 403)
  await region.getByLabel('Portfolio ID', { exact: true }).fill(e.portfolio_id)
  await region.getByLabel('Custodian ID', { exact: true }).fill(e.custodian_id)
  async function open(date, saved, button = 'View dated result', previous = false) {
    if (button === 'View dated result') {
      await region.getByLabel('Business date', { exact: true }).fill(date)
      await region.getByLabel('Saved run ID (optional)', { exact: true }).fill(saved || '')
    }
    const pending = page.waitForResponse(r => {
      const url = new URL(r.url())
      return r.request().method() === 'GET' && url.pathname === '/api/v1/portfolios/PF1/custody/comparison' &&
        url.searchParams.get('business_date') === date && url.searchParams.get('custodian_id') === e.custodian_id &&
        url.searchParams.get('run_id') === (saved || null) && url.searchParams.get('include') === (previous ? 'previous_comparison' : null)
    })
    await region.getByRole('button', { name: button }).click()
    const reply = await pending
    assert.equal(reply.status(), 200)
    const body = await reply.json()
    assert.equal(body.tenant_id, 'synthetic-custody-pilot')
    assert.equal(body.portfolio_id, e.portfolio_id)
    assert.equal(body.custodian_id, e.custodian_id)
    assert.equal(body.business_date, date)
    assert.equal(body.state, 'recorded')
    assert.equal(body.account_assignment_verified, true)
    assert.equal(body.journal_position, '1')
    await region.locator('output').getByText(body.run_id, { exact: true }).waitFor()
    return body
  }
  stage = 'authorized-open-details'
  const current = await open(e.date)
  assert.equal(current.outcome, 'historical_clean')
  assert.equal(current.run_id, e.run_id)
  assert.equal(current.statement_id, e.statement_id)
  assert((await region.locator('output').innerText()).includes('Historical close comparison'))
  const sealed = await open(e.date, e.run_id, 'View original comparison receipt')
  assert.equal(sealed.outcome, current.outcome)
  assert.equal(sealed.run_id, e.run_id)
  assert.equal(sealed.statement_id, e.statement_id)
  assert.equal(sealed.signed_statement_sha256, e.statement_id.slice(-64))
  assert.deepEqual(sealed.breaks, [])
  assert.deepEqual(sealed.execution_quantities, { compared: 1, unassessed: 0, differences: [] })
  await comparisonDownload(page, region, sealed)
  const original = await open(e.date, e.run_id, 'View previous statement comparison', true)
  assert.equal(original.previous_of_run_id, e.run_id)
  assert.equal(original.previous_of_statement_id, e.statement_id)
  assert.equal(original.run_id, e.original_run)
  assert.equal(original.statement_id, e.original_statement)
  assert.equal(original.signed_statement_sha256, e.original_statement.slice(-64))
  assert.equal(original.outcome, 'breaks')
  assert.equal(original.breaks.length, 1)
  const position = original.breaks[0]
  assert.deepEqual([position.kind, position.key, position.ibor, position.custodian, position.difference], ['quantity', 'SYN', '10.5', '9.5', '1'])
  const priorBytes = await comparisonDownload(page, region, original)
  const byID = await open(e.date, e.original_run)
  assert.deepEqual(await comparisonDownload(page, region, byID), priorBytes)
  stage = 'authorized-reload-details'
  for (const saved of ['', e.following_run]) {
    const following = await open(e.following_date, saved)
    assert.equal(following.outcome, 'clean')
    assert.equal(following.run_id, e.following_run)
    assert.equal(following.statement_id, e.following_statement)
    if (saved) {
      assert.deepEqual(following.breaks, [])
      assert.equal(following.signed_statement_sha256, e.following_statement.slice(-64))
    }
  }
}

async function main() {
  let raw = ''
  for await (const chunk of process.stdin) { raw += chunk; assert(raw.length < 64 * 1024) }
  const { base, cookies, expected: e } = JSON.parse(raw)
  const url = new URL(base)
  assert(url.protocol === 'http:' && url.hostname === '127.0.0.1' && url.origin === base && url.port)
  assert(['operator', 'customer'].includes(e.phase))
  assert.equal(e.portfolio_id, 'PF1')
  assert.equal(e.custodian_id, 'CUST-A')
  assert(e.date < e.following_date && e.original_run && e.following_run && e.original_run !== e.following_run)
  assert.match(e.break_id, /^q1:[a-f0-9]{64}$/)
  for (const statement of [e.statement_id, e.original_statement, e.following_statement]) {
    assert.match(statement, /^custody-batch\.v1\.[A-Za-z0-9._-]+\.r[1-9]\d*\.[a-f0-9]{64}$/)
  }
  if (e.phase === 'customer') assert(e.run_id && ![e.original_run, e.following_run].includes(e.run_id))
  stage = 'launch'
  const browser = await chromium.launch({ headless: true, timeout: 10_000 })
  try {
    const context = await browser.newContext()
    await context.route('**/*', route => new URL(route.request().url()).origin === base ? route.continue() : route.abort())
    await context.addCookies([{ name: 'kanz_session', value: cookies.authorized, url: base, httpOnly: true, sameSite: 'Lax' }])
    const page = await context.newPage()
    page.setDefaultTimeout(7000)
    stage = 'authorized-open-response'
    await page.goto(base + '/custody-breaks')
    if (e.phase === 'operator') await operator(page, e)
    else await customer(page, e)
  } finally { await browser.close() }
}

main().catch(() => { process.stderr.write('custody-browser-stage=' + stage + '\n'); process.exitCode = 1 })
