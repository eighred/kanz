import { afterEach, expect, it, vi } from 'vitest'
import { api } from './client'
import { reference } from './reference'

afterEach(() => vi.restoreAllMocks())
const price = { instrument_id: 'BTC/USD', has_price: true, chosen: '12345678901.12345678', exceptions: 0, stale_candidates: 0, observed_at: '2026-09-13T00:00:00Z', observation_only: true }

it('encodes price identifiers and preserves every decimal digit', async () => {
  const get = vi.spyOn(api, 'get').mockResolvedValue(price)
  expect((await reference.price('BTC/USD')).chosen).toBe(price.chosen)
  expect(get).toHaveBeenCalledWith('/api/v1/prices/BTC%2FUSD')
})

it.each([null, {}, { ...price, chosen: 100 }, { ...price, chosen: 'NaN' }, { ...price, instrument_id: 'other' },
  { ...price, observation_only: undefined }, { ...price, has_price: false }, { ...price, observed_at: '' },
  { ...price, stale_candidates: 1 }, { ...price, exceptions: -1 }])('refuses unverifiable price responses', async (body) => {
  vi.spyOn(api, 'get').mockResolvedValue(body)
  await expect(reference.price('BTC/USD')).rejects.toThrow('Invalid price observation')
})

it('keeps missing and zero prices distinct', async () => {
  vi.spyOn(api, 'get').mockResolvedValueOnce({ ...price, chosen: '0' }).mockResolvedValueOnce({ ...price, has_price: false, chosen: undefined, exceptions: 1 })
  expect((await reference.price('BTC/USD')).chosen).toBe('0')
  expect((await reference.price('BTC/USD')).chosen).toBeUndefined()
})
