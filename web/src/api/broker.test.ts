import { afterEach, describe, expect, it, vi } from 'vitest'
import { api, ApiError } from './client'
import { broker, describeBroker } from './broker'
import served from '../../../kanz/services/tv_sync/internal/brokerapi/testdata/account_read.json'

afterEach(() => vi.restoreAllMocks())

const state = { id: 'PF1/account', currency: 'USD', balance: '100.00000001', equity: '99.5', realizedPl: '-1', unrealizedPl: '0.5', openPositions: 1 }

describe('broker response boundary', () => {
  it.each([
    { baseCurrency: 'BTC' },
    { baseCurrency: 'BTC', quoteCurrency: 'BTC' },
    { inventoryChange: '0.9992' },
    { cashChange: '-50000', inventoryChange: '0.9992' },
    { baseCurrency: 'BTC', quoteCurrency: 'USDT', cashChange: '-50000', inventoryChange: '0.9992', feeCurrency: undefined },
  ])('refuses a partial or unitless settlement receipt', async fields => {
    vi.spyOn(api, 'get').mockResolvedValueOnce(served.state).mockResolvedValueOnce(served.positions)
      .mockResolvedValueOnce(served.orders).mockResolvedValueOnce({
        executions: [{ ...served.executions.executions[0], ...fields }],
      })
    await expect(broker.account('fund-a')).rejects.toThrow('Invalid broker execution receipt.')
  })

  it('reads the authorized projection responses without changing financial text or adding absent fields', async () => {
    const fetch = vi.spyOn(globalThis, 'fetch')
    for (const body of [served.accounts, served.state, served.positions, served.orders, served.executions]) {
      fetch.mockResolvedValueOnce(Response.json(body))
    }
    expect(await broker.accounts()).toEqual(served.accounts.accounts)
    const account = await broker.account('fund-a')
    expect(account.state).toEqual(served.state)
    expect(account.positions).toEqual(served.positions.positions)
    expect(account.orders).toEqual(served.orders.orders)
    expect(account.executions).toEqual(served.executions.executions)
    expect(fetch.mock.calls.map(call => call[0])).toEqual([
      '/api/v1/broker/accounts',
      ...['state', 'positions', 'orders', 'executions'].map(path => '/api/v1/broker/accounts/fund-a/' + path),
    ])
    for (const [, options] of fetch.mock.calls) {
      expect(options).toMatchObject({ method: 'GET', credentials: 'same-origin' })
    }
  })

  it('preserves signed zero, trailing fractional digits and values beyond the JavaScript number range', async () => {
    const exact = '900719925474099312345.0000000000001000'
    vi.spyOn(api, 'get')
      .mockResolvedValueOnce({ ...served.state, equity: '-0.0000' })
      .mockResolvedValueOnce(served.positions)
      .mockResolvedValueOnce({ orders: [{ ...served.orders.orders[0], venue: '', limitPrice: exact }] })
      .mockResolvedValueOnce({ executions: [{ ...served.executions.executions[0], qty: exact, fee: '0' }] })
    const detail = await broker.account('fund-a')
    expect(detail.state.equity).toBe('-0.0000')
    expect(detail.orders[0]?.venue).toBe('')
    expect(detail.orders[0]?.limitPrice).toBe(exact)
    expect(detail.executions[0]?.qty).toBe(exact)
    expect(detail.executions[0]?.fee).toBe('0')
  })

  it.each([
    ['state', 'balance', 'NaN'], ['state', 'equity', '1/3'], ['state', 'realizedPl', '--2'],
    ['state', 'unrealizedPl', ' 0'], ['positions', 'qty', '1e3'], ['positions', 'avgPrice', '1.2.5'],
    ['positions', 'unrealizedPl', ''], ['positions', 'realizedPl', 'A'], ['orders', 'qty', '3#'],
    ['orders', 'filledQty', '3 apples'], ['orders', 'leavesQty', '+'],
    ['orders', 'avgFillPrice', 'Infinity'], ['orders', 'limitPrice', '0\n'],
    ['executions', 'qty', ''], ['executions', 'price', '-'], ['executions', 'fee', 'not available'],
  ])('refuses malformed %s.%s before returning the account detail', async (resource, key, value) => {
    const rows = { state: served.state, positions: served.positions.positions[0],
      orders: served.orders.orders[0], executions: served.executions.executions[0] }
    const get = vi.spyOn(api, 'get')
    for (const [name, row] of Object.entries(rows)) {
      const changed = name === resource ? { ...row, [key]: value } : row
      get.mockResolvedValueOnce(name === 'state' ? changed : { [name]: [changed] })
    }
    await expect(broker.account('fund-a')).rejects.toThrow('Invalid broker response')
  })

  it.each([
    ['order', '1'],
    ['execution', '2024-02-29T12:00:00'],
    ['orders window', '2025-02-29T00:00:00Z'],
    ['executions window', '2024-02-30T12:00:00.123456789Z'],
    ['execution', '2024-02-29T12:00:00.1234567890Z'],
  ])('refuses an invalid %s timestamp in the account response', async (resource, invalid) => {
    const valid = '2024-02-29T12:00:00.123456789Z'
    vi.spyOn(api, 'get')
      .mockResolvedValueOnce(served.state).mockResolvedValueOnce(served.positions)
      .mockResolvedValueOnce({
        orders: [{ ...served.orders.orders[0], updatedAt: resource === 'order' ? invalid : valid }],
        retainedFrom: resource === 'orders window' ? invalid : valid,
      })
      .mockResolvedValueOnce({
        executions: [{ ...served.executions.executions[0], time: resource === 'execution' ? invalid : valid }],
        retainedFrom: resource === 'executions window' ? invalid : valid,
      })
    await expect(broker.account('fund-a')).rejects.toThrow('Invalid broker response')
  })

  it('preserves a leap-day retention horizon down to nanoseconds', async () => {
    const retainedFrom = '2024-02-29T23:59:59.123456789Z'
    vi.spyOn(api, 'get')
      .mockResolvedValueOnce(served.state).mockResolvedValueOnce(served.positions)
      .mockResolvedValueOnce({ ...served.orders, retainedFrom })
      .mockResolvedValueOnce({ ...served.executions, retainedFrom })
    const account = await broker.account('fund-a')
    expect(account.ordersRetainedFrom).toBe(retainedFrom)
    expect(account.executionsRetainedFrom).toBe(retainedFrom)
  })

  it('preserves decimal strings and marks retained list windows', async () => {
    const get = vi.spyOn(api, 'get')
      .mockResolvedValueOnce(state)
      .mockResolvedValueOnce({ positions: [{ instrument: 'BTC-USD', side: 'long', qty: '0.00000001', avgPrice: '100', realizedPl: '0' }] })
      .mockResolvedValueOnce({ orders: [], retainedFrom: '2026-01-01T00:00:00Z' })
      .mockResolvedValueOnce({ executions: null })
    const result = await broker.account('PF1/account')
    expect(get.mock.calls[0]?.[0]).toBe('/api/v1/broker/accounts/PF1%2Faccount/state')
    expect(result.positions[0]?.qty).toBe('0.00000001')
    expect(result.ordersRetainedFrom).toBe('2026-01-01T00:00:00Z')
    expect(result.executions).toEqual([])
  })

  it.each([
    { ...state, balance: 100 }, { ...state, openPositions: -1 }, { ...state, openPositions: 1.5 },
  ])('rejects an invalid state rather than presenting financial data', async (invalid) => {
    vi.spyOn(api, 'get')
      .mockResolvedValueOnce(invalid).mockResolvedValueOnce({ positions: [] })
      .mockResolvedValueOnce({ orders: [] }).mockResolvedValueOnce({ executions: [] })
    await expect(broker.account('PF1')).rejects.toThrow('Invalid broker response')
  })

  it('keeps denial and deployment absence distinct', () => {
    expect(describeBroker(new ApiError(403, 'hidden'))).toContain('not permitted')
    expect(describeBroker(new ApiError(404, 'hidden'))).toContain('unavailable')
  })
})

it('passes cancellation to all reads and refuses a different returned account',async()=>{
 const controller=new AbortController()
 const get=vi.spyOn(api,'get').mockResolvedValueOnce(state).mockResolvedValueOnce({positions:[]}).mockResolvedValueOnce({orders:[]}).mockResolvedValueOnce({executions:[]})
 await expect(broker.account('different',controller.signal)).rejects.toThrow('identity differs')
 expect(get.mock.calls).toHaveLength(4)
 for(const call of get.mock.calls) expect(call[1]?.signal).toBe(controller.signal)
})
