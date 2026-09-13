import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AuditView from './AuditView.vue'
import { audit } from '../api/audit'
import { ApiError } from '../api/client'

afterEach(() => vi.restoreAllMocks())

describe('audit search', () => {
  it('waits for a search and distinguishes a confirmed empty result', async () => {
    const search = vi.spyOn(audit, 'events').mockResolvedValue([])
    const wrapper = mount(AuditView)
    expect(search).not.toHaveBeenCalled()
    expect(wrapper.text()).not.toContain('No recorded events')
    await wrapper.find('input[name=correlation]').setValue('test-correlation')
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(search).toHaveBeenCalledWith('test-correlation', '')
    expect(wrapper.text()).toContain('No recorded events matched')
  })

  it('clears old results on a failed subsequent search', async () => {
    const search = vi.spyOn(audit, 'events').mockResolvedValue([{ event_id: 'test-event', correlation_id: '', event_type: 'test', kind: 'domain', occurred_at: '2026-01-01T00:00:00Z', source: 'test' }])
    const wrapper = mount(AuditView)
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(wrapper.text()).toContain('test-event')
    search.mockRejectedValue(new ApiError(403, 'private detail'))
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(wrapper.find('[role=alert]').text()).toContain('does not have permission')
    expect(wrapper.text()).not.toContain('test-event')
    expect(wrapper.text()).not.toContain('No recorded events')
    expect(wrapper.text()).not.toContain('private detail')
  })
})
