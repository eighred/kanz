import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { useSession } from '../stores/session'
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
      order_id: 'o-1', portfolio_id: 'PF1',
      instrument_id: 'BTC-USD',
      side: 'SIDE_BUY',
      status: 'ORDER_STATUS_FILLED',
      // 2^53 + 1 in the quantity: an exchange quantity in base units is exactly
      // where a JS number stops being exact.
      ordered_quantity: { coefficient: '9007199254740993', exponent: -8 },
      filled_quantity: { coefficient: '9007199254740993', exponent: -8 },
      average_fill_price: { coefficient: '4212345', exponent: -2 },
      venue: 'BINANCE',
    },
  ],
  unindexed: '0',
}

beforeEach(() => {
  vi.restoreAllMocks()
  setActivePinia(createPinia())
  useSession().identity = { tenant: 'fixture-tenant', subject: 'reader', expires_at: '2099-01-01T00:00:00Z' }
})

describe('order observations', () => {
  it('distinguishes an old effective time from an aging observation, and refreshes', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'performance'] })
    try {
      const wrapper = await open({ ...filled, orders: [{ ...filled.orders[0]!, as_of: '2000-01-01T00:00:00Z' }] })
      expect(wrapper.text()).toContain('Effective at 2000-01-01')
      expect(wrapper.text()).not.toContain('snapshot is stale')
      await vi.advanceTimersByTimeAsync(60_001)
      expect(wrapper.text()).toContain('snapshot is stale')
      await wrapper.get('button').trigger('click')
      await flushPromises()
      expect(api.risk.orders).toHaveBeenCalledTimes(2)
      expect(wrapper.text()).not.toContain('snapshot is stale')
      wrapper.unmount()
    } finally {
      vi.useRealTimers()
    }
  })

  it('shows the remote uncertainty alongside the last recorded status', async () => {
    const wrapper = await open({ ...filled, orders: [{ ...filled.orders[0]!, quarantine: {
      at: '2026-01-01T00:00:00Z', last_query_at: '2026-01-01T00:00:01Z',
    } }] })
    expect(wrapper.text()).toContain('Remote outcome unknown')
    expect(wrapper.text()).toContain('Last recorded status: filled')
    expect(wrapper.text()).toContain('Last venue query: 2026-01-01T00:00:01Z')
    expect(wrapper.find('tbody tr .state-warn').exists()).toBe(true)
    wrapper.unmount()
  })
})

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
      orders: [{ portfolio_id: 'PF1', order_id: 'o-2', status: 'ORDER_STATUS_ROUTED' }],
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
    expect(wrapper.get('[role="alert"]').text()).toMatch(/order history is unavailable for this portfolio/i)
    expect(wrapper.find('table').exists()).toBe(false)
  })
})

it('reloads a reused portfolio route and ignores a late order response', async () => {
  let finish!: (value: OrdersResponse) => void
  vi.spyOn(api.risk, 'orders').mockImplementation((id) => id === 'A'
    ? new Promise(resolve => { finish = resolve })
    : Promise.resolve({ orders: [{ portfolio_id: 'B', order_id: 'current-order' }], unindexed: '0' }))
  const r = router()
  await r.push('/portfolios/A/orders')
  const wrapper = mount(OrdersView, { global: { plugins: [r] } })
  await r.push('/portfolios/B/orders')
  await flushPromises()
  finish({ orders: [{ portfolio_id: 'A', order_id: 'previous-order' }], unindexed: '0' })
  await flushPromises()
  expect(wrapper.text()).toContain('current-order')
  expect(wrapper.text()).not.toContain('previous-order')
})

it('discloses the bounded history independently of unindexed orders', async () => {
  const orders = Array.from({ length: 200 }, (_, i) => ({ portfolio_id: 'PF1', order_id: String(i) }))
  const wrapper = await open({ orders, unindexed: '3' })
  expect(api.risk.orders).toHaveBeenCalledWith('PF1', 'fixture-tenant', expect.any(AbortSignal))
  expect(wrapper.find('[role=status]').text()).toContain('Earlier indexed orders may exist')
  expect(wrapper.find('[role=alert]').text()).toContain('3 order(s)')
  const shorter = await open({ orders: orders.slice(1), unindexed: '0' })
  expect(shorter.find('[role=status]').exists()).toBe(false)
})

it('clears the previous book when the reader changes on the same route', async () => {
  const view = await open(filled)
  const read = vi.mocked(api.risk.orders)
  const oldRequest = read.mock.calls[0]![2]!
  read.mockRejectedValueOnce(new ApiError(403, 'refused'))
  useSession().identity!.tenant = 'other'
  await flushPromises()
  expect(oldRequest.aborted).toBe(true)
  expect(read).toHaveBeenLastCalledWith('PF1', 'other', expect.any(AbortSignal))
  expect(view.get('[role=alert]').text()).toMatch(/read authority/)
  expect(view.find('table').exists()).toBe(false)
})

it('shows unrecognized enum codes with neutral status', async () => {
  const view = await open({ orders: [{ portfolio_id: 'PF1', order_id: 'A', side: 99, status: 42 }], unindexed: '0' })
  expect(view.get('.state-unset').text()).toBe('unknown (42)')
  expect(view.text()).toContain('unknown (99)')
})

it('follows a partially executed cancellation through OMS observations on the portfolio route', async () => {
  const order = {
    portfolio_id: 'PF1', order_id: 'synthetic-order', instrument_id: 'SYN',
    ordered_quantity: { coefficient: '125', exponent: -2 },
    filled_quantity: { coefficient: '625', exponent: -3 },
    leaves_quantity: { coefficient: '625', exponent: -3 },
    average_fill_price: { coefficient: '200', exponent: 0 },
    limit_price: { coefficient: '201', exponent: 0 },
  }
  const markTime = '2026-11-19T09:30:01.000000321Z'
  const page = (fields: object) => Response.json({
    owner_tenant: 'fixture-tenant', unindexed: '0', orders: [{ ...order, ...fields }],
  })
  const fetch = vi.fn()
    .mockResolvedValueOnce(page({ status: 'ORDER_STATUS_PARTIALLY_FILLED' }))
    .mockResolvedValueOnce(page({ status: 'ORDER_STATUS_PARTIALLY_FILLED', quarantine: { at: markTime } }))
    .mockResolvedValueOnce(page({ status: 'ORDER_STATUS_CANCELLED' }))
    .mockResolvedValueOnce(Response.json({}, { status: 503 }))
    .mockResolvedValueOnce(page({
      status: 'ORDER_STATUS_FILLED', order_id: 'another-order', filled_quantity: order.ordered_quantity,
      leaves_quantity: { coefficient: '0', exponent: 0 },
      arrival_price: { coefficient: '9007199254740993', exponent: -8 }, arrival_at: markTime,
    }))
  vi.stubGlobal('fetch', fetch)
  const r = router()
  await r.push('/portfolios/PF1/orders')
  const wrapper = mount(OrdersView, { global: { plugins: [r] } })
  const refresh = async () => {
    await wrapper.get('button').trigger('click')
    await flushPromises()
  }
  const cell = (heading: string) => {
    const index = wrapper.findAll('th').findIndex(th => th.text() === heading)
    expect(index).toBeGreaterThanOrEqual(0)
    return wrapper.findAll('tbody td')[index]!.text()
  }
  try {
    await flushPromises()
    expect(fetch.mock.calls[0]?.[0]).toBe('/api/v1/portfolios/PF1/orders?limit=200')
    expect(cell('Status')).toBe('partially filled')
    expect(cell('Filled')).toBe('0.625')
    expect(cell('Unfilled quantity')).toBe('0.625')
    expect(cell('Market mark at admission')).toBe('Unavailable')
    await refresh()
    expect(cell('Status')).toContain('Remote outcome unknown')
    expect(cell('Status')).toContain('Last recorded status: partially filled')
    expect(cell('Filled')).toBe('0.625')
    await refresh()
    expect(cell('Status')).toBe('cancelled')
    expect(cell('Filled')).toBe('0.625')
    expect(cell('Unfilled quantity')).toBe('0.625')
    await refresh()
    expect(wrapper.get('[role=alert]').text()).toMatch(/order history is temporarily unavailable/i)
    expect(wrapper.find('table').exists()).toBe(false)
    await refresh()
    expect(cell('Status')).toBe('filled')
    expect(cell('Unfilled quantity')).toBe('0')
    expect(cell('Market mark at admission')).toContain('90071992.54740993')
    expect(cell('Market mark at admission')).toContain(markTime)
  } finally {
    wrapper.unmount()
    vi.unstubAllGlobals()
  }
})

it.each([
  { arrival_at: '2026-11-19T09:30:00Z' },
  { arrival_price: { coefficient: '200', exponent: 0 } },
])('requires both the admission mark and its observation time', async partial => {
  const view = await open({ ...filled, orders: [{ ...filled.orders[0]!, ...partial }] })
  const cells = view.findAll('tbody td')
  expect(cells.at(-1)!.text()).toBe('Unavailable')
  expect(cells.at(-2)!.text()).toBe('—')
  view.unmount()
})
