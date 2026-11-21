// The signed-source pilot harness keeps synthetic session cookies on private stdin.
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
  assert.equal(origin.protocol, 'http:')
  assert.equal(origin.hostname, '127.0.0.1')
  assert.equal(origin.origin, base)
  assert(origin.port)
  assert.equal(expected.portfolio_id, 'PF1')
  assert.equal(expected.custodian_id, 'CUST-A')
  for (const name of ['break_id', 'key', 'ibor', 'custodian', 'difference']) assert.equal(typeof expected[name], 'string')
  assert.equal(expected.detection.business_date, '2020-02-04')
  assert.match(expected.detection.statement_id, /^custody-batch\.v1\.synthetic-source\.r1\.[0-9a-f]{64}$/)
  assert(expected.detection.run_id)
  const quantity = expected.execution_quantity
  if (quantity) {
    assert.equal(quantity.kind, 'execution_quantity')
    assert.match(quantity.break_id, /^q1:[0-9a-f]{64}$/)
    assert.deepEqual([quantity.key, quantity.instrument_id, quantity.ibor, quantity.custodian, quantity.difference],
      ['external-7', 'SYN', '10.5', '9.5', '1'])
    assert.deepEqual(quantity.detection, expected.detection)
  }
  const queue = quantity ? [expected, quantity] : [expected]
  const endpoint = base + '/api/v1/custody/breaks'
  stage = 'launch'
  const browser = await chromium.launch({ headless: true, timeout: 10_000 })
  try {
    for (const [name, status] of Object.entries({ authorized: 200, foreign: 404, excluded: 200, unfunded: 403 })) {
      const context = await browser.newContext()
      try {
        await context.route('**/*', route => new URL(route.request().url()).origin === base ? route.continue() : route.abort())
        await context.addCookies([{ name: 'kanz_session', value: cookies[name], url: base, httpOnly: true, sameSite: 'Lax' }])
        const page = await context.newPage()
        page.setDefaultTimeout(5_000)
        let priorReceipt
        for (const navigation of ['open', 'reload']) {
          stage = name + '-' + navigation + '-response'
          const response = page.waitForResponse(r => {
            const url = new URL(r.url())
            return url.origin + url.pathname === endpoint && r.request().method() === 'GET'
          })
          if (navigation === 'open') await page.goto(base + '/custody-breaks')
          else await page.reload()
          const received = await response
          const query = new URL(received.url()).searchParams
          assert.equal(query.get('limit'), '100')
          assert.equal(query.has('after'), false)
          assert.equal(received.status(), status)
          stage = name + '-' + navigation + '-payload'
          const data = await received.json()
          if (name !== 'authorized') {
            if (name === 'excluded') {
              assert.equal(data.count, 0)
              assert.deepEqual(data.breaks, [])
              await page.getByText('No outstanding custody breaks were returned', { exact: false }).waitFor()
            } else {
              await page.getByRole('alert').waitFor()
              assert.equal(await page.getByRole('table').count(), 0)
            }
            for (const secret of [...queue.map(item => item.break_id), expected.detection.run_id, expected.detection.statement_id]) {
              assert(!JSON.stringify(data).includes(secret))
              assert(!(await page.locator('body').innerText()).includes(secret))
            }
            continue
          }
          assert.equal(data.actions_enabled, false)
          if (navigation === 'open') {
            const access = await page.evaluate(async () => {
              const permissions = await fetch('/api/v1/permissions')
              const accounts = await fetch('/api/v1/broker/accounts')
              return { status: permissions.status, grants: await permissions.json(), accounts: accounts.status }
            })
            assert.equal(access.status, 200)
            assert.equal(access.grants.subject, 'custody-pilot-reader')
            assert.equal(access.grants.tenant, 'synthetic-custody-pilot')
            assert.deepEqual(access.grants.portfolios, [expected.portfolio_id])
            assert.deepEqual(access.grants.capabilities, ['custody_read', 'session.read'])
            assert.deepEqual(access.grants.routes, [
              { Pattern: 'GET /v1/custody/breaks', Capability: 'custody_read' },
              { Pattern: 'GET /v1/permissions', Capability: 'session.read' },
              { Pattern: 'GET /v1/portfolios/{id}/custody/comparison', Capability: 'custody_read' },
            ])
            assert.equal(access.accounts, 403)
          }
          assert.equal(await page.getByRole('region', { name: 'Rerun custody comparison' }).count(), 0)
          assert.equal(data.count, queue.length)
          assert.equal(data.breaks.length, queue.length)
          for (const item of queue) {
            const matches = data.breaks.filter(value => value.break_id === item.break_id)
            assert.equal(matches.length, 1)
            const record = matches[0]
            for (const field of ['portfolio_id', 'custodian_id', 'key', 'ibor', 'custodian', 'difference']) assert.equal(record[field], item[field])
            assert.deepEqual(record.detection, item.detection)
            assert.equal(record.values_state, 'exact')
            assert.equal(record.kind, item.kind)
            if (item.kind === 'execution_quantity') {
              assert.equal(record.instrument_id, item.instrument_id)
              assert.equal(record.revision, undefined)
            }
            stage = name + '-' + navigation + '-row'
            const row = page.getByRole('row').filter({ has: page.getByRole('rowheader', { name: item.break_id, exact: true }) })
            await row.waitFor()
            const identifiers = row.locator('td').filter({ hasText: item.kind })
            await identifiers.getByText(item.key, { exact: true }).waitFor()
            if (item.kind === 'execution_quantity') await identifiers.getByText(item.instrument_id, { exact: true }).waitFor()
            assert.equal(await row.getByRole('button', { name: /^(Claim|Explain)$/ }).count(), 0)
            const table = page.getByRole('table')
            const headers = await table.getByRole('columnheader').allTextContents()
            const cells = row.locator(':scope > th, :scope > td')
            for (const [heading, value] of [['IBOR', item.ibor], ['Custodian', item.custodian], ['Difference', item.difference]]) {
              assert.equal((await cells.nth(headers.indexOf(heading)).innerText()).trim(), value)
            }
            stage = name + '-' + navigation + '-details'
            await row.getByText('View detection evidence', { exact: true }).click()
            const labels = ['Portfolio', 'Custodian ID', 'Business date', 'Reconciliation run', 'Statement', 'Journal position', 'Account scope']
            const values = [item.portfolio_id, item.custodian_id, item.detection.business_date,
              item.detection.run_id, item.detection.statement_id, item.detection.journal_position ?? 'Unavailable', item.detection.scope_mode ?? 'Unavailable']
            const source = row.locator('details[open]')
            assert.deepEqual(await source.locator('dt').allInnerTexts(), labels)
            assert.deepEqual(await source.locator('dd').allInnerTexts(), values)
          }
          // Read the comparison independently of the outstanding-break queue.
          const dated = page.getByRole('region', { name: 'Dated custody result' })
          await dated.getByLabel('Portfolio ID', { exact: true }).fill(expected.portfolio_id)
          await dated.getByLabel('Custodian ID', { exact: true }).fill(expected.custodian_id)
          await dated.getByLabel('Business date', { exact: true }).fill(expected.detection.business_date)
          const request = page.waitForResponse(r => new URL(r.url()).pathname === '/api/v1/portfolios/PF1/custody/comparison')
          await dated.getByRole('button', { name: 'View dated result' }).click()
          const reply = await request
          assert.equal(reply.status(), 200)
          const comparison = await reply.json()
          assert.equal(comparison.tenant_id, 'synthetic-custody-pilot')
          assert.equal(comparison.run_id, expected.detection.run_id)
          assert.equal(comparison.statement_id, expected.detection.statement_id)
          assert.equal(comparison.outcome, 'breaks')
          assert.equal(comparison.transaction_pass, 'ran')
          assert.deepEqual([comparison.statement_reference_count, comparison.book_reference_count, comparison.unreferenced_book_trades], ['1', '1', '0'])
          const bytes = await comparisonDownload(page, dated, comparison)
          if (priorReceipt) assert.deepEqual(bytes, priorReceipt)
          priorReceipt = bytes
        }
      } finally { await context.close() }
    }
  } finally { await browser.close() }
}

main().catch(() => { process.stderr.write('custody-browser-stage=' + stage + '\n'); process.exitCode = 1 })
