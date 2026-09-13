import { afterEach, describe, expect, it, vi } from 'vitest'
import { api, ApiError } from './client'
import { audit, describeAudit } from './audit'

const event = { event_id: 'test-event', correlation_id: '', event_type: 'test.type', kind: 'domain', occurred_at: '2026-01-01T00:00:00Z', source: 'test' }
afterEach(() => vi.restoreAllMocks())

describe('audit response boundary', () => {
  it('encodes filters and projects only display metadata', async () => {
    const get = vi.spyOn(api, 'get').mockResolvedValue({ records: [{ ...event, attributes: { internal: 'omitted' }, summary: 'omitted' }], count: 1 })
    expect(await audit.events(' a&tenant=other ', ' x/y ')).toEqual([event])
    expect(get).toHaveBeenCalledWith('/api/audit/events?limit=100&correlation=a%26tenant%3Dother&event_type=x%2Fy')
  })

  it.each([[], null])('accepts the server empty representation %j', async (records) => {
    vi.spyOn(api, 'get').mockResolvedValue({ records, count: 0 })
    expect(await audit.events('', '')).toEqual([])
  })

  it.each([
    null, {}, { records: [], count: 1 }, { records: null, count: 1 },
    { records: [event], count: '1' }, { records: Array(101).fill(event), count: 101 },
    { records: [{ ...event, occurred_at: 'invalid' }], count: 1 },
    { records: [{ ...event, event_id: '' }], count: 1 },
    { records: [{ ...event, source: 123 }], count: 1 },
  ])('rejects malformed results instead of reporting an empty trail', async (body) => {
    vi.spyOn(api, 'get').mockResolvedValue(body)
    await expect(audit.events('', '')).rejects.toThrow()
  })

  it.each([401, 403, 404, 503, 500])('does not disclose upstream errors (%s)', (status) => {
    expect(describeAudit(new ApiError(status, 'private server details'))).not.toContain('private server details')
  })
})
