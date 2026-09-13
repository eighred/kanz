import { afterEach, describe, expect, it, vi } from 'vitest'
import { api, ApiError } from './client'
import { broker, describeBroker } from './broker'

afterEach(() => vi.restoreAllMocks())

const state = { id: 'PF1/account', currency: 'USD', balance: '100.00000001', equity: '99.5', realizedPl: '-1', unrealizedPl: '0.5', openPositions: 1 }

describe('broker response boundary', () => {
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
