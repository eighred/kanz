import { afterEach, describe, expect, it, vi } from 'vitest'
import { api, ApiError } from './client'
import { evidence, parseEvidencePage, parseControlEvidence, describeEvidence, downloadEvidence } from './evidence'
afterEach(() => { vi.restoreAllMocks(); vi.useRealTimers() })
function page() { return { version: 2, tenant_id: 'acme', template: 'full-log', title: 'Full audit log', generated_at: '2026-01-01T00:00:00Z', integrity: { state: 'not_requested' }, count: 1, complete: false, next_cursor: '9007199254740993', records: [{ seq: '9007199254740993', tenant_id: 'acme', event_id: 'event', occurred_at: '2026-01-01T00:00:00Z', event_type: 'test.fact', kind: 'event', source: 'test', correlation_id: 'test', attributes: { private: 'raw field must be discarded' } }] } }
function controls() { return { version: 2, tenant_id: 'acme', coverage: 'complete', assessment: 'not_assessed', from: '2026-01-01T00:00:00Z', to: '2026-01-02T00:00:00Z', total_count: 0, satisfied: false, gaps: ['CC6.1', 'CC7.2', 'CC8.1', 'PI1.1'], controls: ['CC6.1', 'CC7.2', 'CC8.1', 'PI1.1'].map(id => ({ control: { id, category: 'Security', min_per_window: 1 }, count: 0, samples: null, satisfied: false })) } }
describe('bounded evidence contract', () => {
  it('keeps exact cursors and requests only a tenant page, never verification', async () => {
    const get = vi.spyOn(api, 'get').mockResolvedValue(page())
    const result = await evidence.page('acme', 'full-log', '9007199254740992')
    expect(get).toHaveBeenCalledExactlyOnceWith('/api/v2/audit/reports/full-log?limit=100&after=9007199254740992')
    expect(result.after_cursor).toBe('9007199254740992'); expect(result.next_cursor).toBe('9007199254740993')
    expect(JSON.stringify(result)).not.toContain('raw field')
  })
  it.each([{ version: 1 }, { tenant_id: 'other' }, { count: 2 }, { next_cursor: '9007199254740994' }, { complete: true }, { records: [] }, { integrity: { state: 'verified' } }])('refuses malformed or mismatched report pages', patch => {
    expect(() => parseEvidencePage({ ...page(), ...patch }, 'acme', 'full-log')).toThrow()
  })
  it('rejects a rounded or repeated sequence', () => {
    const body = page()
    expect(() => parseEvidencePage(body, 'acme', 'full-log', body.next_cursor)).toThrow()
    expect(() => parseEvidencePage({ ...body, records: [{ ...body.records[0], seq: 9007199254740992 }] }, 'acme', 'full-log')).toThrow()
  })
  it('reads HTTP 409 evidence gaps as a complete negative evidence result', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response(JSON.stringify(controls()), { status: 409 }))
    const result = await evidence.controls('acme', '2026-01-01T00:00:00Z', '2026-01-02T00:00:00Z')
    expect(result.minimums_met).toBe(false); expect(result.assessment).toBe('not_assessed'); expect(result.gaps).toHaveLength(4)
    expect(result.controls[0]?.samples).toEqual([])
  })
  it.each([{ assessment: 'passed' }, { tenant_id: 'other' }, { coverage: 'partial' }, { satisfied: true }, { controls: [] }, { gaps: [] }])('refuses incomplete or falsely passing control evidence', patch => {
    expect(() => parseControlEvidence({ ...controls(), ...patch }, 'acme', '2026-01-01T00:00:00Z', '2026-01-02T00:00:00Z')).toThrow()
  })
  it('downloads only validated metadata with page scope and releases the object URL', async () => {
    vi.useFakeTimers(); const create = vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:test')
    const revoke = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined)
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined)
    const result = parseEvidencePage(page(), 'acme', 'full-log', '9007199254740992')
    downloadEvidence(result, 'audit-page.json')
    expect(create).toHaveBeenCalledOnce(); const payload = create.mock.calls[0]![0]; if (!(payload instanceof Blob)) throw new Error("export is not a Blob"); const exported = JSON.parse(await payload.text()); expect(exported.page_only).toBe(true); expect(exported.after_cursor).toBe('9007199254740992'); expect(JSON.stringify(exported)).not.toContain('raw field'); expect(click).toHaveBeenCalledOnce(); vi.runAllTimers(); expect(revoke).toHaveBeenCalledWith('blob:test')
  })
  it('keeps permission, disabled, oversized-window and service failures distinct without raw errors', () => {
    for (const status of [400, 403, 404, 422, 503]) expect(describeEvidence(new ApiError(status, 'private backend detail'))).not.toContain('private backend detail')
    expect(describeEvidence(new ApiError(422, 'private'))).toContain('Narrow it')
  })
})


