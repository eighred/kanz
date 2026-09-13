import { afterEach, describe, expect, it, vi } from 'vitest'
import { api, ApiError } from './client'
import { describeReference, reference } from './reference'

afterEach(() => vi.restoreAllMocks())

const security = {
  instrument_id: 'BTC/USD', asset_class: 'crypto', currency_code: 'USD', description: 'Bitcoin', issuer_id: '', as_of: '',
  sector: { taxonomy: '', code: '', name: '' }, identifiers: { isin: '', cusip: '', sedol: '', figi: '', ric: '' },
  provenance: { description: 'vendor-a' },
}
const exception = { ID: 'exception-1', Kind: 'STALE_PRICE', InstrumentID: 'BTC-USD', Detail: 'stale', Status: 'OPEN', DetectedAt: '2026-01-01T00:00:00Z', Overrides: null }

describe('reference-data response boundary', () => {
  it('encodes the identifier and preserves unresolved fields', async () => {
    const get = vi.spyOn(api, 'get').mockResolvedValue(security)
    expect((await reference.security('BTC/USD')).issuer_id).toBe('')
    expect(get).toHaveBeenCalledWith('/api/v1/securities/BTC%2FUSD')
  })

  it('reads the server exception shape without converting financial fields', async () => {
    vi.spyOn(api, 'get').mockResolvedValue([exception])
    expect(await reference.exceptions()).toEqual([{ ...exception, overrideCount: 0, Overrides: undefined }])
  })

  it.each([{ ...security, as_of: 'not-a-time' }, { ...security, instrument_id: 'another' }, { ...security, provenance: { field: 1 } }])('rejects malformed security records', async (body) => {
    vi.spyOn(api, 'get').mockResolvedValue(body)
    await expect(reference.security('BTC/USD')).rejects.toThrow('Invalid reference-data response')
  })

  it('keeps unavailable and denied results distinct', () => {
    expect(describeReference(new ApiError(403, 'hidden'))).toContain('not permitted')
    expect(describeReference(new ApiError(503, 'hidden'))).toContain('unavailable')
  })
})
