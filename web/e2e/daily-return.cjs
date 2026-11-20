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
          for (const edition of [expected.editions[0], expected.editions[3], expected.editions[5], expected.editions[12], expected.editions[18], expected.editions[25]]) {
            const body = await check(edition, status)
            const denied = JSON.stringify(body)
            assert(!denied.includes(edition.digest))
            if (edition.benchmark_digest) {
              assert(!denied.includes(edition.benchmark_digest))
              assert(!denied.includes('SYNTH-TR'))
            }
            assert(!denied.includes('SYNTH-A'))
            assert(!denied.includes('position_contribution'))
            assert(!denied.includes('sector_attribution'))
            assert(!denied.includes('datamaster:'))
            for (const flow of edition.flow_legs || []) {
              assert(!denied.includes(flow.movement_id) && !denied.includes(flow.command_digest))
            }
          }
          continue
        }
        const [original, corrected, unknown, splitOriginal, splitCorrected, indexCorrected,
          missingBenchmark, mismatchedBenchmark, splitUnknown, missingMarks, badMarks, unsupported,
          traded, tradeRestated, unknownTrades, omittedTrade, wrongTradeTime, missingTradePrices,
          sectorOriginal, sectorRestated, unknownSector, missingComposition, incompleteComposition,
          benchmarkDisagreement, unsupportedSector, twoFlows, missingSecondNAV] = expected.editions
        async function measured(edition, value, text, flowCount = '0') {
          const body = await check(edition, 200)
          assert.equal(body.source_digest, edition.digest)
          assert.equal(body.measured.return_value, value)
          assert.equal(body.external_flow_count, flowCount)
          assert.equal(body.unassessed_reason, undefined)
          await section.getByText(text, { exact: true }).waitFor()
          if (flowCount === '1') {
            const flow = body.intraday_flow
            assert.deepEqual(body.intraday_flows, [])
            assert.equal(flow.movement_id, edition.movement_id)
            assert.equal(flow.command_digest, edition.command_digest)
            assert.equal(flow.effective_time, '2020-02-04T12:00:00Z')
            assert.equal(flow.pre_flow_nav_reference, edition.pre_flow_nav)
            assert.equal(flow.post_flow_nav_reference, edition.post_flow_nav)
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
        const contributionTable = () => section.getByRole('table', { name: 'Position contributions to time-weighted return' })
        async function contribution(body, reference, rows, total, tradeReference) {
          const result = body.position_contribution
          assert.equal(result.method, tradeReference
            ? 'POSITION_CONTRIBUTION_METHOD_LINKED_BOOKED_TRADES'
            : 'POSITION_CONTRIBUTION_METHOD_LINKED_UNCHANGED_HOLDINGS')
          assert.equal(result.unassessed_reason, undefined)
          assert.equal(result.opening_price_reference, 'prices-open')
          assert.equal(result.flow_price_reference, 'prices-flow')
          assert.equal(result.closing_price_reference, reference)
          const found = result.measured
          assert.deepEqual(found.positions.map(p => p.instrument_id), ['SYNTH-A', 'SYNTH-B'])
          const toNumber = ({ coefficient, exponent = 0 }) => Number(coefficient + 'e' + exponent)
          const amounts = found.positions.map(p => toNumber(p.pnl))
          const returns = found.positions.map(p => p.return_contribution)
          if (tradeReference) {
            assert.equal(result.trade_coverage_reference, tradeReference)
            amounts.push(toNumber(found.execution_costs.pnl))
            returns.push(found.execution_costs.return_contribution)
            await section.getByText(tradeReference, { exact: true }).waitFor()
          } else {
            assert.equal(found.execution_costs, undefined)
          }
          amounts.push(toNumber(found.cash_pnl), toNumber(found.total_pnl))
          returns.push(found.cash_return_contribution, found.total_return_contribution)
          const expectedRows = [...rows, total]
          assert.deepEqual(amounts, expectedRows.map(p => p[1]))
          for (const [i, [label, amount, fraction, displayed]] of expectedRows.entries()) {
            assert(Math.abs(returns[i] - fraction) <= 1e-12)
            const row = contributionTable().getByRole('row', { name: new RegExp('^' + label) })
            await row.getByRole('cell', { name: String(amount), exact: true }).waitFor()
            await row.getByRole('cell', { name: displayed, exact: true }).waitFor()
          }
          assert.equal(returns.at(-1), body.measured.return_value)
          assert(Math.abs(amounts.slice(0, -1).reduce((sum, value) => sum + value, 0) - amounts.at(-1)) <= 1e-12)
          assert(Math.abs(returns.slice(0, -1).reduce((sum, value) => sum + value, 0) - returns.at(-1)) <= 1e-12)
          await section.getByText('prices-open · prices-flow · ' + reference, { exact: true }).waitFor()
        }
        async function unassessedContribution(body, reason, text) {
          assert.equal(body.position_contribution.measured, undefined)
          assert.equal(body.position_contribution.unassessed_reason, reason)
          await section.getByText('Position contribution unassessed. ' + text, { exact: true }).waitFor()
          assert.equal(await contributionTable().count(), 0)
        }
        const sectorTable = () => section.getByRole('table', { name: 'Brinson sector attribution (percentage points)' })
        const closeTo = (a, b) => assert(Math.abs(a - b) <= 1e-12)
        async function sectorEffects(body, ref, expected, totals, displayTotals) {
          const result = body.sector_attribution
          const value = result.measured
          assert.equal(result.classification_reference, ref)
          assert.equal(result.benchmark_composition_reference, 'sector-index-members-r1')
          assert(Date.parse(result.classification_known_at) <= Date.parse(body.as_of))
          assert.equal(result.unassessed_reason, undefined)
          assert.equal(value.benchmark_id, 'SYNTH-TR')
          assert.equal(value.portfolio_id, portfolio)
          for (const field of ['start', 'end', 'as_of']) assert.equal(value[field], body[field])
          assert.deepEqual(value.sectors.map(s => s.sector), ['SYNTH:GROWTH', 'SYNTH:VALUE'])
          for (const [i, row] of value.sectors.entries()) {
            const [pw, bw, pr, br, allocation, selection, interaction, display] = expected[i]
            for (const [a, b] of [
              [row.portfolio_weight, pw], [row.benchmark_weight, bw], [row.portfolio_return, pr], [row.benchmark_return, br],
              [row.allocation, allocation], [row.selection, selection], [row.interaction, interaction],
            ]) closeTo(a, b)
            const cells = sectorTable().getByRole('row', { name: new RegExp('^' + row.sector) }).getByRole('cell')
            await cells.first().waitFor()
            assert.deepEqual(await cells.allTextContents(), display)
          }
          const actual = [value.total_allocation, value.total_selection, value.total_interaction]
          totals.forEach((t, i) => closeTo(t, actual[i]))
          closeTo(value.active_return, 17 / 200)
          closeTo(actual.reduce((a, b) => a + b, 0), value.active_return)
          const totalCells = sectorTable().getByRole('row', { name: /^Total/ }).getByRole('cell')
          assert.deepEqual(await totalCells.allTextContents(), displayTotals)
          await section.getByText(ref, { exact: true }).waitFor()
        }
        async function unassessedSector(body, reason, text) {
          assert.equal(body.sector_attribution.measured, undefined)
          assert.equal(body.sector_attribution.unassessed_reason, reason)
          await section.getByText('Sector attribution unassessed. ' + text, { exact: true }).waitFor()
          assert.equal(await sectorTable().count(), 0)
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
        stage = 'authorized-position-contribution'
        const originalRows = [['SYNTH-A', 24, 53 / 250, '+21.2'], ['SYNTH-B', 4, 9 / 500, '+1.8'],
          ['Cash and accruals', -3, -1 / 50, '-2']]
        await contribution(splitBody, 'prices-close-r1', originalRows, ['Total', 25, 21 / 100, '+21'])
        const splitUpdated = await measured(splitCorrected, -0.032, '-3.2%', '1')
        assert.equal(splitUpdated.previous_source_digest, splitOriginal.digest)
        await comparison(splitUpdated, splitCorrected, 2 / 25, -14 / 125, '8%', '-11.2 percentage points')
        assert.equal(splitUpdated.benchmark_comparison.source_digest, splitBody.benchmark_comparison.source_digest)
        const correctedRows = [['SYNTH-A', 4, 98 / 1875, '+5.2266667'], ['SYNTH-B', -9, -241 / 3750, '-6.4266667'],
          ['Cash and accruals', -3, -1 / 50, '-2']]
        await contribution(splitUpdated, 'prices-close-r2', correctedRows, ['Total', -8, -4 / 125, '-3.2'])
        stage = 'authorized-benchmark-restatement'
        const revisedIndex = await measured(indexCorrected, -0.032, '-3.2%', '1')
        assert.equal(revisedIndex.previous_source_digest, splitCorrected.digest)
        assert.notEqual(revisedIndex.benchmark_comparison.source_digest, splitUpdated.benchmark_comparison.source_digest)
        await comparison(revisedIndex, indexCorrected, 1 / 10, -33 / 250, '10%', '-13.2 percentage points')
        assert.deepEqual(revisedIndex.position_contribution, splitUpdated.position_contribution)
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
        await unassessedContribution(splitUnassessed, 'portfolio_return_unassessed', 'The portfolio return is unassessed.')
        stage = 'authorized-position-coverage'
        const missingPrices = await measured(missingMarks, -0.032, '-3.2%', '1')
        assert.equal(missingPrices.previous_source_digest, splitUnknown.digest)
        await unassessedContribution(missingPrices, 'position_marks_missing', 'A dated price is missing for one or more holdings.')
        const badPrice = await measured(badMarks, -0.032, '-3.2%', '1')
        assert.equal(badPrice.previous_source_digest, missingMarks.digest)
        await unassessedContribution(badPrice, 'position_nav_not_reconciled', 'The valued holdings and cash do not reconcile to the signed NAV.')
        const staticReport = await measured(unsupported, 163 / 750, '21.733333%', '1')
        assert.equal(staticReport.previous_source_digest, badMarks.digest)
        await unassessedContribution(staticReport, 'position_activity_unsupported', 'The holdings or journal activity require a different contribution method.')
        stage = 'authorized-booked-trades'
        const tradeBody = await measured(traded, 163 / 750, '21.733333%', '1')
        assert.equal(tradeBody.previous_source_digest, unsupported.digest)
        const tradesRows = [['SYNTH-A', 24.8, 409 / 1875, '+21.813333'],
          ['SYNTH-B', 6.2, 277 / 7500, '+3.6933333'],
          ['Execution costs', -2, -133 / 7500, '-1.7733333'], ['Cash and accruals', -3, -1 / 50, '-2']]
        await contribution(tradeBody, 'prices-close-traded-r1', tradesRows, ['Total', 26, 163 / 750, '+21.733333'], 'booked-trades-r1')
        const tradeRestatedBody = await measured(tradeRestated, 119 / 750, '15.866667%', '1')
        assert.equal(tradeRestatedBody.previous_source_digest, traded.digest)
        const tradeRestatedRows = [['SYNTH-A', 19.8, 223 / 1250, '+17.84'],
          ['SYNTH-B', 3.2, 131 / 7500, '+1.7466667'],
          ['Execution costs', -2, -43 / 2500, '-1.72'], ['Cash and accruals', -3, -1 / 50, '-2']]
        await contribution(tradeRestatedBody, 'prices-close-traded-r2', tradeRestatedRows,
          ['Total', 18, 119 / 750, '+15.866667'], 'booked-trades-r1')
        stage = 'authorized-trade-coverage'
        for (const [edition, reason, text] of [
          [unknownTrades, 'trade_coverage_missing', 'Dated fill coverage is unavailable.'],
          [omittedTrade, 'trade_coverage_mismatch', 'The signed fill list does not match the booked trades.'],
          [wrongTradeTime, 'trade_timing_unverified', 'A booked trade could not be placed in the signed return subperiod.'],
          [missingTradePrices, 'position_marks_missing', 'A dated price is missing for one or more holdings.'],
        ]) {
          await unassessedContribution(await measured(edition, 119 / 750, '15.866667%', '1'), reason, text)
        }
        stage = 'authorized-sector-effects'
        const attribution = await measured(sectorOriginal, 29 / 200, '14.5%')
        await comparison(attribution, sectorOriginal, 3 / 50, 17 / 200, '6%', '+8.5 percentage points')
        assert.match(sectorOriginal.classification_reference, /^datamaster:[0-9a-f]{64}$/)
        await sectorEffects(attribution, sectorOriginal.classification_reference, [
          [3 / 5, 1 / 2, 1 / 5, 1 / 10, 1 / 100, 1 / 20, 1 / 100, ['+1', '+5', '+1', '+7']],
          [2 / 5, 1 / 2, 1 / 16, 1 / 50, -1 / 500, 17 / 800, -17 / 4000, ['-0.2', '+2.125', '-0.425', '+1.5']],
        ], [1 / 125, 57 / 800, 23 / 4000], ['+0.8', '+7.125', '+0.575', '+8.5'])
        const revisedAttribution = await measured(sectorRestated, 29 / 200, '14.5%')
        assert.equal(revisedAttribution.previous_source_digest, sectorOriginal.digest)
        assert.equal(revisedAttribution.benchmark_comparison.source_digest, attribution.benchmark_comparison.source_digest)
        assert.notEqual(sectorOriginal.classification_reference, sectorRestated.classification_reference)
        await sectorEffects(revisedAttribution, sectorRestated.classification_reference, [
          [3 / 5, 1 / 5, 1 / 5, 1 / 10, 1 / 25, 1 / 50, 1 / 25, ['+4', '+2', '+4', '+10']],
          [2 / 5, 4 / 5, 1 / 16, 1 / 20, -1 / 50, 1 / 100, -1 / 200, ['-2', '+1', '-0.5', '-1.5']],
        ], [1 / 50, 3 / 100, 7 / 200], ['+2', '+3', '+3.5', '+8.5'])
        stage = 'authorized-sector-coverage'
        for (const [edition, reason, description] of [
          [unknownSector, 'sector_classification_missing', 'A required instrument has no dated sector classification.'],
          [missingComposition, 'sector_benchmark_coverage_missing', 'The dated benchmark constituent weights or returns are incomplete.'],
          [incompleteComposition, 'sector_benchmark_coverage_missing', 'The dated benchmark constituent weights or returns are incomplete.'],
          [benchmarkDisagreement, 'sector_benchmark_not_reconciled', 'Constituent returns do not reconcile to the signed benchmark return.'],
        ]) {
          const body = await measured(edition, 29 / 200, '14.5%')
          closeTo(body.benchmark_comparison.measured.active_return, 17 / 200)
          await unassessedSector(body, reason, description)
        }
        const intraday = await measured(unsupportedSector, 119 / 750, '15.866667%', '1')
        assert.deepEqual(intraday.position_contribution, tradeRestatedBody.position_contribution)
        await unassessedSector(intraday, 'sector_regime_unsupported', 'This method requires a fully invested day without cash flows or trades.')
        for (const edition of [twoFlows, missingSecondNAV]) {
          const complete = edition === twoFlows
          const body = complete ? await measured(edition, 331 / 1000, '33.1%', '2') : await check(edition, 200)
          assert.equal(body.source_digest, edition.digest)
          assert.equal(body.intraday_flow, undefined)
          assert.equal(body.external_flow_count, '2')
          assert.equal(body.intraday_flows.length, 2)
          assert.deepEqual(edition.flow_legs.map(flow => flow.signed_amount), ['50', '-40'])
          if (complete) {
            await section.getByText('3 linked subperiods', { exact: true }).waitFor()
          } else {
            assert.equal(body.measured, undefined)
            assert.equal(body.unassessed_reason, 'nav_at_flow_missing')
            await section.getByText('Unassessed. A valuation at the cash flow is unavailable.', { exact: false }).waitFor()
            assert.equal(await section.getByText('3 linked subperiods', { exact: true }).count(), 0)
          }
          const rows = section.getByRole('table', { name: 'Intraday external flows' }).locator('tbody tr')
          await rows.nth(1).waitFor()
          for (const [index, leg] of edition.flow_legs.entries()) {
            const wire = body.intraday_flows[index]
            for (const field of ['movement_id', 'command_digest', 'effective_time', 'pre_flow_nav_reference', 'post_flow_nav_reference']) assert.equal(wire[field], leg[field])
            assert.equal(Number(wire.signed_amount.coefficient + 'e' + wire.signed_amount.exponent), Number(leg.signed_amount))
            assert.deepEqual(await rows.nth(index).locator('th, td').allTextContents(), [
              leg.movement_id, leg.effective_time, leg.signed_amount,
              leg.pre_flow_nav_reference || 'Unavailable', leg.post_flow_nav_reference || 'Unavailable', leg.command_digest,
            ])
          }
        }
        stage = 'authorized-reload-details'
        await page.reload()
        assert.deepEqual(await measured(original, 0.02, '2%'), originalBody)
        await check({ as_of: '2020-02-04T06:00:00Z' }, 404)
        assert.deepEqual(await measured(splitOriginal, 0.21, '21%', '1'), splitBody)
        await contribution(splitBody, 'prices-close-r1', originalRows, ['Total', 25, 21 / 100, '+21'])
        assert.deepEqual(await measured(splitCorrected, -0.032, '-3.2%', '1'), splitUpdated)
        await comparison(splitUpdated, splitCorrected, 2 / 25, -14 / 125, '8%', '-11.2 percentage points')
        assert.deepEqual(await measured(traded, 163 / 750, '21.733333%', '1'), tradeBody)
        assert.deepEqual(await measured(sectorOriginal, 29 / 200, '14.5%'), attribution)
        assert.deepEqual(await measured(sectorRestated, 29 / 200, '14.5%'), revisedAttribution)
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
