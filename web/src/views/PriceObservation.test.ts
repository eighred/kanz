import { afterEach, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ReferenceDataView from './ReferenceDataView.vue'
import { reference, type PriceObservation } from '../api/reference'
import { ApiError } from '../api/client'

afterEach(() => vi.restoreAllMocks())
const price: PriceObservation = { instrument_id: 'X', has_price: true, chosen: '12345678901.12345678', exceptions: 0, stale_candidates: 0, observed_at: '2026-09-13T00:00:00Z', observation_only: true }

it('loads on submission and shows an exact observation without asserting approval', async () => {
  vi.spyOn(reference, 'exceptions').mockResolvedValue([])
  let finish!: (p: PriceObservation) => void
  const get = vi.spyOn(reference, 'price').mockReturnValue(new Promise((resolve) => { finish = resolve }))
  const wrapper = mount(ReferenceDataView)
  expect(get).not.toHaveBeenCalled()
  await wrapper.find('[name=price-instrument-id]').setValue('X')
  await wrapper.find('[data-testid=price-form]').trigger('submit')
  expect(wrapper.text()).toContain('Loading price')
  expect(wrapper.find('[name=price-instrument-id]').attributes('disabled')).toBeDefined()
  finish(price)
  await flushPromises()
  expect(wrapper.text()).toContain(price.chosen)
  expect(wrapper.text()).toContain('persistence and review status are not established')
})

it.each([[new ApiError(403, 'private'), 'not permitted'], [new ApiError(503, 'private'), 'unavailable'], [new Error('private'), 'could not be verified']] as const)('shows safe refusal messages', async (error, message) => {
  vi.spyOn(reference, 'exceptions').mockResolvedValue([])
  vi.spyOn(reference, 'price').mockRejectedValue(error)
  const wrapper = mount(ReferenceDataView)
  await wrapper.find('[name=price-instrument-id]').setValue('X')
  await wrapper.find('[data-testid=price-form]').trigger('submit')
  await flushPromises()
  expect(wrapper.text()).toContain(message)
  expect(wrapper.text()).not.toContain('private')
  expect(wrapper.text()).not.toContain('Consensus price')
})

it('shows an all-stale result without presenting zero as a price', async () => {
  vi.spyOn(reference, 'exceptions').mockResolvedValue([])
  vi.spyOn(reference, 'price').mockResolvedValue({ ...price, has_price: false, chosen: undefined, stale_candidates: 2, exceptions: 2 })
  const wrapper = mount(ReferenceDataView)
  await wrapper.find('[name=price-instrument-id]').setValue('X')
  await wrapper.find('[data-testid=price-form]').trigger('submit')
  await flushPromises()
  expect(wrapper.text()).toContain('No price available')
  expect(wrapper.text()).toContain('Stale candidates excluded2')
})
