import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import AuditEventView from './AuditEventView.vue'
import { audit, type AuditRecord } from '../api/audit'

afterEach(() => vi.restoreAllMocks())

const record: AuditRecord = { event_id: 'event-1', correlation_id: 'correlation-1', causation_id: '', event_type: 'order.recorded', event_class: 'FACT', domain: 'orders', kind: 'domain', occurred_at: '2026-01-01T00:00:00Z', recorded_at: '2026-01-01T00:00:01Z', source: 'oms', schema_ref: '', prev_hash: '', hash: 'abc' }

async function router() {
  const value = createRouter({ history: createMemoryHistory(), routes: [
    { path: '/audit', component: { template: '<p>Search</p>' } },
    { path: '/audit/events/:id', name: 'audit-event', component: AuditEventView },
  ] })
  await value.push('/audit/events/event-1')
  return value
}

describe('audit event investigation', () => {
  it('shows verified metadata and direct ancestry without payload attributes', async () => {
    vi.spyOn(audit, 'event').mockResolvedValue(record)
    vi.spyOn(audit, 'lineage').mockResolvedValue({ target: record, ancestry: [record] })
    const wrapper = mount(AuditEventView, { global: { plugins: [await router()] } })
    await flushPromises()
    expect(wrapper.text()).toContain('order.recorded')
    expect(wrapper.text()).toContain('Direct causal ancestry')
    expect(wrapper.text()).toContain('intentionally omitted')
  })

  it('follows an ancestry link and discards a superseded detail or lineage response', async () => {
    const route = await router()
    const parent = { ...record, event_id: 'parent', event_type: 'parent.fact' }
    const other = { ...record, event_id: 'other', event_type: 'other.fact' }
    let finishDetail!: (value: AuditRecord) => void
    let finishLineage!: (value: { target: AuditRecord; ancestry: AuditRecord[] }) => void
    vi.spyOn(audit, 'event').mockResolvedValueOnce(record)
      .mockImplementationOnce(() => new Promise(resolve => { finishDetail = resolve }))
      .mockResolvedValueOnce(other).mockResolvedValueOnce(parent)
    const lineage = vi.spyOn(audit, 'lineage').mockResolvedValueOnce({ target: record, ancestry: [parent, record] })
      .mockImplementationOnce(() => new Promise(resolve => { finishLineage = resolve }))
      .mockResolvedValueOnce({ target: parent, ancestry: [parent] })
    const wrapper = mount(AuditEventView, { global: { plugins: [route] } })
    await flushPromises()
    await wrapper.get('a[href="/audit/events/parent"]').trigger('click')
    await flushPromises()
    expect(wrapper.text()).not.toContain('order.recorded')
    await route.push('/audit/events/other')
    await flushPromises()
    finishDetail(parent)
    await flushPromises()
    expect(lineage.mock.calls.map(([id]) => id)).toEqual(['event-1', 'other'])
    expect(wrapper.text()).toContain('other.fact')
    await route.push('/audit/events/parent')
    await flushPromises()
    finishLineage({ target: other, ancestry: [other] })
    await flushPromises()
    expect(wrapper.text()).toContain('parent.fact')
    expect(wrapper.text()).not.toContain('other.fact')
  })

  it('keeps a lineage failure visible without discarding the event', async () => {
    vi.spyOn(audit, 'event').mockResolvedValue(record)
    vi.spyOn(audit, 'lineage').mockRejectedValue(new Error('private detail'))
    const wrapper = mount(AuditEventView, { global: { plugins: [await router()] } })
    await flushPromises()
    expect(wrapper.text()).toContain('order.recorded')
    expect(wrapper.find('[role=alert]').text()).toContain('could not be verified')
    expect(wrapper.text()).not.toContain('private detail')
  })
})


it('shows venue evidence as an investigation rather than a completed repair', async () => {
  vi.spyOn(audit, 'event').mockResolvedValue({ ...record, kind: 'venue_discrepancy', event_type: 'accounting.balance.reconciled', discrepancy: { disposition: 'investigate', actual: '1/1000000000', scope_status: 'unverified' } })
  vi.spyOn(audit, 'lineage').mockResolvedValue({ target: record, ancestry: [record] })
  const wrapper = mount(AuditEventView, { global: { plugins: [await router()] } })
  await flushPromises()
  expect(wrapper.text()).toContain('investigation required')
  expect(wrapper.text()).toContain('1/1000000000')
  expect(wrapper.text()).toContain('not a completed repair')
})
