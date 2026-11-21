import { mount, flushPromises } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { afterEach, expect, it, vi } from 'vitest'
import { api } from '../api/client'
import { isDepthInput, spotRequest } from '../api/spot_depth'
import example from '../api/spot_depth_example.json'
import { useSession } from '../stores/session'
import SpotDepthView from './SpotDepthView.vue'

afterEach(() => vi.restoreAllMocks())

it('clears private assumptions on recapture and route changes and projects each portfolio binding', async () => {
  if (!isDepthInput(example)) throw new Error('Invalid fixture')
  const books = spotRequest(example).spot_depth.venues.reverse()
  const post = vi.spyOn(api, 'post').mockImplementation(async (path) => {
    const portfolio = path.split('/')[4]
    return {
      portfolio_id: portfolio, owner_tenant: 'pilot',
      spot_books: {
        status: 'SUPPORTED', as_of: example.cutoff, instrument_id: example.instrument,
        product_type: 'CRYPTO_SPOT', source: 'demo', producer_version: 'v1', rights_reference: 'demo-only',
        base_asset_ref: 'bitcoin', quote_asset_ref: 'tether',
        venues: books.map((row, i) => ({
          book: row.book, known_at: row.known_at, event_id: 'event-' + i,
          portfolio_account_ref: i === 0 ? 'binance-' + portfolio : '',
        })),
      },
    }
  })
  const pinia = createPinia()
  useSession(pinia).identity = { tenant: 'pilot', subject: 'operator', expires_at: '2030-01-01T00:00:00Z' }
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/portfolios/:id/spot-depth', component: SpotDepthView },
      { path: '/portfolios/:id/exposure', name: 'exposure', component: { template: '<div />' } },
    ],
  })
  await router.push('/portfolios/alpha/spot-depth')
  const view = mount(SpotDepthView, { global: { plugins: [router, pinia] } })
  const capture = async () => {
    await view.get('button').trigger('click')
    await flushPromises()
  }
  const field = (venue: number, label: string) => {
    const group = view.findAll('fieldset')[venue]!
    return group.findAll('label').find((item) => item.text() === label)!.get('input')
  }
  try {
    await capture()
    expect(post.mock.calls[0]?.[1]).toHaveProperty('spot_book_capture_max_age_ms', 250)
    expect(view.get('[role="alert"]').text()).toContain('OKX book exceeds')
    await view.get('input[type=number]').setValue(500)
    await capture()
    expect(post.mock.calls[1]?.[1]).not.toHaveProperty('wait_for_spot_books')
    expect(view.findAll('fieldset')[0]!.text()).toContain('Book age at capture 250 ms')
    expect(view.findAll('fieldset')[1]!.text()).toContain('Venue event to risk receipt 150 ms')
    expect(view.findAll('fieldset')[1]!.text()).toContain('Risk receipt to capture 350 ms')
    expect(field(0, 'Portfolio account').element.value).toBe('binance-alpha')
    expect(field(0, 'Portfolio account').element.readOnly).toBe(true)
    await field(0, 'Base available').setValue('7.5')
    await field(1, 'Scenario account').setValue('hypothetical')
    await field(1, 'Buy taker fee (bps)').setValue('2')
    expect(field(1, 'Sell taker fee (bps)').element.value).toBe('')
    await view.get('input[type=checkbox]').setValue(true)
    await capture()
    expect(post.mock.calls[2]?.[1]).toMatchObject({wait_for_spot_books: true, spot_book_capture_max_age_ms: 500})
    expect(field(0, 'Base available').element.value).toBe('')
    expect(field(1, 'Buy taker fee (bps)').element.value).toBe('')
    expect(field(1, 'Scenario account').element.value).toBe('')
    await router.push('/portfolios/beta/spot-depth')
    expect(view.find('fieldset').exists()).toBe(false)
    await capture()
    expect(field(0, 'Portfolio account').element.value).toBe('binance-beta')
    const funds = {
      owner_tenant: 'pilot', mic: 'BINANCE', venue_account_id: 'binance-beta',
      instrument_id: 'BTC-USDT', venue_symbol: 'BTCUSDT', base_currency: 'BTC', quote_currency: 'USDT',
      request_started_at: '2026-11-21T08:00:00.100Z', received_at: example.cutoff,
      available_base: { coefficient: '0', exponent: 0 },
      available_quote: { coefficient: '900719925474099301', exponent: -2 },
      taker_schedule: {
        request_started_at: example.cutoff, received_at: example.cutoff,
        buy: { bps: { coefficient: '121', exponent: -1 }, currency: 'BTC' },
        sell: { bps: { coefficient: '114', exponent: -1 }, currency: 'USDT' },
      },
    }
    const get = vi.spyOn(api, 'get')
    const fullCapture = async (observation: typeof funds) => {
      get.mockResolvedValueOnce({ portfolio_id: 'beta', owner_tenant: 'pilot', market_venue: 'BINANCE', funds: observation })
        .mockRejectedValueOnce(new Error('OKX demo read unavailable'))
      await view.findAll('button').find((b) => b.text() === 'Capture books, demo balances and fees')!.trigger('click')
      await flushPromises()
    }
    await fullCapture(funds)
    expect(field(0, 'Quote available').element.value).toBe('9007199254740993.01')
    expect(field(0, 'Base available').element.value).toBe('0')
    expect(field(0, 'Base available').element.readOnly).toBe(true)
    expect(field(0, 'Buy taker fee (bps)').element.value).toBe('12.1')
    expect(field(0, 'Sell taker fee (bps)').element.value).toBe('11.4')
    expect(field(0, 'Buy fee currency').element.readOnly).toBe(true)
    expect(get.mock.calls[0]?.[0]).toContain('taker_fees=true')
    expect(field(1, 'Base available').element.value).toBe('')
    await fullCapture({ ...funds, taker_schedule: { ...funds.taker_schedule, received_at: '2026-11-21T08:00:00.500000001Z' } })
    expect(field(0, 'Base available').element.readOnly).toBe(true)
    expect(field(0, 'Buy taker fee (bps)').element.value).toBe('')
    for (const change of [
      { received_at: '2026-11-21T08:00:00.500000001Z' },
      { request_started_at: '2026-11-21T07:59:50Z' },
      { venue_account_id: 'binance-alpha' }, { owner_tenant: 'another' }, { quote_currency: 'USD' },
    ]) {
      await fullCapture({ ...funds, ...change })
      expect(field(0, 'Base available').element.value).toBe('')
      expect(field(0, 'Base available').element.readOnly).toBe(false)
    }
    // Tokyo has an OMS account master but no risk-side margin bindings.
    get.mockRejectedValueOnce(new Error('Binance unavailable')).mockResolvedValueOnce({
      owner_tenant: 'pilot', portfolio_id: 'beta', market_venue: 'OKX',
      funds: { ...funds, mic: 'XOKX', venue_account_id: 'okx-sub-1', venue_symbol: 'BTC-USDT' },
    })
    await view.findAll('button').find((b) => b.text() === 'Capture books, demo balances and fees')!.trigger('click')
    await flushPromises()
    expect(field(1, 'Portfolio account').element.value).toBe('okx-sub-1')
    expect(field(1, 'Quote available').element.readOnly).toBe(true)
    expect(field(1, 'Sell taker fee (bps)').element.value).toBe('')
    await view.findAll('button').find((b) => b.text() === 'Use group fees as scenario assumptions')!.trigger('click')
    expect(field(1, 'Buy taker fee (bps)').element.value).toBe('12.1')
    expect(field(1, 'Sell fee currency').element.value).toBe('USDT')
    expect(field(1, 'Sell fee currency').element.readOnly).toBe(false)
    const session = useSession(pinia)
    session.forget()
    await flushPromises()
    expect(view.find('fieldset').exists()).toBe(false)
    expect(post).toHaveBeenCalledTimes(12)
  } finally {
    view.unmount()
  }
})
