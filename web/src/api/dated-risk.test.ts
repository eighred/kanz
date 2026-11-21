import { afterEach, describe, expect, it, vi } from 'vitest'
import { readDatedRisk, reportedLoss } from './dated-risk'

const digest = 'a'.repeat(64)
const source = { base_currency: 'USDT', code_revision: 'original', input_manifest_digest: digest }
const historical = {
  name: 'VaR99', value: { coefficient: '900719925474099301', exponent: -2 },
  coverage: { contributed: 2, excluded_count: 0 },
  provenance: { method: 'historical_simulation', input_manifest_digest: digest },
}
const id = 'book/1'
const cutoff = '2026-01-02T03:04:05.000000123Z'
const signal = new AbortController().signal
const set = { portfolio_id: id, as_of: cutoff, measures: [historical] }
const response = { portfolio_id: id, as_of: cutoff, set, retained_evaluation: source }

afterEach(() => vi.unstubAllGlobals())

describe('dated portfolio loss report', () => {
  it('preserves the original amount, denomination and a measured zero', () => {
    expect(reportedLoss(historical, source)).toBe('9007199254740993.01 USDT')
    expect(reportedLoss({ ...historical, value: { coefficient: '0' } }, source)).toBe('0 USDT')
  })

  it('withholds unsupported or unmeasured amounts', () => {
    expect(reportedLoss(undefined, source)).toBe('Measure unavailable')
    expect(reportedLoss(historical)).toBe('Historical currency unavailable')
    expect(reportedLoss(historical, { ...source, input_manifest_digest: 'b'.repeat(64) })).toBe('Unverified measure origin')
    expect(reportedLoss({ ...historical, coverage: undefined }, source)).toBe('Coverage unknown')
    expect(reportedLoss({ ...historical, coverage: { contributed: 0, excluded_count: 2 } }, source)).toBe('No contributing positions')
    expect(reportedLoss({ ...historical, provenance: { ...historical.provenance, method: 'placeholder_1pct_gross' } }, source)).toBe('Placeholder model')
    expect(reportedLoss({ ...historical, coverage: { ...historical.coverage, excluded_count: 1,
      exclusions: [{ instrument_id: '', reason: 'insufficient_history' }] } }, source)).toBe('Evaluation unavailable')
  })

  it('requests both measures at the exact cutoff, retaining all wire evidence', async () => {
    const fetch = vi.fn().mockResolvedValue(Response.json(response))
    vi.stubGlobal('fetch', fetch)
    expect(await readDatedRisk(id, cutoff, signal)).toEqual(response)
    const url = new URL(fetch.mock.calls[0]![0], 'http://localhost')
    expect(url.pathname).toBe('/api/v1/portfolios/book%2F1/measures')
    expect(url.searchParams.get('as_of')).toBe(cutoff)
    expect(url.searchParams.getAll('measure')).toEqual(['VaR99', 'ES99'])
  })

  it.each(['', '2026-02-30T00:00:00Z', '0000-01-01T00:00:00Z', 'yesterday'])('refuses invalid cutoff %s before requesting data', async value => {
    const fetch = vi.fn()
    vi.stubGlobal('fetch', fetch)
    await expect(readDatedRisk(id, value, signal)).rejects.toThrow('UTC timestamp')
    expect(fetch).not.toHaveBeenCalled()
  })

  it.each([
    { ...response, portfolio_id: 'other' },
    { ...response, set: { ...set, portfolio_id: 'other' } },
    { ...response, as_of: cutoff.replace('123Z', '124Z'), set: { ...set, as_of: cutoff.replace('123Z', '124Z') } },
    { ...response, set: { ...set, as_of: cutoff.replace('123Z', '122Z') } },
    { ...response, set: { ...set, measures: [historical, historical] } },
  ])('rejects mismatched or ambiguous evaluations', async body => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(Response.json(body)))
    await expect(readDatedRisk(id, cutoff, signal)).rejects.toThrow()
  })
})
