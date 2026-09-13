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

  it('requires exact data, a revision and durable action capability together', async () => {
    const get = vi.spyOn(api, 'get')
    for (const body of [
      { breaks: [item], count: 1, actions_enabled: true },
      { breaks: [{ ...item, revision: '1', values_state: 'legacy_unverified' }], count: 1, actions_enabled: true },
      { breaks: [{ ...item, status: 'open', revision: '1', values_state: 'exact' }], count: 1, actions_enabled: false },
    ]) {
      get.mockResolvedValueOnce(body)
      expect((await custody.breaks())[0]?.actions_enabled).toBe(false)
    }
    get.mockResolvedValueOnce({ breaks: [{ ...item, status: 'open', revision: '1', values_state: 'exact' }], count: 1, actions_enabled: true })
    expect((await custody.breaks())[0]?.actions_enabled).toBe(true)
  })

  it('encodes the break id and refuses mismatched action evidence', async () => {
    const row = { ...item, break_id: 'BTC/USD?x#y', status: 'open', revision: '1', values_state: 'exact' as const, actions_enabled: true }
    const post = vi.spyOn(api, 'post').mockResolvedValue({ actor: 'someone' })
    const decision = { action: 'claim' as const, request_id: 'request-1', expected_revision: '1', review_contract: 'exact-v1' as const }
    await expect(custody.act(row, decision)).rejects.toThrow('evidence could not be verified')
    expect(post).toHaveBeenCalledWith('/api/v1/custody/breaks/BTC%2FUSD%3Fx%23y/actions', decision)
  })

  it('accepts only evidence for the exact reviewed figures and state', async () => {
    const row = { ...item, status: 'open', revision: '9007199254740993', values_state: 'exact' as const, actions_enabled: true }
    const decision = { action: 'claim' as const, request_id: 'request-1', expected_revision: row.revision, review_contract: 'exact-v1' as const }
    const result = { request_id: decision.request_id, break_id: row.break_id, actor: 'verified-human', action: 'claim', recorded_at: '2026-01-02T01:00:00Z', before: { status: 'open', assignee: '', explanation: '', revision: row.revision }, after: { status: 'assigned', assignee: 'verified-human', explanation: '', revision: '9007199254740994' }, values: { ibor: row.ibor, custodian: row.custodian, difference: row.difference } }
    const post = vi.spyOn(api, 'post').mockResolvedValue(result)
    expect(await custody.act(row, decision)).toEqual(result)
    post.mockResolvedValue({ ...result, values: { ...result.values, difference: '0' } })
    await expect(custody.act(row, decision)).rejects.toThrow('evidence could not be verified')
  })
})
