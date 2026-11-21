import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, expect, it, vi } from 'vitest'
import { api, ApiError } from '../api/client'
import DemoSpotAccounts from './DemoSpotAccounts.vue'

const props = { portfolio: 'PF1', instrument: 'BTC-USDT', identity: { tenant: 'pilot', subject: 'reader', expires_at: '2030-01-01T00:00:00Z' } }
const stamp = '2026-11-21T08:00:00Z'
function response(venue: 'BINANCE' | 'OKX') {
  return {
    owner_tenant: 'pilot', portfolio_id: 'PF1', market_venue: venue,
    funds: {
      owner_tenant: 'pilot', venue_account_id: venue + '-account', mic: venue === 'OKX' ? 'XOKX' : 'XBIN',
      instrument_id: 'BTC-USDT', venue_symbol: venue === 'OKX' ? 'BTC-USDT' : 'BTCUSDT',
      base_currency: 'BTC', quote_currency: 'USDT', available_base: { coefficient: '0', exponent: 0 },
      available_quote: { coefficient: '900719925474099319', exponent: -2 }, request_started_at: stamp, received_at: stamp,
      taker_schedule: { request_started_at: stamp, received_at: stamp,
        buy: { bps: { coefficient: '0', exponent: 0 }, currency: 'BTC' },
        sell: { bps: { coefficient: '1', exponent: -1 }, currency: 'USDT' } },
    },
  }
}
afterEach(() => vi.restoreAllMocks())

it('shows each dated venue result independently and forgets late results when the portfolio changes', async () => {
  let finish!: (result: unknown) => void
  const pending = new Promise(resolve => { finish = resolve })
  const get = vi.spyOn(api, 'get').mockResolvedValueOnce(response('BINANCE')).mockReturnValueOnce(pending)
  const post = vi.spyOn(api, 'post')
  const view = mount(DemoSpotAccounts, { props })
  try {
    await view.get('button').trigger('click')
    await flushPromises()
    const binance = view.get('[aria-label="BINANCE demo account"]')
    expect(binance.text()).toContain('BINANCE-account')
    expect(binance.findAll('dd').map(v => v.text())).toEqual(['0', '9007199254740993.19'])
    expect(binance.text()).toContain('Buy 0 bps in BTC')
    expect(view.get('[aria-label="OKX demo account"]').text()).toContain('Reading account')
    expect(get.mock.calls.map(([url]) => url)).toEqual(['BINANCE','OKX'].map(v => '/api/v1/portfolios/PF1/demo-spot-funds/' + v + '?instrument=BTC-USDT&taker_fees=true'))
    await view.setProps({ portfolio: 'PF2' })
    expect(get.mock.calls[1]?.[1]?.signal?.aborted).toBe(true)
    finish(response('OKX'))
    await flushPromises()
    expect(view.find('article').exists()).toBe(false)
    expect(post).not.toHaveBeenCalled()
  } finally { view.unmount() }
})

it('withholds unverified fee terms and account identities, and projects only the HTTP outcome', async () => {
  const wrong = response('BINANCE')
  wrong.funds.owner_tenant = 'other'
  const incomplete = response('OKX')
  incomplete.funds.taker_schedule.received_at = '2026-11-21T08:00:06.000000001Z'
  const get = vi.spyOn(api, 'get').mockResolvedValueOnce(wrong).mockResolvedValueOnce(incomplete)
  const view = mount(DemoSpotAccounts, { props })
  try {
    await view.get('button').trigger('click')
    await flushPromises()
    expect(view.text()).not.toContain('BINANCE-account')
    const okx = view.get('[aria-label="OKX demo account"]')
    expect(okx.text()).toContain('OKX-account')
    expect(okx.text()).toContain('Taker fees unknown')
    get.mockRejectedValueOnce(new ApiError(404, 'upstream-private-detail')).mockResolvedValueOnce(response('OKX'))
    await view.get('button').trigger('click')
    await flushPromises()
    expect(view.text()).toContain('HTTP 404')
    expect(view.text()).not.toContain('upstream-private-detail')
    expect(okx.text()).toContain('OKX account fee group')
    await view.setProps({ identity: { ...props.identity, subject: 'second-reader' } })
    expect(view.find('article').exists()).toBe(false)
  } finally { view.unmount() }
})
