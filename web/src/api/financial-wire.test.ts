import { afterEach, describe, expect, it, vi } from 'vitest'
import { api } from './client'
import { approvals } from './approvals'
import { risk } from './risk'
import { formatDecimal } from './decimal'

afterEach(() => vi.restoreAllMocks())

function history(...orders: Record<string, unknown>[]) {
  return { owner_tenant: 'tenant', unindexed: '0', orders: orders.map(order => ({ portfolio_id: 'portfolio', ...order })) }
}

describe('financial JSON at the browser read boundaries', () => {
  it.each([17, '17', [], { coefficient: 17 }, { coefficient: null }, { exponent: null }, { exponent: '2' }])(
    'refuses an invalid quantity before order history can render it', async invalid => {
      vi.spyOn(api, 'get').mockResolvedValue(history({ order_id: 'one', ordered_quantity: invalid }))
      await expect(risk.orders('portfolio', 'tenant')).rejects.toThrow('invalid decimal representation')
    },
  )

  it('checks filled quantities and both prices in an order history', async () => {
    const get = vi.spyOn(api, 'get')
    for (const field of ['filled_quantity', 'limit_price', 'average_fill_price']) {
      get.mockResolvedValueOnce(history({ order_id: 'one', [field]: false }))
      await expect(risk.orders('portfolio', 'tenant')).rejects.toThrow(/invalid (decimal|money) representation/)
    }
  })

  it('rejects malformed money in either exposure column before its sign is inspected', async () => {
    const get = vi.spyOn(api, 'get')
    for (const field of ['gross', 'net']) {
      get.mockResolvedValueOnce({ set: { exposures: [{ [field]: { amount: { coefficient: -2 } } }] } })
      await expect(risk.exposure('portfolio')).rejects.toThrow('invalid decimal representation')
    }
    get.mockResolvedValueOnce({ set: { exposures: [{ net: { amount: {}, currency_code: {} } }] } })
    await expect(risk.exposure('portfolio')).rejects.toThrow('invalid money representation')
  })

  it('refuses malformed proposed terms before they reach the confirmation queue', async () => {
    const get = vi.spyOn(api, 'get')
    for (const field of ['quantity', 'limit_price', 'stop_price']) {
      get.mockResolvedValueOnce({ pending: [{ command: { [field]: { coefficient: null } } }] })
      await expect(approvals.list()).rejects.toThrow('invalid decimal representation')
    }
  })

  it('preserves absent values, omitted scalar defaults and exact legacy text', async () => {
    const digits = '9'.repeat(129)
    vi.spyOn(api, 'get').mockResolvedValue(history(
      { order_id: 'missing' }, { order_id: 'zero', ordered_quantity: { exponent: -2 } },
      { order_id: 'legacy', ordered_quantity: { coefficient: digits } },
    ))
    const result = await risk.orders('portfolio', 'tenant')
    expect(result.orders?.map(order => formatDecimal(order.ordered_quantity))).toEqual([null, '0.00', digits])
  })
})
