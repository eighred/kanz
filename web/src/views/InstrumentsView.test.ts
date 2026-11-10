import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createRouter, createWebHistory } from 'vue-router'
import InstrumentsView from './InstrumentsView.vue'
import * as api from '../api/instruments'
import { ApiError } from '../api/client'
import type { InstrumentsResponse } from '../api/instruments'

// THE CLAIMS THIS SCREEN MAKES:
//
//   1. every pair it shows is ROUTABLE — it reports configuration, so a pair
//      here will not be refused at admission;
//   2. the exchange symbol is shown, because "BTC-USD" trades as BTCUSDT and a
//      user picking it is buying a stablecoin-quoted pair;
//   3. an empty catalogue is a deployment that can trade NOTHING, stated as
//      such — never a blank table that reads as "quiet".

function router() {
  return createRouter({
    history: createWebHistory(),
    routes: [
      { path: '/instruments', name: 'instruments', component: InstrumentsView },
      {
        path: '/portfolios',
        name: 'portfolios',
        component: { template: '<div />' },
      },
    ],
  })
}

async function open(resp: InstrumentsResponse | Error) {
  if (resp instanceof Error) vi.spyOn(api.instruments, 'list').mockRejectedValue(resp)
  else vi.spyOn(api.instruments, 'list').mockResolvedValue(resp)
  const r = router()
  await r.push('/instruments')
  await r.isReady()
  const wrapper = mount(InstrumentsView, { global: { plugins: [r] } })
  await flushPromises()
  return wrapper
}

const base: InstrumentsResponse = {
  owner_tenant: 'acme',
  instruments: [
    // The platform resolves base/quote from the SYMBOL and states the
    // disagreement; the screen reports what it is told (#407).
    {
      instrument_id: 'BTC-USD',
      venue_symbol: 'BTCUSDT',
      mic: 'XBIN',
      base_asset: 'BTC',
      quote_asset: 'USDT',
      quote_mismatch: true,
    },
    {
      instrument_id: 'BTC-USD',
      venue_symbol: 'BTC-USDT',
      mic: 'XOKX',
      base_asset: 'BTC',
      quote_asset: 'USDT',
      quote_mismatch: true,
    },
    {
      instrument_id: 'ETH-EUR',
      venue_symbol: 'ETHEUR',
      mic: 'XBIN',
      base_asset: 'ETH',
      quote_asset: 'EUR',
      quote_mismatch: false,
    },
  ],
}

beforeEach(() => vi.restoreAllMocks())

describe('a pair is shown with the venue that can actually route it', () => {
  it('lists every venue for a pair rather than collapsing them', async () => {
    const wrapper = await open(base)
    const text = wrapper.text()

    // The same canonical id at two venues is two ROUTES, not a duplicate: an
    // order names a venue, so collapsing them would offer a pair with nowhere
    // to send it.
    expect(text).toContain('XBIN')
    expect(text).toContain('XOKX')
    expect(wrapper.findAll('tbody tr')).toHaveLength(3)
  })

  it('shows the exchange symbol, not only the canonical id', async () => {
    const wrapper = await open(base)
    const text = wrapper.text()
    expect(text).toContain('BTCUSDT')
    expect(text).toContain('BTC-USDT')
  })
})

// The quote mismatch is the reason the symbol is on screen at all. "BTC-USD"
// trading as BTCUSDT is a USDT position marked as dollars — a different credit
// exposure behind a peg this platform never states and cannot monitor (#407).
describe('a pair whose quote is not what its id claims says so', () => {
  it('marks BTC-USD trading as BTCUSDT', async () => {
    const wrapper = await open(base)
    const row = wrapper.findAll('tbody tr')[0]!
    expect(row.text()).toMatch(/quoted in USDT, not USD/i)
  })

  // A pair the platform could not decompose must not be rendered as a fact. Both
  // fields empty is "could not tell", and a screen that showed it as a mismatch
  // would put a scary warning on a correctly configured venue.
  it('says nothing about a pair whose quote the platform could not determine', async () => {
    const wrapper = await open({
      owner_tenant: 'acme',
      instruments: [{ instrument_id: 'BTC-USD', venue_symbol: 'XBTUSD', mic: 'XBIN' }],
    })
    const row = wrapper.findAll('tbody tr')[0]!
    expect(row.text()).not.toMatch(/quoted in/i)
  })

  it('says nothing when the symbol agrees with the id', async () => {
    const wrapper = await open(base)
    const rows = wrapper.findAll('tbody tr')
    expect(rows).toHaveLength(3)
    const eth = rows[2]!
    expect(eth.text()).toContain('ETHEUR')
    // It states the quote it resolved, without the mismatch warning: "checked,
    // and fine" must not look like "nobody looked".
    expect(eth.text()).toMatch(/quoted in EUR/i)
    expect(eth.text()).not.toMatch(/not EUR/i)
  })
})

describe('an empty catalogue is a statement, not a blank table', () => {
  it('says the deployment can trade nothing, with alert prominence', async () => {
    const wrapper = await open({ owner_tenant: 'acme', instruments: [] })

    const alert = wrapper.find('[role="alert"]')
    expect(alert.exists()).toBe(true)
    expect(alert.text()).toMatch(/can trade nothing/i)
    expect(alert.text()).toMatch(/configuration state/i)
    expect(wrapper.find('table').exists()).toBe(false)
  })

  // A REFUSAL IS NOT AN EMPTY ESTATE. 404 means this deployment serves no
  // catalogue, which is a different sentence from "it can trade nothing" — and
  // presenting the first as the second would report a routing gap as a platform
  // with no markets.
  it('explains a 404 rather than claiming the platform trades nothing', async () => {
    const wrapper = await open(new ApiError(404, 'not found'))

    const alert = wrapper.find('[role="alert"]')
    expect(alert.text()).toMatch(/does not serve a tradeable-pair catalogue/i)
    expect(alert.text()).not.toMatch(/can trade nothing/i)
  })
})
