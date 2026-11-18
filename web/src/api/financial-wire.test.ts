import { afterEach, describe, expect, it, vi } from 'vitest'
import { api } from './client'
import { approvals } from './approvals'
import { risk } from './risk'
import { formatDecimal } from './decimal'

afterEach(() => vi.restoreAllMocks())

describe('financial JSON at the browser read boundaries', () => {
  it.each([17, '17', [], { coefficient: 17 }, { coefficient: null }, { exponent: null }, { exponent: '2' }])(
    'refuses an invalid quantity before order history can render it', async invalid => {
      vi.spyOn(api, 'get').mockResolvedValue({ orders: [{ order_id: 'one', quantity: invalid }] })
      await expect(risk.orders('portfolio')).rejects.toThrow('invalid decimal representation')
    },
  )

  it('checks filled quantities and both prices in an order history', async () => {
    const get = vi.spyOn(api, 'get')
    for (const field of ['filled_quantity', 'limit_price', 'average_fill_price']) {
      get.mockResolvedValueOnce({ orders: [{ [field]: false }] })
      await expect(risk.orders('portfolio')).rejects.toThrow(/invalid (decimal|money) representation/)
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
    vi.spyOn(api, 'get').mockResolvedValue({ orders: [
      { order_id: 'missing' }, { order_id: 'zero', quantity: { exponent: -2 } },
      { order_id: 'legacy', quantity: { coefficient: digits } },
    ] })
    const result = await risk.orders('portfolio')
    expect(result.orders?.map(order => formatDecimal(order.quantity))).toEqual([null, '0.00', digits])
  })
})
