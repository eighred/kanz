const assert = require('node:assert/strict')
const { chromium } = require(process.env.TEST_MFA_PLAYWRIGHT)

let stage = 'input'
async function main() {
  let raw = ''
  for await (const chunk of process.stdin) {
    raw += chunk
    assert(raw.length < 65536)
  }
  const { base, cookies, expected } = JSON.parse(raw)
  assert.equal(new URL(base).origin, base)
  assert.equal(new URL(base).hostname, '127.0.0.1')
  const portfolio = 'PF1'
  const endpoint = '/api/v1/portfolios/PF1/daily-return'
  stage = 'launch'
  const browser = await chromium.launch({ headless: true })
  try {
    for (const [role, status] of Object.entries({ authorized: 200, foreign: 404, excluded: 404, unfunded: 403 })) {
      const context = await browser.newContext()
      try {
        await context.route('**/*', route => new URL(route.request().url()).origin === base ? route.continue() : route.abort())
        await context.addCookies([{ name: 'kanz_session', value: cookies[role], url: base, httpOnly: true, sameSite: 'Lax' }])
        const page = await context.newPage()
        page.setDefaultTimeout(8000)
        stage = role + '-open-response'
        await page.goto(base + '/portfolios/' + portfolio + '/exposure')
        const section = page.getByRole('region', { name: 'Daily return' })
        const check = async (edition, code) => {
          await section.getByLabel('UTC close date').fill(expected.date)
          await section.getByLabel('Reporting currency').fill('USD')
          await section.getByLabel('Knowledge time (UTC)').fill(edition.as_of)
          const pending = page.waitForResponse(response => new URL(response.url()).pathname === endpoint)
          await section.getByRole('button', { name: 'View daily return' }).click()
          const response = await pending
          assert.equal(response.status(), code)
          return response.json()
        }
        if (role !== 'authorized') {
          const body = await check(expected.editions[0], status)
          assert(!JSON.stringify(body).includes(expected.editions[0].digest))
          continue
        }
        const [original, corrected, unknown] = expected.editions
        async function measured(edition, value, text) {
          const body = await check(edition, 200)
          assert.equal(body.source_digest, edition.digest)
          assert.equal(body.measured.return_value, value)
          assert.equal(body.external_flow_count, '0')
          assert.equal(body.unassessed_reason, undefined)
          await section.getByText(text, { exact: true }).waitFor()
          return body
        }
        stage = 'authorized-open-payload'
        const originalBody = await measured(original, 0.02, '2%')
        const updated = await measured(corrected, 0, '0%')
        assert.equal(updated.previous_source_digest, original.digest)
        const unassessed = await check(unknown, 200)
        assert.equal(unassessed.source_digest, unknown.digest)
        assert.equal(unassessed.measured, undefined)
        assert.equal(unassessed.external_flow_count, undefined)
        assert.equal(unassessed.unassessed_reason, 'flow_coverage_unknown')
        await section.getByText('Unassessed. External flow coverage is unavailable.', { exact: false }).waitFor()
        stage = 'authorized-reload-details'
        await page.reload()
        assert.deepEqual(await measured(original, 0.02, '2%'), originalBody)
        await check({ as_of: '2020-02-04T06:00:00Z' }, 404)
      } finally {
        await context.close()
      }
    }
  } finally {
    await browser.close()
  }
}
main().catch(() => {
  process.stderr.write('return-browser-stage=' + stage + '\n')
  process.exitCode = 1
})
