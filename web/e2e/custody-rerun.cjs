// Private stdin from the loopback BFF fixture, with freshly issued sessions.
const assert = require('node:assert/strict')
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
  assert.equal(expected.detection.business_date, '2020-02-04')
  assert.match(expected.detection.statement_id, /^custody-batch\.v1\.synthetic-source\.r1\.[0-9a-f]{64}$/)
  const path = '/api/v1/portfolios/PF1/custody/reruns'
  let command, receipt
  stage = 'launch'
  const browser = await chromium.launch({ headless: true, timeout: 10_000 })
  try {
    for (const [name, status] of Object.entries({ authorized: 200, foreign: 404, excluded: 404, unfunded: 403 })) {
      const context = await browser.newContext()
      try {
        stage = name + '-open-response'
        await context.route('**/*', route => new URL(route.request().url()).origin === base ? route.continue() : route.abort())
        await context.addCookies([{ name: 'kanz_session', value: cookies[name], url: base, httpOnly: true, sameSite: 'Lax' }])
        const page = await context.newPage()
        page.setDefaultTimeout(7000)
        const permissions = name === 'authorized' ? page.waitForResponse(response =>
          response.url() === base + '/api/v1/permissions' && response.request().method() === 'GET') : undefined
        await page.goto(base + '/custody-breaks')
        if (name === 'authorized') {
          const response = await permissions
          assert.equal(response.status(), 200)
          const grants = await response.json()
          assert.equal(grants.subject, 'custody-pilot-rerun-authorized')
          assert.equal(grants.tenant, 'synthetic-custody-pilot')
          assert.deepEqual(grants.portfolios, [expected.portfolio_id])
          assert(grants.routes.some(route => route.Pattern === 'POST /v1/portfolios/{id}/custody/reruns' && route.Capability === 'custody_rerun'))
          const form = page.getByRole('region', { name: 'Rerun custody comparison' })
          await form.getByLabel('Portfolio ID').fill(expected.portfolio_id)
          await form.getByLabel('Custodian ID').fill(expected.custodian_id)
          await form.getByLabel('UTC business date').fill(expected.detection.business_date)
          await form.getByRole('button', { name: 'Review comparison' }).click()
          const requestID = await form.locator('code').innerText()
          assert.match(requestID, /^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$/)
          command = { request_id: requestID, custodian_id: expected.custodian_id, business_date: expected.detection.business_date }
          const received = page.waitForResponse(response => response.url() === base + path && response.request().method() === 'POST')
          await form.getByRole('button', { name: 'Run comparison' }).click()
          const result = await received
          stage = 'authorized-open-payload'
          assert.equal(result.status(), 200)
          assert.deepEqual(result.request().postDataJSON(), command)
          receipt = await result.json()
          assert.equal(receipt.actor, 'custody-pilot-rerun-authorized')
          assert.equal(receipt.request_id, requestID)
          assert.equal(receipt.statement_id, expected.detection.statement_id)
          assert.equal(receipt.outcome, 'breaks')
          assert.equal(receipt.journal_position, expected.detection.journal_position)
          assert.equal(receipt.scope_mode, expected.detection.scope_mode)
          assert.equal(receipt.business_date, command.business_date)
          assert.equal(receipt.portfolio_id, expected.portfolio_id)
          assert.equal(receipt.custodian_id, command.custodian_id)
          assert(receipt.run_id && receipt.run_id !== expected.detection.run_id)
          stage = 'authorized-open-row'
          const statusText = page.getByRole('status', { name: 'Recorded custody comparison' })
          await statusText.waitFor()
          for (const text of [receipt.run_id, receipt.request_id, receipt.statement_id]) assert((await statusText.innerText()).includes(text))
          await page.reload()
          const row = page.getByRole('row').filter({ has: page.getByRole('rowheader', { name: expected.break_id, exact: true }) })
          await row.getByText('View detection evidence', { exact: true }).click()
          assert((await row.innerText()).includes(receipt.run_id))
        }
        stage = name + '-reload-response'
        const response = await page.evaluate(async ({path, command}) => {
          const reply = await fetch(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(command) })
          return { code: reply.status, data: await reply.json() }
        }, { path, command })
        assert.equal(response.code, status)
        if (name === 'authorized') assert.deepEqual(response.data, receipt)
        else {
          for (const id of [receipt.run_id, receipt.statement_id, command.request_id]) assert(!JSON.stringify(response.data).includes(id))
          if (name === 'unfunded') assert.equal(await page.getByRole('region', { name: 'Rerun custody comparison' }).count(), 0)
        }
      } finally { await context.close() }
    }
  } finally { await browser.close() }
}

main().catch(() => { process.stderr.write('custody-browser-stage=' + stage + '\n'); process.exitCode = 1 })
