import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ReferenceDataView from './ReferenceDataView.vue'
import { reference } from '../api/reference'
import { ApiError } from '../api/client'

afterEach(() => vi.restoreAllMocks())

describe('reference data', () => {
  it('loads exceptions but only looks up a security on submission', async () => {
    vi.spyOn(reference, 'exceptions').mockResolvedValue([])
    const lookup = vi.spyOn(reference, 'security').mockResolvedValue({
      instrument_id: 'BTC-USD', asset_class: 'crypto', currency_code: 'USD', description: 'Bitcoin', issuer_id: '', as_of: '',
      sector: { taxonomy: '', code: '', name: '' }, identifiers: { isin: '', cusip: '', sedol: '', figi: '', ric: '' }, provenance: {},
    })
    const wrapper = mount(ReferenceDataView)
    await flushPromises()
    expect(lookup).not.toHaveBeenCalled()
    await wrapper.find('input[name=instrument-id]').setValue('BTC-USD')
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(wrapper.text()).toContain('No source timestamp')
    expect(wrapper.text()).toContain('Issuer')
    expect(wrapper.text()).toContain('Unresolved')
  })

  it('does not turn an unavailable exception queue into a clean queue', async () => {
    vi.spyOn(reference, 'exceptions').mockRejectedValue(new ApiError(503, 'private detail'))
    const wrapper = mount(ReferenceDataView)
    await flushPromises()
    expect(wrapper.find('[role=alert]').text()).toContain('unavailable')
    expect(wrapper.text()).not.toContain('No open reference-data exceptions')
    expect(wrapper.text()).not.toContain('private detail')
  })
})
