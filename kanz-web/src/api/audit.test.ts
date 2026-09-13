import { afterEach, describe, expect, it, vi } from 'vitest'
import { api, ApiError } from './client'
import { audit, describeAudit } from './audit'

const event = { event_id: 'test-event', correlation_id: '', event_type: 'test.type', kind: 'domain', occurred_at: '2026-01-01T00:00:00Z', source: 'test' }
const record = { ...event, causation_id: '', domain: 'orders', event_class: 'FACT', recorded_at: '2026-01-01T00:00:01Z', schema_ref: '', prev_hash: '', hash: 'abc' }
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

  it('reads a tenant-scoped event and its direct ancestry with encoded ids', async () => {
    const slashRecord = { ...record, event_id: 'test/event' }
    const get = vi.spyOn(api, 'get')
      .mockResolvedValueOnce({ ...slashRecord, attributes: { hidden: 'value' } })
      .mockResolvedValueOnce({ target: record, ancestry: [record], tree: { record } })
    expect(await audit.event('test/event')).toEqual(slashRecord)
    expect((await audit.lineage('test-event')).ancestry).toEqual([record])
    expect(get.mock.calls[0]?.[0]).toBe('/api/v1/audit/events/test%2Fevent')
  })

  it('rejects lineage that does not terminate at the requested event', async () => {
    vi.spyOn(api, 'get').mockResolvedValue({ target: record, ancestry: [{ ...record, event_id: 'other' }] })
    await expect(audit.lineage('test-event')).rejects.toThrow('Invalid audit lineage')
  })
})
