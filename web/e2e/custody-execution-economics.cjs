const assert = require('node:assert/strict')
const fs = require('node:fs/promises')
const { createHash } = require('node:crypto')
const { chromium } = require(process.env.TEST_MFA_PLAYWRIGHT)

let stage = 'input'
async function main() {
  let input = ''
  for await (const chunk of process.stdin) {
    input += chunk
    assert(input.length < 65536)
  }
  const { base, cookies, expected } = JSON.parse(input)
  const { revision, date, run_id: run, statement_id: statement, original_run: original } = expected
  assert.equal(new URL(base).origin, base)
  assert.equal(new URL(base).hostname, '127.0.0.1')
  const pathname = '/api/v1/portfolios/PF1/custody/comparison'
  const file = process.env.CUSTODY_PILOT_RETAINED_DOWNLOADS
  const states = ['mismatch', 'equal', 'unknown', 'unknown']
  const labels = { mismatch: 'Execution economics differ', equal: 'Execution economics agree', unknown: 'Execution economics unknown' }
  const state = states[revision - 1]
  const browser = await chromium.launch({ headless: true })
  const history = revision > 1 ? JSON.parse(await fs.readFile(file, 'utf8')) : []
  try {
    const roles = revision === 2
      ? { authorized: 200, foreign: 404, excluded: 404, unfunded: 403 }
      : { authorized: 200 }
    for (const [name, permitted] of Object.entries(roles)) {
      const context = await browser.newContext()
      try {
        await context.route('**/*', route => new URL(route.request().url()).origin === base ? route.continue() : route.abort())
        await context.addCookies([{ name: 'kanz_session', value: cookies[name], url: base, httpOnly: true, sameSite: 'Lax' }])
        const page = await context.newPage()
        page.setDefaultTimeout(7000)
        stage = name + '-open-response'
        await page.goto(base + '/custody-breaks')
        if (name !== 'authorized') {
          for (const saved of [run, original]) {
            const query = new URLSearchParams({ custodian_id: 'CUST-A', business_date: date, run_id: saved })
            const reply = await page.evaluate(async path => {
              const r = await fetch(path)
              return [r.status, await r.text()]
            }, pathname + '?' + query)
            assert.equal(reply[0], permitted)
            for (const restricted of [saved, 'external-7', 'execution_economics']) assert(!reply[1].includes(restricted))
          }
          continue
        }
        const region = page.getByRole('region', { name: 'Dated custody result' })
        const answer = id => page.waitForResponse(r => {
          const u = new URL(r.url())
          return r.request().method() === 'GET' && u.pathname === pathname && u.searchParams.get('run_id') === id
        })
        async function dated(id) {
          await region.getByLabel('Portfolio ID', { exact: true }).fill('PF1')
          await region.getByLabel('Custodian ID', { exact: true }).fill('CUST-A')
          await region.getByLabel('Business date', { exact: true }).fill(date)
          await region.getByLabel('Saved run ID (optional)', { exact: true }).fill(id || '')
          const pending = answer(id)
          await region.getByRole('button', { name: 'View dated result' }).click()
          const r = await pending
          assert.equal(r.status(), 200)
          return r.json()
        }
        async function download(body) {
          const pending = page.waitForEvent('download')
          await region.getByRole('button', { name: 'Download comparison receipt' }).click()
          const item = await pending
          const stream = await item.createReadStream()
          const chunks = []
          let size = 0
          for await (const chunk of stream) {
            size += chunk.length
            assert(size < 65536)
            chunks.push(chunk)
          }
          const bytes = Buffer.concat(chunks)
          await item.delete()
          const doc = JSON.parse(bytes)
          assert.equal(doc.run_id, body.run_id)
          assert.equal(doc.signed_statement_sha256, body.statement_id.slice(-64))
          assert.deepEqual(doc.execution_economics, body.execution_economics)
          return { receipt: doc, sha256: createHash('sha256').update(bytes).digest('hex') }
        }
        const current = await dated(null)
        assert.equal(current.run_id, run)
        assert.equal(current.execution_economics_status, state)
        await region.getByText(labels[state], { exact: true }).first().waitFor()
        assert(!(await region.innerText()).includes('Individual trade quantities, prices and cash amounts remain unverified.'))
        stage = 'authorized-open-details'
        const pending = answer(run)
        await region.getByRole('button', { name: 'View original comparison receipt' }).click()
        const body = await (await pending).json()
        assert.equal(body.statement_id, statement)
        assert.equal(body.execution_economics.rows.length, 1)
        const row = body.execution_economics.rows[0]
        assert.equal(row.external_ref, 'external-7')
        assert.equal(row.status, state)
        const economics = page.getByRole('region', { name: 'Execution economics', exact: true })
        await economics.getByText('external-7', { exact: false }).first().waitFor()
        if (state === 'unknown') {
          const reason = revision === 3
            ? 'The signed cash is inconsistent with the stated execution terms.'
            : 'The signed statement gives no price and cash convention.'
          await economics.getByText(reason, { exact: true }).waitFor()
          assert.equal(await economics.getByRole('table').count(), 0)
        } else {
          const table = economics.getByRole('table')
          const first = revision === 1
          for (const [label, unit, book, custodian, delta] of [
            ['Signed quantity', 'SYN', '10.5', '10.5', '0'],
            ['Unit price', 'USD per SYN', '1', '1', '0'],
            ['Signed gross cash', 'USD', '-10.5', '-10.5', '0'],
            ['Commission', 'USD', '0.25', first ? '0.5' : '0.25', first ? '-0.25' : '0'],
            ['Signed net cash', 'USD', '-10.75', first ? '-11' : '-10.75', first ? '0.25' : '0'],
            ['Settled cash', 'USD', '-10.75', first ? '-11' : '-10.75', first ? '0.25' : '0'],
          ]) {
            const heading = page.getByRole('rowheader', { name: label, exact: true })
            const line = table.getByRole('row').filter({ has: heading })
            assert.deepEqual(await line.getByRole('cell').allTextContents(), [unit, book, custodian, delta])
          }
          assert((await economics.innerText()).includes(date))
        }
        const retained = await download(body)
        history.push(retained)
        await page.reload()
        const same = await dated(run)
        assert.deepEqual(await download(same), retained)
        if (revision === 2) {
          const previous = await dated(original)
          assert.equal(previous.execution_economics.rows[0].status, 'mismatch')
          assert.deepEqual(await download(previous), history[0])
        }
      } finally { await context.close() }
    }
    await fs.writeFile(file, JSON.stringify(history), { mode: 0o600 })
  } finally { await browser.close() }
}
main().catch(() => { process.stderr.write('custody-browser-stage = '+stage+'\n'); process.exitCode=1 })
