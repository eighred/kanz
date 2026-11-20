const assert = require('node:assert/strict')
const { writeFile } = require('node:fs/promises')
const { chromium } = require(process.env.TEST_MFA_PLAYWRIGHT)
const { comparisonDownload } = require('./custody-comparison-receipt.cjs')

let stage = 'input'
const endpoint = '/api/v1/portfolios/PF1/custody/comparison'

function lookup(date, run, custodian) {
  return endpoint + '?' + new URLSearchParams({ business_date: date, custodian_id: custodian, run_id: run })
}

async function customer(page, e) {
  const region = page.getByRole('region', { name: 'Dated custody result' })
  await region.getByLabel('Portfolio ID', { exact: true }).fill(e.portfolio_id)
  await region.getByLabel('Custodian ID', { exact: true }).fill(e.custodian_id)
  const permissions = await page.evaluate(async () => {
    const reply = await fetch('/api/v1/permissions')
    return { status: reply.status, body: await reply.json() }
  })
  assert.equal(permissions.status, 200)
  assert.equal(permissions.body.tenant, 'synthetic-custody-pilot')
  assert.deepEqual(permissions.body.portfolios, [e.portfolio_id])
  assert.deepEqual(permissions.body.capabilities.slice().sort(), ['custody_read', 'session.read'])
  assert(permissions.body.routes.length > 0 && permissions.body.routes.every(route => route.Pattern.startsWith('GET ')))
  assert.equal(await page.getByRole('region', { name: 'Rerun custody comparison' }).count(), 0)

  async function saved(date, run) {
    await region.getByLabel('Business date', { exact: true }).fill(date)
    await region.getByLabel('Saved run ID (optional)', { exact: true }).fill(run)
    const waiting = page.waitForResponse(response => {
      const url = new URL(response.url())
      return response.request().method() === 'GET' && url.pathname === endpoint &&
        url.searchParams.get('business_date') === date && url.searchParams.get('custodian_id') === e.custodian_id &&
        url.searchParams.get('run_id') === run
    })
    await region.getByRole('button', { name: 'View dated result' }).click()
    const reply = await waiting
    assert.equal(reply.status(), 200)
    const body = await reply.json()
    assert.equal(body.tenant_id, 'synthetic-custody-pilot')
    assert.equal(body.portfolio_id, e.portfolio_id)
    assert.equal(body.custodian_id, e.custodian_id)
    assert.equal(body.business_date, date)
    return body
  }

  stage = 'authorized-open-payload'
  const original = await saved(e.date, e.original_run)
  assert.equal(original.state, 'recorded')
  assert.equal(original.run_id, e.original_run)
  assert.equal(original.statement_id, e.original_statement)
  assert.equal(original.signed_statement_sha256, e.original_statement.slice(-64))
  assert.equal(original.account_assignment_verified, true)
  assert.equal(original.outcome, 'breaks')
  assert.equal(original.breaks.length, 1)
  const row = original.breaks[0]
  assert.equal(row.values_state, 'exact')
  const figures = ['quantity', 'SYN', '10.5', '9.5', '1']
  assert.deepEqual([row.kind, row.key, row.ibor, row.custodian, row.difference], figures)
  stage = 'authorized-open-row'
  await region.locator('output').getByText(e.original_run, { exact: true }).waitFor()
  const table = region.getByRole('table', { name: 'Differences at comparison' })
  const cells = table.getByRole('row', { name: row.break_id, exact: false }).locator('th,td')
  assert.deepEqual(await cells.allTextContents(), [row.break_id, ...figures])
  const bytes = await comparisonDownload(page, region, original)

  stage = 'authorized-reload-details'
  const following = await saved(e.following_date, e.following_run)
  assert.equal(following.state, 'recorded')
  assert.equal(following.run_id, e.following_run)
  assert.equal(following.statement_id, e.following_statement)
  assert.equal(following.signed_statement_sha256, e.following_statement.slice(-64))
  assert.equal(following.account_assignment_verified, true)
  assert.equal(following.outcome, 'clean')
  assert.deepEqual(following.breaks, [])
  await region.locator('output').getByText(e.following_run, { exact: true }).waitFor()
  assert.equal(await table.count(), 0)

  if (e.phase === 'restored') {
    const absent = await saved(e.date, e.future_run)
    assert.deepEqual(absent, { tenant_id: 'synthetic-custody-pilot', portfolio_id: e.portfolio_id,
      custodian_id: e.custodian_id, business_date: e.date, state: 'unknown' })
    await region.locator('output[data-state="unknown"]').waitFor()
    assert.equal(await region.getByRole('button', { name: 'Download comparison receipt' }).count(), 0)
    assert(!(await region.innerText()).includes(e.future_statement))
  }
  return bytes
}

async function main() {
  let raw = ''
  for await (const chunk of process.stdin) { raw += chunk; assert(raw.length < 64 * 1024) }
  const { base, cookies, expected: e } = JSON.parse(raw)
  const url = new URL(base)
  assert(url.protocol === 'http:' && url.hostname === '127.0.0.1' && url.origin === base && url.port)
  assert(['source', 'restored'].includes(e.phase))
  assert.equal(e.portfolio_id, 'PF1')
  assert.equal(e.custodian_id, 'CUST-A')
  assert(e.date < e.following_date && e.original_run && e.following_run && e.original_run !== e.following_run)
  const statements = [e.original_statement, e.following_statement]
  if (e.phase === 'restored') {
    assert(e.future_run && ![e.original_run, e.following_run].includes(e.future_run))
    statements.push(e.future_statement)
  }
  for (const id of statements) assert.match(id, /^custody-batch\.v1\.[A-Za-z0-9._-]+\.r[1-9]\d*\.[a-f0-9]{64}$/)
  assert.equal(new Set(statements).size, statements.length)
  assert(process.env.CUSTODY_PILOT_RETAINED_DOWNLOADS)
  stage = 'launch'
  const browser = await chromium.launch({ headless: true, timeout: 10_000 })
  let download
  try {
    for (const name of ['authorized', 'foreign', 'excluded']) {
      stage = name + '-open-response'
      const context = await browser.newContext()
      try {
        await context.route('**/*', route => new URL(route.request().url()).origin === base ? route.continue() : route.abort())
        await context.addCookies([{ name: 'kanz_session', value: cookies[name], url: base, httpOnly: true, sameSite: 'Lax' }])
        const page = await context.newPage()
        page.setDefaultTimeout(7000)
        await page.goto(base + '/custody-breaks')
        if (name === 'authorized') {
          download = await customer(page, e)
          continue
        }
        stage = name + '-open-payload'
        for (const [day, run] of [[e.date, e.original_run], [e.following_date, e.following_run]]) {
          const denied = await page.evaluate(async path => {
            const response = await fetch(path)
            return { status: response.status, text: await response.text() }
          }, lookup(day, run, e.custodian_id))
          assert.equal(denied.status, 404)
          for (const privateValue of [e.original_run, e.following_run, ...statements]) assert(!denied.text.includes(privateValue))
        }
      } finally { await context.close() }
    }
    stage = 'authorized-reload-payload'
    await writeFile(process.env.CUSTODY_PILOT_RETAINED_DOWNLOADS, download, { flag: 'wx', mode: 0o600 })
  } finally { await browser.close() }
}

main().catch(() => { process.stderr.write('custody-browser-stage=' + stage + '\n'); process.exitCode = 1 })
