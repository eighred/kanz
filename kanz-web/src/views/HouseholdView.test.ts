import { afterEach, describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import HouseholdView from './HouseholdView.vue'
import { households, type Household } from '../api/households'
import { ApiError } from '../api/client'
afterEach(() => vi.restoreAllMocks())
function valuation(): Household {
  return { arithmetic_version: 2, household_id: 'household', total_value: '9007199254740993.000000001', cash: '0', holdings: { X: '9007199254740993.000000001' }, weights: { X: '1/3' }, asset_class: { EQUITY: '1/3' }, weights_state: 'exact', currency_code: 'USD', as_of: '2020-01-01T00:00:00Z', recorded_by: 'test:source', source_reason: 'Test statement', risk_profile: 'GROWTH', drift: { evaluated: false, outcome: 'invalid_model' } }
}
describe('household valuation view', () => {
  it('looks up on submission, clears prior values during loading and preserves precision', async () => {
    const get = vi.spyOn(households, 'get').mockResolvedValue(valuation()), wrapper = mount(HouseholdView)
    expect(get).not.toHaveBeenCalled()
    await wrapper.find('input').setValue('household'); await wrapper.find('form').trigger('submit'); await flushPromises()
    expect(wrapper.text()).toContain('9007199254740993.000000001')
    expect(wrapper.text()).toContain('1/3'); expect(wrapper.text()).toContain('Freshness has not been established')
    expect(wrapper.text()).toContain('2020-01-01'); expect(wrapper.text()).toContain('unverified legacy precision')
    expect(wrapper.text()).not.toContain('Maximum absolute drift')
    let finish!: (value: Household) => void
    get.mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
    await wrapper.find('form').trigger('submit')
    expect(wrapper.text()).toContain('Loading valuation'); expect(wrapper.text()).not.toContain('9007199254740993')
    finish(valuation()); await flushPromises()
  })
  it.each([403, 404, 503, 500])('shows safe refusal for %s and removes stale numbers', async status => {
    const get = vi.spyOn(households, 'get').mockResolvedValue(valuation()), wrapper = mount(HouseholdView)
    await wrapper.find('input').setValue('household'); await wrapper.find('form').trigger('submit'); await flushPromises()
    get.mockRejectedValueOnce(new ApiError(status, 'private backend body'))
    await wrapper.find('form').trigger('submit'); await flushPromises()
    expect(wrapper.find('[role=alert]').exists()).toBe(true)
    expect(wrapper.text()).not.toContain('private backend body'); expect(wrapper.text()).not.toContain('9007199254740993')
  })
  it('shows empty holdings, exact zero, and unavailable allocation separately', async () => {
    vi.spyOn(households, 'get').mockResolvedValue({ ...valuation(), total_value: '0', holdings: {}, weights: null, asset_class: null, weights_state: 'unavailable' })
    const wrapper = mount(HouseholdView)
    await wrapper.find('input').setValue('household'); await wrapper.find('form').trigger('submit'); await flushPromises()
    expect(wrapper.text()).toContain('No holdings are recorded'); expect(wrapper.text()).toContain('Allocation weights are unavailable')
    expect(wrapper.find('dd').text()).toBe('0')
  })
})

it('renders an evaluated exact band and instrument deviation', async () => {
  const body = valuation(); body.drift = { evaluated: true, outcome: 'breached', model_id: 'test-model', tolerance: '0.05', max: '0.050000000000000000001', total: '0.100000000000000000002', by_instrument: { X: '0.050000000000000000001' }, breached: true }
  vi.spyOn(households, 'get').mockResolvedValue(body)
  const wrapper = mount(HouseholdView)
  await wrapper.find('input').setValue('household'); await wrapper.find('form').trigger('submit'); await flushPromises()
  expect(wrapper.text()).toContain('Published model band breached'); expect(wrapper.text()).toContain('0.050000000000000000001')
  expect(wrapper.text()).toContain('test-model'); expect(wrapper.findAll('button')).toHaveLength(1)
})
