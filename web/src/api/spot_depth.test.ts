import { afterEach, describe, expect, it, vi } from 'vitest'
import { api } from './client'
import example from './spot_depth_example.json'
import { captureDepth, isDepthInput, priceDepth, spotRequest } from './spot_depth'

function fixture() {
  const input = structuredClone(example)
  if (!isDepthInput(input)) throw new Error('Invalid built-in example')
  return input
}

afterEach(() => vi.restoreAllMocks())

describe('operator spot depth', () => {
  it('keeps market observations distinct from venue account terms', async () => {
    const input = fixture()
    const rows = [...spotRequest(input).spot_depth.venues].reverse()
    const observed = {
      status: 'SUPPORTED',
      as_of: input.cutoff,
      instrument_id: input.instrument,
      source: 'demo',
      producer_version: '1.1',
      product_type: 'CRYPTO_SPOT',
      rights_reference: 'local-demo-only',
      base_asset_ref: 'bitcoin',
      quote_asset_ref: 'tether-usd',
      venues: rows.map((row, i) => ({
        book: row.book, known_at: row.known_at, event_id: 'book-' + i,
        portfolio_account_ref: i === 0 ? 'bn-pilot' : undefined,
      })),
    }
    const raw = { portfolio_id: 'PF1', owner_tenant: 'pilot', spot_books: observed }
    const post = vi.spyOn(api, 'post').mockResolvedValue(raw)
    const read = () => captureDepth('PF1', 'pilot', input.instrument, new AbortController().signal)
    const captured = await read()
    expect(captured.venues[0]?.venue).toBe('BINANCE')
    expect(captured.venues[0]?.feeBps).toBe('')
    expect(captured.venues[0]?.account).toBe('bn-pilot')
    expect(captured.venues[1]?.account).toBe('')
    expect(captured.observation).toMatchObject({ owner_tenant: 'pilot', portfolio_id: 'PF1' })
    expect(captured.venues[0]?.baseBalance).toBe('')
    expect(captured.observation?.event_ids).toEqual(['book-0', 'book-1'])
    post.mockResolvedValueOnce({ ...raw, spot_books: { ...observed, product_type: 'EQUITY_TOKEN' } })
    await expect(read()).rejects.toThrow('incomplete')
    post.mockResolvedValueOnce({ ...raw, spot_books: { status: 'UNKNOWN', instrument_id: input.instrument } })
    await expect(read()).rejects.toThrow('No authorized current pair')
  })
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
