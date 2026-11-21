import { afterEach, describe, expect, it, vi } from 'vitest'
import { api } from './client'
import example from './spot_depth_example.json'
import { isDepthInput, priceDepth, spotRequest } from './spot_depth'

function fixture() {
  const input = structuredClone(example)
  if (!isDepthInput(input)) throw new Error('Invalid built-in example')
  return input
}

afterEach(() => vi.restoreAllMocks())

describe('operator spot depth', () => {
  it('preserves large coefficients, absent fees and explicitly free rebalancing', () => {
    const input = fixture()
    input.quantity = '9007199254740993.01'
    input.rebalanceCost = '0'
    input.venues[0]!.feeBps = ''
    const wire = spotRequest(input).spot_depth
    expect(wire.requested_buy_quantity).toEqual({ coefficient: '900719925474099301', exponent: -2 })
    expect(wire.venues[0]!.fee_bps).toBeUndefined()
    expect(wire.rebalance_cost).toEqual({ coefficient: '0', exponent: 0 })
    input.quantity = '9223372036854775808'
    expect(() => spotRequest(input)).toThrow('exact decimal range')
  })

  it('binds the reply to the portfolio, tenant and exact observation cutoff', async () => {
    const input = fixture()
    input.cutoff = '2026-11-21T11:00:00.500000001+03:00'
    const result = {
      instrument_id: input.instrument,
      base_currency: 'BTC',
      quote_currency: 'USDT',
      input_sha256: '1'.repeat(64),
      as_of: '2026-11-21T08:00:00.500000001Z',
      directions: input.venues.map((venue, i) => ({
        status: 'UNKNOWN',
        reason: 'missing fee',
        buy_venue: venue.venue,
        sell_venue: input.venues[1 - i]!.venue,
      })),
    }
    const raw = { portfolio_id: 'portfolio', owner_tenant: 'owner', spot_depth: result }
    const post = vi.spyOn(api, 'post').mockResolvedValue(raw)
    const read = () => priceDepth('portfolio', 'owner', input, new AbortController().signal)
    await expect(read()).resolves.toEqual(result)
    for (const mismatch of [
      { ...raw, owner_tenant: 'other' },
      { ...raw, portfolio_id: 'other' },
      { ...raw, spot_depth: { ...result, as_of: '2026-11-21T08:00:00.500000002Z' } },
      { ...raw, spot_depth: { ...result, quote_currency: 'USD' } },
    ]) {
      post.mockResolvedValueOnce(mismatch)
      await expect(read()).rejects.toThrow('could not be verified')
    }
  })
})
