import { afterEach, describe, expect, it, vi } from 'vitest'
import { api } from './client'
import example from './spot_depth_example.json'
import { captureDepth, elapsedMillis, fundingRate, isDepthInput, priceDepth, separateSellFees, spotRequest, type DepthInput } from './spot_depth'

function fixture() {
  const input = structuredClone(example)
  if (!isDepthInput(input)) throw new Error('Invalid built-in example')
  return input
}

afterEach(() => vi.restoreAllMocks())

describe('operator spot depth', () => {
  it('preserves clock ordering and distinguishes a submillisecond interval from zero', () => {
    const start = '2026-11-21T11:00:00.999999999+03:00'
    expect(elapsedMillis(start, start)).toBe('0 ms')
    expect(elapsedMillis(start, '2026-11-21T08:00:01Z')).toBe('<0.1 ms')
    expect(elapsedMillis(start, '2026-11-21T08:00:01.123456789Z')).toBe('123.5 ms')
    expect(elapsedMillis(start, '2026-11-21T08:00:00.999999998Z')).toBe('Unknown')
  })
  it('retains exact side-specific execution bounds when replaying an input', () => {
    const input: DepthInput = fixture()
    const venue = input.venues[0]!
    expect(spotRequest(input).spot_depth.venues[0]!.buy_price_range).toBeUndefined()
    venue.buyPriceMax = '60373.00000001'
    venue.sellPriceMin = '50000'
    expect(isDepthInput(JSON.parse(JSON.stringify(input)))).toBe(true)
    const row = spotRequest(input).spot_depth.venues[0]!
    expect(row.buy_price_range).toEqual({ minimum: undefined, maximum: { coefficient: '6037300000001', exponent: -8 } })
    expect(row.sell_price_range).toEqual({ minimum: { coefficient: '5', exponent: 4 }, maximum: undefined })
  })
  it('keeps a subprecision return distinct from zero and an absent budget distinct from zero', () => {
    const zero = { coefficient: '0', exponent: 0 }
    expect(fundingRate(zero, { coefficient: '-1', exponent: -18 })).toBe('Between −0.0000005 and 0 bps')
    expect(fundingRate(zero, { coefficient: '1', exponent: -18 })).toBe('Between 0 and 0.0000005 bps')
    expect(fundingRate(zero, zero)).toBe('0 bps')
    const input: DepthInput = fixture()
    expect(spotRequest(input).spot_depth.one_leg_loss_limit).toBeUndefined()
    input.oneLegLossLimit = '0'
    expect(spotRequest(input).spot_depth.one_leg_loss_limit).toEqual(zero)
  })
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
    for (const known_at of ['2026-11-21T08:00:00.249999999Z', '2026-11-21T08:00:00.500000001Z']) {
      post.mockResolvedValueOnce({ ...raw, spot_books: { ...observed, venues: [{ ...observed.venues[0], known_at }, observed.venues[1]] } })
      await expect(read()).rejects.toThrow('out of order')
    }
    post.mockResolvedValueOnce({ ...raw, spot_books: { ...observed, product_type: 'EQUITY_TOKEN' } })
    await expect(read()).rejects.toThrow('incomplete')
    post.mockResolvedValueOnce({ ...raw, spot_books: { status: 'UNKNOWN', instrument_id: input.instrument } })
    await expect(read()).rejects.toThrow('No authorized current pair')
    const refused = { status: 'UNKNOWN', instrument_id: input.instrument, as_of: input.cutoff,
      refusal: 'SPOT_BOOK_REFUSAL_STALE', refusal_venue: 'OKX', reason: 'OKX book is older than 2s.' }
    post.mockResolvedValueOnce({ ...raw, spot_books: refused })
    await expect(read()).rejects.toThrow(refused.reason)
    post.mockResolvedValueOnce({ ...raw, spot_books: { ...refused, refusal: 'SPOT_BOOK_REFUSAL_UNSPECIFIED' } })
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

  it('replays old fee inputs and keeps an absent sell fee distinct from a free buy', () => {
    const input: DepthInput = fixture()
    const old = JSON.stringify(spotRequest(input))
    separateSellFees(input.venues)
    expect(JSON.stringify(spotRequest(input))).toBe(old)
    Object.assign(input.venues[0]!, { feeBps: '0', sellFeeBps: '', sellFeeCurrency: 'USDT' })
    const terms = spotRequest(input).spot_depth.venues[0]!
    expect(terms.fee_bps).toBeUndefined()
    expect(terms.buy_taker_fee).toEqual({ bps: { coefficient: '0', exponent: 0 }, currency: 'BTC' })
    expect(terms.sell_taker_fee).toEqual({ bps: undefined, currency: 'USDT' })
    delete input.venues[0]!.sellFeeCurrency
    expect(isDepthInput(input)).toBe(false)
  })

  it('keeps quantity and value limits in their original units and preserves the rule receipt', () => {
    const input: DepthInput = fixture()
    const v = input.venues[0]!
    v.maxQty = '2.5'
    v.limitCurrency = 'USD'
    v.maxMarketValue = '1000000'
    v.limitReferencePrice = ''
    v.ruleEvidence = [{source: 'OKX instrument terms', known_at: input.cutoff, response_sha256: '1'.repeat(64)}]
    const wire = spotRequest(input).spot_depth.venues[0]!
    expect(wire.maximum_quantity).toEqual({ coefficient: '25', exponent: -1 })
    expect(wire.market_value_limit).toEqual({ currency: 'USD', maximum_value: { coefficient: '1', exponent: 6 },
      minimum_value: undefined, reference_price: undefined })
    expect(wire.rule_evidence).toEqual(v.ruleEvidence)
    expect(isDepthInput(input)).toBe(true)
    v.ruleEvidence[0]!.response_sha256 = 'wrong'
    expect(isDepthInput(input)).toBe(false)
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
    await expect(read()).resolves.toEqual({ result, serviceTiming: undefined })
    const timing = { started_at: input.cutoff, completed_at: '2026-11-21T08:00:01.000000002Z' }
    post.mockResolvedValueOnce({ ...raw, spot_depth_timing: timing })
    await expect(read()).resolves.toEqual({ result, serviceTiming: timing })
    post.mockResolvedValueOnce({ ...raw, spot_depth_timing: { ...timing, completed_at: '2020-01-01T00:00:00Z' } })
    await expect(read()).resolves.toEqual({ result, serviceTiming: undefined })
    for (const mismatch of [
      { ...raw, owner_tenant: 'other' },
      { ...raw, portfolio_id: 'other' },
      { ...raw, spot_depth: { ...result, as_of: '2026-11-21T08:00:00.500000002Z' } },
      { ...raw, spot_depth: { ...result, quote_currency: 'USD' } },
      { ...raw, spot_depth: { ...result, directions: result.directions.map(d => ({
        ...d, local_unwind: { status: 'SUPPORTED' },
      })) } },
    ]) {
      post.mockResolvedValueOnce(mismatch)
      await expect(read()).rejects.toThrow('could not be verified')
    }
  })
})
