import { afterEach, describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { useSession } from '../stores/session'
import { evidence, type EvidencePage } from '../api/evidence'
import { ApiError } from '../api/client'
import EvidenceView from './EvidenceView.vue'
afterEach(() => vi.restoreAllMocks())
function mountView() { const pinia = createPinia(); useSession(pinia).identity = { subject: 'test:reader', tenant: 'acme', expires_at: '2099-01-01T00:00:00Z' }; return mount(EvidenceView, { global: { plugins: [pinia], stubs: { RouterLink: { template: '<a><slot /></a>' } } } }) }
function page(): EvidencePage { return { version: 2, tenant_id: 'acme', template: 'authz-decisions', title: 'Authorization decisions', generated_at: '2026-01-01T00:00:00Z', integrity: { state: 'not_requested' }, count: 1, after_cursor: '', complete: false, next_cursor: '9007199254740993', records: [{ seq: '9007199254740993', event_id: 'test-event', occurred_at: '2026-01-01T00:00:00Z', event_type: 'test.fact', kind: 'event', source: 'test', correlation_id: 'test', tenant_id: 'acme' }] } }
describe('evidence view', () => {
  it('does not fetch automatically and uses the snapshot template with its next cursor', async () => {
    const get = vi.spyOn(evidence, 'page').mockResolvedValue(page()), wrapper = mountView()
    expect(get).not.toHaveBeenCalled()
    await wrapper.findAll('form')[0]!.trigger('submit'); await flushPromises()
    expect(wrapper.text()).toContain('Partial export'); expect(wrapper.text()).toContain('Integrity not checked'); expect(wrapper.text()).toContain('9007199254740993')
    await wrapper.find('select').setValue('full-log')
    const next = wrapper.findAll('button').find(b => b.text() === 'Read next page')!
    await next.trigger('click'); await flushPromises()
    expect(get).toHaveBeenLastCalledWith('acme', 'authz-decisions', '9007199254740993')
  })
  it('clears stale rows when the next read is denied', async () => {
    const get = vi.spyOn(evidence, 'page').mockResolvedValue(page()), wrapper = mountView()
    await wrapper.findAll('form')[0]!.trigger('submit'); await flushPromises(); get.mockRejectedValueOnce(new ApiError(403, 'private error'))
    await wrapper.findAll('form')[0]!.trigger('submit'); await flushPromises()
    expect(wrapper.find('[role=alert]').text()).toContain('not permitted'); expect(wrapper.text()).not.toContain('private error'); expect(wrapper.text()).not.toContain('9007199254740993')
  })
  it('shows explicit empty pages without calling them verified', async () => {
    vi.spyOn(evidence, 'page').mockResolvedValue({ ...page(), records: [], count: 0, complete: true, next_cursor: '' })
    const wrapper = mountView(); await wrapper.findAll('form')[0]!.trigger('submit'); await flushPromises()
    expect(wrapper.text()).toContain('No matching audit records'); expect(wrapper.text()).toContain('Integrity not checked')
    expect(wrapper.findAll('button').some(b => b.text() === 'Read next page')).toBe(false)
  })
  it('renders zero evidence and gaps as unassessed, not a control failure or compliance pass', async () => {
    vi.spyOn(evidence, 'controls').mockResolvedValue({ version: 2, tenant_id: 'acme', coverage: 'complete', assessment: 'not_assessed', from: '2026-01-01T00:00:00Z', to: '2026-01-02T00:00:00Z', total_count: 0, minimums_met: false, gaps: ['CC6.1'], controls: [{ id: 'CC6.1', category: 'Security', count: 0, minimum: 1, minimum_met: false, samples: [] }] })
    const wrapper = mountView(); await wrapper.find('[name=window-start]').setValue('2026-01-01T00:00:00Z'); await wrapper.findAll('form')[1]!.trigger('submit'); await flushPromises()
    expect(wrapper.text()).toContain('Control effectiveness is not assessed'); expect(wrapper.text()).toContain('Insufficient records'); expect(wrapper.text()).toContain('No sample events')
  })
})
