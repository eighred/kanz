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
          await section.getByLabel('UTC close date').fill(edition.date || expected.date)
          await section.getByLabel('Reporting currency').fill('USD')
          await section.getByLabel('Knowledge time (UTC)').fill(edition.as_of)
          const pending = page.waitForResponse(response => new URL(response.url()).pathname === endpoint)
          await section.getByRole('button', { name: 'View daily return' }).click()
          const response = await pending
          assert.equal(response.status(), code)
          return response.json()
        }
        if (role !== 'authorized') {
          for (const edition of [expected.editions[0], expected.editions[3], expected.editions[5]]) {
            const body = await check(edition, status)
            const denied = JSON.stringify(body)
            assert(!denied.includes(edition.digest))
            if (edition.benchmark_digest) {
              assert(!denied.includes(edition.benchmark_digest))
              assert(!denied.includes('SYNTH-TR'))
            }
          }
          continue
        }
        const [original, corrected, unknown, splitOriginal, splitCorrected, indexCorrected,
          missingBenchmark, mismatchedBenchmark, splitUnknown] = expected.editions
        async function measured(edition, value, text, flowCount = '0') {
          const body = await check(edition, 200)
          assert.equal(body.source_digest, edition.digest)
          assert.equal(body.measured.return_value, value)
          assert.equal(body.external_flow_count, flowCount)
          assert.equal(body.unassessed_reason, undefined)
          await section.getByText(text, { exact: true }).waitFor()
          if (flowCount === '1') {
            const flow = body.intraday_flow
            assert.equal(flow.movement_id, edition.movement_id)
            assert.equal(flow.command_digest, edition.command_digest)
            assert.equal(flow.effective_time, '2020-02-04T12:00:00Z')
            assert.equal(flow.pre_flow_nav_reference, 'nav-pre-flow')
            assert.equal(flow.post_flow_nav_reference, 'nav-post-flow')
            const { coefficient, exponent = 0 } = flow.signed_amount
            assert.equal(Number(coefficient) * 10 ** exponent, 40)
            await section.getByText('Two linked subperiods · 40 USD external flow at 2020-02-04T12:00:00Z').waitFor()
          }
          return body
        }
        async function comparison(body, edition, benchmark, active, benchmarkText, activeText) {
          const value = body.benchmark_comparison
          assert.equal(value.source_digest, edition.benchmark_digest)
          assert.equal(value.definition.benchmark_id, 'SYNTH-TR')
          assert.equal(value.definition.currency_code, 'USD')
          assert.equal(value.start, body.start)
          assert.equal(value.end, body.end)
          assert(Date.parse(value.known_at) <= Date.parse(body.as_of))
          assert.equal(value.unassessed_reason, undefined)
          assert(Math.abs(value.measured.benchmark_return - benchmark) <= 1e-12)
          assert(Math.abs(value.measured.active_return - active) <= 1e-12)
          await section.getByText('SYNTH-TR return', { exact: false }).waitFor()
          await section.getByText(benchmarkText, { exact: true }).waitFor()
          await section.getByText(activeText, { exact: true }).waitFor()
          await section.getByText(edition.benchmark_digest, { exact: true }).waitFor()
        }
        stage = 'authorized-open-payload'
        const originalBody = await measured(original, 0.02, '2%')
        assert.equal(originalBody.benchmark_comparison, undefined)
        const updated = await measured(corrected, 0, '0%')
        assert.equal(updated.previous_source_digest, original.digest)
        const unassessed = await check(unknown, 200)
        assert.equal(unassessed.source_digest, unknown.digest)
        assert.equal(unassessed.measured, undefined)
        assert.equal(unassessed.external_flow_count, undefined)
        assert.equal(unassessed.unassessed_reason, 'flow_coverage_unknown')
        await section.getByText('Unassessed. External flow coverage is unavailable.', { exact: false }).waitFor()
        stage = 'authorized-intraday-payload'
        const splitBody = await measured(splitOriginal, 0.21, '21%', '1')
        await comparison(splitBody, splitOriginal, 2 / 25, 13 / 100, '8%', '+13 percentage points')
        const splitUpdated = await measured(splitCorrected, -0.032, '-3.2%', '1')
        assert.equal(splitUpdated.previous_source_digest, splitOriginal.digest)
        await comparison(splitUpdated, splitCorrected, 2 / 25, -14 / 125, '8%', '-11.2 percentage points')
        assert.equal(splitUpdated.benchmark_comparison.source_digest, splitBody.benchmark_comparison.source_digest)
        stage = 'authorized-benchmark-restatement'
        const revisedIndex = await measured(indexCorrected, -0.032, '-3.2%', '1')
        assert.equal(revisedIndex.previous_source_digest, splitCorrected.digest)
        assert.notEqual(revisedIndex.benchmark_comparison.source_digest, splitUpdated.benchmark_comparison.source_digest)
        await comparison(revisedIndex, indexCorrected, 1 / 10, -33 / 250, '10%', '-13.2 percentage points')
        stage = 'authorized-benchmark-coverage'
        const missingIndex = await measured(missingBenchmark, -0.032, '-3.2%', '1')
        assert.equal(missingIndex.previous_source_digest, indexCorrected.digest)
        assert.equal(missingIndex.benchmark_comparison.unassessed_reason, 'benchmark_missing')
        assert.equal(missingIndex.benchmark_comparison.measured, undefined)
        assert.equal(missingIndex.benchmark_comparison.definition, undefined)
        assert(!missingIndex.benchmark_comparison.source_digest)
        await section.getByText('Benchmark comparison unassessed. A dated benchmark observation is unavailable.').waitFor()
        const wrongWindow = await measured(mismatchedBenchmark, -0.032, '-3.2%', '1')
        assert.equal(wrongWindow.previous_source_digest, missingBenchmark.digest)
        assert.equal(wrongWindow.benchmark_comparison.source_digest, mismatchedBenchmark.benchmark_digest)
        assert.equal(wrongWindow.benchmark_comparison.end, wrongWindow.start)
        assert.equal(wrongWindow.benchmark_comparison.measured, undefined)
        assert.equal(wrongWindow.benchmark_comparison.unassessed_reason, 'benchmark_window_mismatch')
        await section.getByText('Benchmark comparison unassessed. The benchmark dates do not match this return window.').waitFor()
        const splitUnassessed = await check(splitUnknown, 200)
        assert.equal(splitUnassessed.source_digest, splitUnknown.digest)
        assert.equal(splitUnassessed.previous_source_digest, mismatchedBenchmark.digest)
        assert.equal(splitUnassessed.measured, undefined)
        assert.equal(splitUnassessed.unassessed_reason, 'nav_at_flow_missing')
        assert.equal(splitUnassessed.benchmark_comparison.measured, undefined)
        assert.equal(splitUnassessed.benchmark_comparison.unassessed_reason, 'portfolio_return_unassessed')
        await section.getByText('Unassessed. A valuation at the cash flow is unavailable.', { exact: false }).waitFor()
        stage = 'authorized-reload-details'
        await page.reload()
        assert.deepEqual(await measured(original, 0.02, '2%'), originalBody)
        await check({ as_of: '2020-02-04T06:00:00Z' }, 404)
        assert.deepEqual(await measured(splitOriginal, 0.21, '21%', '1'), splitBody)
        assert.deepEqual(await measured(splitCorrected, -0.032, '-3.2%', '1'), splitUpdated)
        await comparison(splitUpdated, splitCorrected, 2 / 25, -14 / 125, '8%', '-11.2 percentage points')
        await check({ date: splitOriginal.date, as_of: '2020-02-05T06:00:00Z' }, 404)
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
