const assert = require('node:assert/strict')
const { writeFile } = require('node:fs/promises')
const { comparisonDownload } = require('./custody-comparison-receipt.cjs')
const { chromium } = require(process.env.TEST_MFA_PLAYWRIGHT)

let stage = 'input'
async function main() {
  let input = ''
  for await (const chunk of process.stdin) {
    input += chunk
    assert(input.length < 64 * 1024)
  }
  const { base, cookies, expected: e } = JSON.parse(input)
  const url = new URL(base)
  assert.equal(url.origin, base)
  assert.equal(url.protocol, 'http:')
  assert.equal(url.hostname, '127.0.0.1')
  assert(url.port)
  assert(e.business_date < e.following_date)
  assert.match(e.break_id, /^q1:[a-f0-9]{64}$/)
  assert.equal(e.agreed_quantity, '10.5')
  const runs = [e.original_run, e.following_run, e.correction_run]
  const sources = [e.original_statement, e.following_statement, e.corrected_statement]
  assert.equal(new Set(runs).size, 3)
  assert.equal(new Set(sources).size, 3)
  for (const source of sources) assert.match(source, /^custody-batch\.v1\.[A-Za-z0-9._-]+\.r[1-9]\d*\.[a-f0-9]{64}$/)
  const path = '/api/v1/portfolios/' + encodeURIComponent(e.portfolio) + '/custody/comparison?'
  const lookup = (date, run) => path + new URLSearchParams({ custodian_id: e.custodian, business_date: date, ...(run && { run_id: run }) })
  const correctionRow = { break_id: e.break_id, detection_run_id: e.original_run, entry_id: e.entry_id,
    external_ref: e.external_ref, instrument_id: e.instrument_id, agreed_quantity: { coefficient: '105', exponent: -1 } }
  let retained
  stage = 'launch'
  const browser = await chromium.launch({ headless: true, timeout: 10_000 })
  try {
    for (const [name, status] of Object.entries({ authorized: 200, foreign: 404, excluded: 404, unfunded: 403 })) {
      stage = name + '-open'
      const context = await browser.newContext()
      try {
        await context.route('**/*', route => new URL(route.request().url()).origin === base ? route.continue() : route.abort())
        await context.addCookies([{ name: 'kanz_session', value: cookies[name], url: base, httpOnly: true, sameSite: 'Lax' }])
        const page = await context.newPage()
        page.setDefaultTimeout(6000)
        await page.goto(base + '/custody-breaks')
        const region = page.getByRole('region', { name: 'Dated custody result' })
        if (name !== 'authorized') {
          stage = name + '-tenant-portfolio-permission'
          for (const [date, run] of [[e.business_date], [e.business_date, e.original_run], [e.business_date, e.correction_run], [e.following_date, e.following_run]]) {
            const response = await page.evaluate(async target => {
              const res = await fetch(target)
              return { status: res.status, body: await res.text() }
            }, lookup(date, run))
            assert.equal(response.status, status)
            for (const field of [...runs, ...sources, e.break_id, e.entry_id, e.external_ref, 'historical_quantity_correction_run_id', 'historical_quantity_resolutions']) {
              assert(!response.body.includes(field))
            }
          }
          assert.equal(await page.getByRole('button', { name: 'Download comparison receipt' }).count(), 0)
          continue
        }
        async function read(date, run, discover = false) {
          if (!discover) {
            await region.getByLabel('Portfolio ID', { exact: true }).fill(e.portfolio)
            await region.getByLabel('Custodian ID', { exact: true }).fill(e.custodian)
            await region.getByLabel('Business date', { exact: true }).fill(date)
            await region.getByLabel('Saved run ID (optional)', { exact: true }).fill(run || '')
          }
          const response = page.waitForResponse(res => res.url() === base + lookup(date, run) && res.request().method() === 'GET')
          await region.getByRole('button', { name: discover ? 'View corrected execution quantities' : 'View dated result', exact: true }).click()
          const res = await response
          assert.equal(res.status(), 200)
          const receipt = await res.json()
          assert.equal(receipt.tenant_id, 'synthetic-custody-pilot')
          assert.equal(receipt.portfolio_id, e.portfolio)
          assert.equal(receipt.custodian_id, e.custodian)
          assert.equal(receipt.business_date, date)
          await region.locator('output[data-state="' + receipt.state + '"]').waitFor()
          return receipt
        }
        stage = 'pending-corrected-source'
        const pending = await read(e.business_date)
        assert.equal(pending.state, 'pending')
        assert.equal(pending.statement_id, e.corrected_statement)
        assert.equal(pending.historical_quantity_correction_run_id, e.correction_run)
        assert.equal(pending.historical_quantity_resolutions, undefined)
        assert.equal(await region.getByRole('button', { name: 'Download comparison receipt' }).count(), 0)
        stage = 'correction-discovery'
        const corrected = await read(e.business_date, e.correction_run, true)
        assert.equal(corrected.state, 'recorded')
        assert.equal(corrected.outcome, 'historical_quantity_correction')
        assert.equal(corrected.run_id, e.correction_run)
        assert.equal(corrected.statement_id, e.corrected_statement)
        assert.equal(corrected.signed_statement_sha256, e.corrected_statement.slice(-64))
        assert.equal(corrected.scope_mode, 'declared')
        assert.equal(corrected.account_assignment_verified, true)
        assert.match(corrected.journal_position, /^(0|[1-9]\d*)$/)
        assert.deepEqual(corrected.historical_quantity_resolutions, [correctionRow])
        const table = region.getByRole('table', { name: 'Execution quantity resolutions' })
        const cells = await table.locator('tbody tr').first().locator('th,td').allTextContents()
        assert.deepEqual(cells, [e.external_ref, e.instrument_id, e.agreed_quantity, e.entry_id, e.original_run, e.break_id])
        const bytes = await comparisonDownload(page, region, corrected)
        retained = JSON.parse(bytes)
        stage = 'correction-recovery'
        await page.reload()
        const again = await read(e.business_date, e.correction_run)
        assert((await comparisonDownload(page, region, again)).equals(bytes))
        stage = 'original-and-following'
        for (const [date, run, source] of [[e.business_date, e.original_run, e.original_statement], [e.following_date, e.following_run, e.following_statement]]) {
          const ordinary = await read(date, run)
          assert.equal(ordinary.state, 'recorded')
          assert(['clean', 'breaks'].includes(ordinary.outcome))
          assert.equal(ordinary.run_id, run)
          assert.equal(ordinary.statement_id, source)
          assert.equal(ordinary.signed_statement_sha256, source.slice(-64))
          assert.equal(ordinary.account_assignment_verified, true)
          assert.equal(ordinary.historical_quantity_resolutions, undefined)
          assert(Array.isArray(ordinary.breaks))
          if (run === e.original_run) {
            const detection = ordinary.execution_quantities.differences.find(row => row.external_ref === e.external_ref && row.instrument_id === e.instrument_id)
            assert(detection)
            assert.notDeepEqual(detection.ibor, detection.custodian)
          }
          await comparisonDownload(page, region, ordinary)
          assert.equal(await table.count(), 0)
        }
        const latest = await read(e.following_date)
        assert.equal(latest.state, 'recorded')
        assert.equal(latest.run_id, e.following_run)
        assert.equal(latest.statement_id, e.following_statement)
        assert.equal(latest.historical_quantity_correction_run_id, undefined)
      } finally { await context.close() }
    }
  } finally { await browser.close() }
  stage = 'retain-corrective-download'
  assert(retained)
  if (process.env.CUSTODY_PILOT_RETAINED_DOWNLOADS) {
    await writeFile(process.env.CUSTODY_PILOT_RETAINED_DOWNLOADS, JSON.stringify(retained), { flag: 'wx', mode: 0o600 })
  }
}

main().catch(() => { process.stderr.write('custody-late-browser-stage=' + stage + '\n'); process.exitCode = 1 })
