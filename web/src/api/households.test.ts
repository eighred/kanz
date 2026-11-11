import { afterEach, describe, expect, it, vi } from 'vitest'
import { api, ApiError } from './client'
import { households, parseHousehold, describeHousehold, type Household } from './households'
afterEach(() => vi.restoreAllMocks())
export function valuation(): Household {
  return { arithmetic_version: 2, household_id: 'house/one?#%', total_value: '9007199254740993.000000003', cash: '0', holdings: { X: '9007199254740993.000000003' }, weights: { X: '1' }, asset_class: { EQUITY: '1' }, weights_state: 'exact', currency_code: 'USD', as_of: '2020-01-01T00:00:00Z', recorded_by: 'test:source', source_reason: 'Test statement', risk_profile: 'GROWTH', drift: { evaluated: false, outcome: 'catalogue_unarmed' } }
}
describe('exact household boundary', () => {
  it('uses only the versioned route and preserves exact text', async () => {
    const body = valuation(), get = vi.spyOn(api, 'get').mockResolvedValue(body)
    expect((await households.get(body.household_id)).total_value).toBe('9007199254740993.000000003')
    expect(get).toHaveBeenCalledExactlyOnceWith('/api/v2/households/house%2Fone%3F%23%25')
  })
  it('preserves recurring fractions and exact drift band comparisons', () => {
    const body = valuation(); body.weights = { X: '1/3' }; body.drift = { evaluated: true, outcome: 'breached', model_id: 'model', tolerance: '0.05', max: '0.050000000000000000001', total: '0.100000000000000000002', by_instrument: { X: '0.050000000000000000001' }, breached: true }
    expect(parseHousehold(body, body.household_id).weights?.X).toBe('1/3')
    body.drift.breached = false
    expect(() => parseHousehold(body, body.household_id)).toThrow()
  })
  it.each([
    { total_value: 9007199254740992 }, { arithmetic_version: 1 }, { cash: '' }, { cash: 'NaN' }, { total_value: '1e99999999' }, { total_value: '1/0' }, { household_id: 'other' }, { as_of: 'bad-time' }, { currency_code: '' }, { total_value: '0' }, { holdings: { X: 1 } }, { drift: { evaluated: false, outcome: 'in_band' } }, { weights_state: 'unavailable' },
  ])('refuses malformed or legacy facts without fallback', patch => {
    const body = valuation()
    expect(() => parseHousehold({ ...body, ...patch }, body.household_id)).toThrow()
  })
  it('distinguishes a measured zero from unavailable weights', () => {
    const body = { ...valuation(), total_value: '0', cash: '0', holdings: {}, weights_state: 'unavailable', weights: null, asset_class: null }
    const result = parseHousehold(body, body.household_id)
    expect(result.total_value).toBe('0'); expect(result.weights).toBeNull(); expect(result.drift.evaluated).toBe(false)
  })
  it('never returns raw backend errors to the view', () => {
    expect(describeHousehold(new ApiError(403, 'private detail'))).toContain('not permitted')
    expect(describeHousehold(new ApiError(404, 'private detail'))).toContain('not found')
    expect(describeHousehold(new ApiError(503, 'private detail'))).toContain('unavailable')
    expect(describeHousehold(new Error('private detail'))).not.toContain('private detail')
  })
})

it('carries a safe refusal code through the real API client and distinguishes legacy from outage', async () => {
  vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response(JSON.stringify({ error: 'private server detail', code: 'legacy_unverified' }), { status: 503 }))
  try { await households.get('legacy'); throw new Error('expected refusal') }
  catch (error) { expect(describeHousehold(error)).toContain('unverified legacy valuation'); expect(describeHousehold(error)).not.toContain('private server detail') }
  expect(describeHousehold(new ApiError(503, 'private server detail'))).toContain('currently unavailable')
})
