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
