import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, expect, it, vi } from 'vitest'
import { api } from '../api/client'
import { isDepthInput, type DepthInput } from '../api/spot_depth'
import example from '../api/spot_depth_example.json'
import SpotTermsComparison from './SpotTermsComparison.vue'

afterEach(() => vi.restoreAllMocks())

it('keeps the original terms and rejects a completed request after the assumptions change', async () => {
  const data = structuredClone(example)
  if (!isDepthInput(data)) throw new Error('Invalid fixture')
  const inputs: DepthInput = data
  const v = inputs.venues[0]!
  v.fundsObservation = {
    source: 'OKX Demo spot cash capacity', request_started_at: v.observed, received_at: v.received,
    available_base: v.baseBalance, available_quote: v.quoteBalance,
  }
  v.feeObservation = {
    source: 'OKX account fee group', request_started_at: v.observed, received_at: v.received,
    buyBps: v.feeBps, buyCurrency: v.feeCurrency, sellBps: v.feeBps, sellCurrency: v.feeCurrency,
  }
  const baseline = {
    instrument_id: inputs.instrument, base_currency: 'BTC', quote_currency: 'USDT', as_of: inputs.cutoff,
    input_sha256: '0'.repeat(64), directions: inputs.venues.map((venue, i) => ({
      buy_venue: venue.venue, sell_venue: inputs.venues[1-i]!.venue, status: 'UNKNOWN', reason: 'Fee unavailable',
    })),
  }
  const post = vi.spyOn(api, 'post')
  const view = mount(SpotTermsComparison, { props: { portfolio: 'PF1', tenant: 'pilot', inputs, baseline } })
  const button = (label: string) => view.findAll('button').find(b => b.text() === label)!
  try {
    await button('Compare alternative fees and balances').trigger('click')
    await button('Set alternative fees to zero').trigger('click')
    expect(v.feeBps).toBe('8')
    let finish!: (value: unknown) => void
    post.mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
    await view.get('form').trigger('submit')
    expect(post.mock.calls[0]?.[1]).not.toHaveProperty('spot_book_check')
    const amount = view.findAll('input')[0]!
    await amount.setValue('0.5')
    const reply = { owner_tenant: 'pilot', portfolio_id: 'PF1', spot_depth: { ...baseline, input_sha256: '1'.repeat(64) } }
    finish(reply)
    await flushPromises()
    expect(view.find('table').exists()).toBe(false)
    post.mockResolvedValueOnce(reply)
    await view.get('form').trigger('submit')
    await flushPromises()
    await button('Save both scenarios').trigger('click')
    const saved = view.emitted<{ inputs: DepthInput }[]>('save')![0]![0]!.inputs
    expect(saved.venues[0]).toMatchObject({ feeBps: '0', sellFeeBps: '0', baseBalance: '0.5', bids: v.bids, sequence: v.sequence })
    expect(saved.venues[0]).not.toHaveProperty('fundsObservation')
    expect(saved.venues[0]).not.toHaveProperty('feeObservation')
    expect(v.fundsObservation.available_base).toBe('2')
    await view.setProps({ portfolio: 'PF2' })
    expect(view.find('form').exists()).toBe(false)
  } finally {
    view.unmount()
  }
})
