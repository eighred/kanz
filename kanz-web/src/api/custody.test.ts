import { afterEach, describe, expect, it, vi } from 'vitest'
import { api, ApiError } from './client'
import { custody, describeCustody } from './custody'

afterEach(() => vi.restoreAllMocks())

const item = { break_id: 'break-1', kind: 'quantity', key: 'BTC-USD', ibor: '1.00000001', custodian: '1', difference: '0.00000001', status: 'OPEN', first_seen_at: '2026-01-01T00:00:00Z', last_seen_at: '2026-01-02T00:00:00Z', status_changed_at: '2026-01-01T00:00:00Z', age_seconds: 86400 }

describe('custody response boundary', () => {
  it('preserves exact decimal text', async () => {
    vi.spyOn(api, 'get').mockResolvedValue({ breaks: [item], count: 1 })
    expect((await custody.breaks())[0]?.difference).toBe('0.00000001')
  })

  it.each([{ breaks: [], count: 1 }, { breaks: [{ ...item, difference: 0.1 }], count: 1 }, { breaks: [{ ...item, age_seconds: -1 }], count: 1 }])('rejects an invalid queue rather than reporting success', async (body) => {
    vi.spyOn(api, 'get').mockResolvedValue(body)
    await expect(custody.breaks()).rejects.toThrow('Invalid custody response')
  })

  it('distinguishes an unavailable control from an empty queue', () => {
    expect(describeCustody(new ApiError(404, 'hidden'))).toContain('not enabled')
    expect(describeCustody(new ApiError(403, 'hidden'))).toContain('not permitted')
  })
})
