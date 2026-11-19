import { afterEach, expect, it, vi } from 'vitest'
import sample from '../../../kanz/services/optimization/internal/server/testdata/model-proposal-review.json'
import { reviewModelProposal, type Scenario } from './model-proposal'

const form = (): Scenario => ({ portfolio: 'PF', currency: 'USD', nav: '9007199254740993', threshold: '0',
  assets: [{ instrument: 'A', current: '0', price: '0.000000000001', expectedReturn: '0.1' }] })
const signal = () => new AbortController().signal
function serve(value: unknown, status = 200) {
  const fetch = vi.fn().mockResolvedValue(Response.json(value, { status }))
  vi.stubGlobal('fetch', fetch)
  return fetch
}
afterEach(() => vi.unstubAllGlobals())

it('posts an exact hypothetical book to the sole read-only v2 route and reads the real producer sample', async () => {
  const fetch = serve(sample), scoped = signal()
  const result = await reviewModelProposal(form(), scoped)
  expect(result.trades).toEqual([{ id: 'A', side: 'Buy', notional: '9007199254740993', quantity: '9007199254740993000000000000' }])
  expect(result.verdict).toBe('UNCHECKED')
  expect(result.turnover).toBe('0.5')
  expect(fetch).toHaveBeenCalledOnce()
  const [url, options] = fetch.mock.calls[0]!
  expect(url).toBe('/api/v2/model-portfolios/propose')
  expect(options.method).toBe('POST')
  expect(options.signal).toBe(scoped)
  expect(JSON.parse(options.body)).toEqual({ portfolio_id: 'PF', currency: 'USD', nav: '9007199254740993', threshold: '0',
    instruments: ['A'], current_weights: { A: '0' }, prices: { A: '0.000000000001' }, expected_returns: [0.1],
    objective: { Type: 0, RiskAversion: 0, RiskFreeRate: 0 } })
})

it('keeps evaluated, threshold-retained holdings separate from solver targets, with rational quantities and refusals', async () => {
  const scenario: Scenario = { portfolio: 'PF', currency: 'USD', nav: '1', threshold: '1/2', assets: [
    { instrument: 'A', price: '3', current: '1/2', expectedReturn: '0.1' },
    { instrument: 'B', price: '3', current: '1/2', expectedReturn: '0.2' },
  ] }
  serve({ ...sample, Targets: { A: '0', B: '1' }, EvaluatedWeights: { A: '0.5', B: '1/2' }, Trades: null,
    Turnover: '0', ExpectedReturn: 0.15, MandateStatus: 'INFEASIBLE', Violations: ['Concentration limit exceeded'] })
  const retained = await reviewModelProposal(scenario, signal())
  expect(retained.book[0]).toEqual({ id: 'A', current: '1/2', target: '0', candidate: '0.5' })
  expect(retained.trades).toEqual([])
  expect(retained.violations).toEqual(['Concentration limit exceeded'])
  scenario.threshold = '0'
  serve({ ...sample, Targets: { A: '0', B: '1' }, EvaluatedWeights: { A: '0', B: '1' }, MandateStatus: 'FEASIBLE',
    Trades: [{ InstrumentID: 'A', CurrentWeight: '0.5', TargetWeight: '0', Side: 2, Notional: '1/2', Quantity: '1/6' },
      { InstrumentID: 'B', CurrentWeight: '1/2', TargetWeight: '1', Side: 1, Notional: '0.5', Quantity: '1/6' }] })
  const candidate = await reviewModelProposal(scenario, signal())
  expect(candidate.verdict).toBe('FEASIBLE')
  expect(candidate.trades.map(trade => [trade.side, trade.quantity])).toEqual([['Sell', '1/6'], ['Buy', '1/6']])
})

it.each([
  { PortfolioID: 'other' }, { Currency: 'EUR' }, { SchemaVersion: 1 }, { ReadOnly: false },
  { AsOf: '2026-02-30T00:00:00Z' }, { Objective: { ...sample.Objective, Type: 1 } }, { ExpectedReturn: '0.1' },
  { ExpectedRisk: 0 }, { CovarianceQuality: 'OBSERVED' }, { Targets: {} }, { Targets: { A: '0' }, EvaluatedWeights: { A: '0' }, Trades: null, Turnover: '0' }, { Targets: { A: '1', B: '0' } },
  { EvaluatedWeights: { A: '1/0' } }, { Targets: { A: 1 } }, { MandateStatus: 'APPROVED' }, { Violations: ['refused'] },
  { Turnover: null }, { Turnover: '0.5000000000000001' }, { Trades: null }, { Trades: [sample.Trades[0], sample.Trades[0]] },
  ...[{ InstrumentID: 'B' }, { Quantity: '9007199254740993000000000001' }, { Notional: 9007199254740992 },
    { CurrentWeight: '1' }, { Side: 2 }, { TargetWeight: '1/3' }].map(change => ({ Trades: [{ ...sample.Trades[0], ...change }] })),
])('refuses an unconfirmed candidate %#', async change => {
  serve({ ...sample, ...change })
  await expect(reviewModelProposal(form(), signal())).rejects.toThrow()
})

it('rejects ambiguous instruments and missing/invalid exact inputs before sending anything', async () => {
  const fetch = serve(sample)
  for (const change of [{ nav: '' }, { nav: '1e9' }, { threshold: '2' }, { currency: 'usd' },
    { assets: [form().assets[0]!, { ...form().assets[0]!, instrument: 'a' }] },
    { assets: [{ ...form().assets[0]!, current: '1/3' }] }, { assets: [{ ...form().assets[0]!, expectedReturn: '' }] }, { assets: [{ ...form().assets[0]!, expectedReturn: '1e-999' }] }]) {
    await expect(reviewModelProposal({ ...form(), ...change }, signal())).rejects.toThrow()
  }
  expect(fetch).not.toHaveBeenCalled()
})

it('requires HTTP 200 and never retries a refused decision', async () => {
  const fetch = serve(sample, 202)
  await expect(reviewModelProposal(form(), signal())).rejects.toThrow('Unexpected success status')
  expect(fetch).toHaveBeenCalledOnce()
})
