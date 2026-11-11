import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createRouter, createWebHistory } from 'vue-router'
import OrdersView from './OrdersView.vue'
import * as api from '../api/risk'
import { ApiError } from '../api/client'
import type { OrdersResponse } from '../api/risk'

// THE HARD REQUIREMENT ON THIS SCREEN IS NOT RENDERING ORDERS.
//
// It is being honest about the ones it cannot show. Orders placed before the OMS
// indexed by portfolio can never appear in any page — their portfolio lives only
// inside a marshaled blob — so a table that omits them silently is a partial
// book presented as a complete one, and a person reads an order history to
// decide something.

function router() {
  return createRouter({
    history: createWebHistory(),
    routes: [
      { path: '/portfolios/:id/orders', name: 'orders', component: OrdersView },
      { path: '/portfolios/:id/exposure', name: 'exposure', component: { template: '<div />' } },
      { path: '/portfolios', name: 'portfolios', component: { template: '<div />' } },
    ],
  })
}

async function open(resp: OrdersResponse | Error) {
  if (resp instanceof Error) vi.spyOn(api.risk, 'orders').mockRejectedValue(resp)
  else vi.spyOn(api.risk, 'orders').mockResolvedValue(resp)
  const r = router()
  await r.push('/portfolios/PF1/orders')
  await r.isReady()
  const wrapper = mount(OrdersView, { global: { plugins: [r] } })
  await flushPromises()
  return wrapper
}

const filled: OrdersResponse = {
  orders: [
    {
      order_id: 'o-1',
      instrument_id: 'BTC-USD',
      side: 'SIDE_BUY',
      status: 'ORDER_STATUS_FILLED',
      // 2^53 + 1 in the quantity: an exchange quantity in base units is exactly
      // where a JS number stops being exact.
      quantity: { coefficient: '9007199254740993', exponent: -8 },
      filled_quantity: { coefficient: '9007199254740993', exponent: -8 },
      average_fill_price: { amount: { coefficient: '4212345', exponent: -2 }, currency_code: 'USD' },
      venue: 'BINANCE',
    },
  ],
  unindexed: '0',
}

beforeEach(() => vi.restoreAllMocks())

describe('orders that cannot be shown are declared', () => {
  it('states the unindexed count as an alert, not a hint', async () => {
    const wrapper = await open({ ...filled, unindexed: '412' })

    const alert = wrapper.find('[role="alert"]')
    expect(alert.exists()).toBe(true)
    expect(alert.text()).toContain('412')
    expect(alert.text()).toMatch(/partial history/i)
  })

  it('says nothing when every order is indexed', async () => {
    const wrapper = await open(filled)
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
  })

  // AN EMPTY TABLE PLUS UNINDEXED ORDERS IS THE MOST MISLEADING STATE AVAILABLE:
  // it reads as "this portfolio has never traded" when the truth is "we cannot
  // tell you what it traded".
  it('does not let an empty page read as a portfolio that never traded', async () => {
    const wrapper = await open({ orders: [], unindexed: '5' })
    expect(wrapper.text()).toMatch(/not the same as none having been placed/i)
  })

  // The count is an int64 and arrives as a string. Comparing it as a string is
  // deliberate — parsing one to ask "is it zero" is how a large one becomes
  // 9007199254740992.
  it('treats a count beyond 2^53 as non-zero without parsing it', async () => {
    const wrapper = await open({ ...filled, unindexed: '9007199254740993' })
    expect(wrapper.find('[role="alert"]').text()).toContain('9007199254740993')
  })
})

describe('figures never pass through a JS number', () => {
  it('renders a quantity beyond 2^53 exactly', async () => {
    const wrapper = await open(filled)
    expect(wrapper.text()).toContain('90071992.54740993')
  })

  it('renders an absent price as unrenderable, never as zero', async () => {
    const wrapper = await open({
      orders: [{ order_id: 'o-2', status: 'ORDER_STATUS_ROUTED' }],
      unindexed: '0',
    })
    const cells = wrapper.findAll('td').map((c) => c.text())
    expect(cells).toContain('—')
    expect(cells).not.toContain('0')
  })
})

describe('a refusal is not an empty history', () => {
  // The route is registered only when the gateway fronts an OMS read surface, so
  // a 404 can also mean "this deployment serves no order history". Neither
  // reading is "this portfolio has never traded".
  it('explains a 404 rather than drawing an empty table', async () => {
    const wrapper = await open(new ApiError(404, 'portfolio not found'))
    expect(wrapper.find('[role="alert"]').text()).toMatch(/not the same as the fund having none/i)
    expect(wrapper.find('table').exists()).toBe(false)
  })
})
