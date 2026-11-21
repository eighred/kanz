import { api } from './client'
import { formatDecimal, MAX_SAFE_EXPONENT, type Decimal, type Money } from './decimal'
import { isTimestamp } from './timestamp'

export interface VenueInput {
  venue: string
  account: string
  // Captured deployment reference, retained with saved scenario inputs.
  portfolioAccount?: string
  portfolioAccountMic?: string
  fundsObservation?: {
    request_started_at: string
    received_at: string
    available_base: string
    available_quote: string
    source: 'Binance Spot Testnet account' | 'OKX Demo spot cash capacity'
  }
  feeObservation?: {
    request_started_at: string
    received_at: string
    buyBps: string
    buyCurrency: string
    sellBps: string
    sellCurrency: string
    source: 'Binance account commission' | 'OKX account fee group'
  }
  symbol: string
  observed: string
  received: string
  sequence: string
  baseAssetRef: string
  quoteAssetRef: string
  productType: string
  bids: [string, string][]
  asks: [string, string][]
  rebalanceCostAfterBuy?: string
  baseBalance: string
  quoteBalance: string
  step: string
  minQty: string
  minNotional: string
  maxQty?: string
  limitCurrency?: string
  limitReferencePrice?: string
  minMarketValue?: string
  maxMarketValue?: string
  buyPriceMin?: string
  buyPriceMax?: string
  sellPriceMin?: string
  sellPriceMax?: string
  ruleEvidence?: { source: string; known_at: string; response_sha256: string }[]
  feeBps: string
  feeCurrency: string
  // Historical files used the buy fields for both sides.
  sellFeeBps?: string
  sellFeeCurrency?: string
}
export interface DepthInput {
  observation?: {
    portfolio_id?: string
    owner_tenant?: string
    source: string
    producer_version: string
    rights_reference: string
    event_ids: string[]
    known_at_clock: 'risk_service_receipt'
  }
  instrument: string
  cutoff: string
  quantity: string
  rebalanceCost: string
  sellFillBps: string
  adverseMoveBps: string
  oneLegLossLimit?: string
  venues: VenueInput[]
}

/** Parse fixed-point text for the canonical int64 coefficient without a JS number round trip. */
function decimal(s: string): Decimal | undefined {
  if (s === '') return undefined
  if (s.length > 128 || !/^-?\d+(\.\d+)?$/.test(s)) throw new Error('Use fixed-point decimal amounts.')
  const [whole, fraction = ''] = s.split('.')
  let n = BigInt(whole + fraction),
    exponent = -fraction.length
  if (n === 0n) return { coefficient: '0', exponent: 0 }
  while (n % 10n === 0n && exponent < MAX_SAFE_EXPONENT) {
    n /= 10n
    exponent++
  }
  if (n !== BigInt.asIntN(64, n) || Math.abs(exponent) > MAX_SAFE_EXPONENT)
    throw new Error('Amount outside the exact decimal range.')
  return { coefficient: String(n), exponent }
}

function record(v: unknown): v is Record<string, unknown> {
  return !!v && typeof v === 'object' && !Array.isArray(v)
}
function texts(v: Record<string, unknown>, keys: string[]) {
  return keys.every((k) => typeof v[k] === 'string' && v[k].length <= 128)
}
function levels(v: unknown) {
  return (
    Array.isArray(v) &&
    v.length > 0 &&
    v.length <= 100 &&
    v.every(
      (row) =>
        Array.isArray(row) && row.length === 2 && row.every((x) => typeof x === 'string' && x.length <= 128),
    )
  )
}
export function isDepthInput(v: unknown): v is DepthInput {
  return (
    record(v) &&
    texts(v, ['instrument', 'cutoff', 'quantity', 'rebalanceCost', 'sellFillBps', 'adverseMoveBps']) &&
    (v.oneLegLossLimit === undefined || texts(v, ['oneLegLossLimit'])) &&
    isTimestamp(v.cutoff) &&
    Array.isArray(v.venues) &&
    v.venues.length === 2 &&
    v.venues.every(
      (x) =>
        record(x) &&
        texts(x, [
          'venue',
          'account',
          'symbol',
          'observed',
          'received',
          'sequence',
          'baseAssetRef',
          'quoteAssetRef',
          'productType',
          'baseBalance',
          'quoteBalance',
          'step',
          'minQty',
          'minNotional',
          'feeBps',
          'feeCurrency',
        ]) &&
        ((x.sellFeeBps === undefined && x.sellFeeCurrency === undefined) ||
          texts(x, ['sellFeeBps', 'sellFeeCurrency'])) &&
        (x.rebalanceCostAfterBuy === undefined || v.rebalanceCost === '') &&
        ['rebalanceCostAfterBuy', 'maxQty', 'limitCurrency', 'limitReferencePrice', 'minMarketValue', 'maxMarketValue',
          'buyPriceMin', 'buyPriceMax', 'sellPriceMin', 'sellPriceMax']
          .every((key) => x[key] === undefined || texts(x, [key])) &&
        (x.ruleEvidence === undefined || (Array.isArray(x.ruleEvidence) && x.ruleEvidence.length <= 8 &&
          x.ruleEvidence.every((r) => record(r) && typeof r.source === 'string' && !!r.source.trim() &&
            r.source.length <= 256 && isTimestamp(r.known_at) && typeof r.response_sha256 === 'string' &&
            /^[0-9a-f]{64}$/i.test(r.response_sha256)))) &&
        isTimestamp(x.observed) &&
        isTimestamp(x.received) &&
        /^\d{1,20}$/.test(String(x.sequence)) &&
        levels(x.bids) &&
        levels(x.asks),
    )
  )
}

export function separateSellFees(venues: VenueInput[]) {
  for (const v of venues) {
    v.sellFeeBps ??= v.feeBps
    v.sellFeeCurrency ??= v.feeCurrency
  }
}

function feeRequest(v: VenueInput) {
  const sellBps = v.sellFeeBps ?? v.feeBps
  const sellCurrency = v.sellFeeCurrency ?? v.feeCurrency
  // Preserve the original protobuf fingerprint when replaying a symmetric schedule.
  if (sellBps === v.feeBps && sellCurrency === v.feeCurrency)
    return { fee_bps: decimal(v.feeBps), fee_currency: v.feeCurrency }
  return {
    buy_taker_fee: { bps: decimal(v.feeBps), currency: v.feeCurrency },
    sell_taker_fee: { bps: decimal(sellBps), currency: sellCurrency },
  }
}

export function spotRequest(s: DepthInput) {
  if (s.rebalanceCost !== '' && s.venues.some(v => v.rebalanceCostAfterBuy !== undefined))
    throw new Error('Choose shared or directional inventory restoration costs.')
  const priceRange = (min?: string, max?: string) =>
    min || max ? { minimum: decimal(min ?? ''), maximum: decimal(max ?? '') } : undefined
  const bookSide = (side: [string, string][]) =>
    side.map(([price, size]) => ({ price: decimal(price), size: decimal(size) }))
  return {
    scenario_name: 'OPERATOR_SPOT_DEPTH',
    spot_depth: {
      as_of: s.cutoff,
      requested_buy_quantity: decimal(s.quantity),
      rebalance_cost: decimal(s.rebalanceCost),
      adverse_move_bps: decimal(s.adverseMoveBps),
      one_leg_loss_limit: decimal(s.oneLegLossLimit ?? ''),
      sell_fill_bps: decimal(s.sellFillBps),
      venues: s.venues.map((v) => ({
        account_label: v.account,
        known_at: v.received,
        product_type: v.productType,
        base_asset_ref: v.baseAssetRef,
        quote_asset_ref: v.quoteAssetRef,
        rebalance_cost_after_buy: decimal(v.rebalanceCostAfterBuy ?? ''),
        available_base: decimal(v.baseBalance),
        available_quote: decimal(v.quoteBalance),
        quantity_step: decimal(v.step),
        minimum_quantity: decimal(v.minQty),
        minimum_notional: decimal(v.minNotional),
        maximum_quantity: decimal(v.maxQty ?? ''),
        buy_price_range: priceRange(v.buyPriceMin, v.buyPriceMax),
        sell_price_range: priceRange(v.sellPriceMin, v.sellPriceMax),
        rule_evidence: v.ruleEvidence,
        market_value_limit: v.minMarketValue || v.maxMarketValue || v.limitReferencePrice || v.limitCurrency ? {
          currency: v.limitCurrency ?? '',
          reference_price: decimal(v.limitReferencePrice ?? ''),
          minimum_value: decimal(v.minMarketValue ?? ''),
          maximum_value: decimal(v.maxMarketValue ?? ''),
        } : undefined,
        ...feeRequest(v),
        book: {
          mic: v.venue,
          instrument_id: s.instrument,
          symbol: v.symbol,
          event_time: v.observed,
          last_update_sequence: v.sequence,
          bids: bookSide(v.bids),
          asks: bookSide(v.asks),
        },
      })),
    },
  }
}

interface Stress {
  inventory_change: Decimal
  cash_change: Decimal
  mark_price: Decimal
  marked_pnl: Decimal
}
interface UnwindLeg {
  quantity: Decimal
  notional: Decimal
  fee: { amount: Decimal; currency_code: string }
  change_from_start: { base_change: Decimal; quote_change: Decimal }
}
type LocalUnwind =
  | { status: 'SUPPORTED'; sell_back: UnwindLeg; buy_back: UnwindLeg; extra_quote_required: Decimal;
      net_quote_cash: Decimal; marked_pnl: Decimal; cost_vs_entry_mark: Decimal }
  | { status: 'UNKNOWN' | 'UNSUPPORTED'; reason: string; extra_quote_required?: Decimal }
export interface Direction {
  buy_venue: string
  sell_venue: string
  status: string
  reason: string
  buy_quantity?: Decimal
  sell_quantity?: Decimal
  shortfall?: Decimal
  buy_notional?: Decimal
  sell_notional?: Decimal
  buy_fee?: Money
  sell_fee?: Money
  cash_change?: Decimal
  residual_base?: Decimal
  marked_net_after_rebalance?: Decimal
  applied_rebalance_cost?: Decimal
  marked_net_before_rebalance?: Decimal
  local_unwind?: LocalUnwind
  gross_quote_cash_change?: Decimal
  total_fees_in_quote?: Decimal
  buy_venue_change?: { base_change: Decimal; quote_change: Decimal }
  sell_venue_change?: { base_change: Decimal; quote_change: Decimal }
  buy_only?: Stress
  sell_only?: Stress
  partial_sell?: Stress
  funding?: {
    buy_quote_required: Decimal
    sell_base_required: Decimal
    sell_base_value_quote?: Decimal
    funded_capital_quote?: Decimal
    gross_turnover_quote?: Decimal
    return_on_capital_bps?: Decimal
    break_even_cost_per_leg_bps?: Decimal
  }
  worst_one_leg_loss?: Decimal
  one_leg_limit_status?: 'WITHIN' | 'EXCEEDED' | 'UNKNOWN'
}
interface Result {
  instrument_id: string
  base_currency: string
  quote_currency: string
  input_sha256: string
  as_of: string
  directions: Direction[]
}
function wireAmount(v: unknown): boolean {
  return (
    record(v) &&
    typeof v.coefficient === 'string' &&
    /^-?\d{1,19}$/.test(v.coefficient) &&
    BigInt(v.coefficient) === BigInt.asIntN(64, BigInt(v.coefficient)) &&
    typeof v.exponent === 'number' &&
    Number.isInteger(v.exponent) &&
    Math.abs(v.exponent) <= MAX_SAFE_EXPONENT
  )
}
function validDirection(v: unknown, buy: VenueInput, sell: VenueInput, hasLossLimit: boolean, sharedCost: string): boolean {
  if (!record(v) || v.buy_venue !== buy.venue || v.sell_venue !== sell.venue || typeof v.reason !== 'string')
    return false
  if (v.status === 'UNKNOWN') return v.reason.length > 0 && v.local_unwind === undefined
  if (v.status === 'UNSUPPORTED') return v.reason.length > 0 && wireAmount(v.shortfall) && v.local_unwind === undefined
  const expectedCost = decimal(buy.rebalanceCostAfterBuy ?? sharedCost)
  if (v.applied_rebalance_cost !== undefined) {
    const applied = v.applied_rebalance_cost
    if (!expectedCost || !record(applied) || !wireAmount(applied) ||
        applied.coefficient !== expectedCost.coefficient || applied.exponent !== expectedCost.exponent) return false
  } else if (buy.rebalanceCostAfterBuy) return false
  if (!expectedCost && v.marked_net_after_rebalance !== undefined) return false
  return (
    v.status === 'SUPPORTED' &&
    [
      'buy_quantity',
      'sell_quantity',
      'shortfall',
      'buy_notional',
      'sell_notional',
      'cash_change',
      'residual_base',
      'gross_quote_cash_change',
      'total_fees_in_quote',
    ].every((k) => wireAmount(v[k])) &&
    ['buy_venue_change', 'sell_venue_change'].every((key) => {
      const change = v[key]
      return record(change) && wireAmount(change.base_change) && wireAmount(change.quote_change)
    }) &&
    (
      [
        ['buy_fee', buy.feeCurrency],
        ['sell_fee', sell.sellFeeCurrency ?? sell.feeCurrency],
      ] as const
    ).every(([key, currency]) => {
      const fee = v[key]
      return record(fee) && fee.currency_code === currency && wireAmount(fee.amount)
    }) &&
    (v.marked_net_after_rebalance === undefined || wireAmount(v.marked_net_after_rebalance)) &&
    (v.marked_net_before_rebalance === undefined || wireAmount(v.marked_net_before_rebalance)) &&
    validFunding(v, hasLossLimit) && validLocalUnwind(v.local_unwind, buy, sell) &&
    ['buy_only', 'sell_only', 'partial_sell'].every((k) => {
      const stress = v[k]
      return (
        stress === undefined ||
        (record(stress) &&
          ['inventory_change', 'cash_change', 'mark_price', 'marked_pnl'].every((f) => wireAmount(stress[f])))
      )
    })
  )
}

function validLocalUnwind(value: unknown, buy: VenueInput, sell: VenueInput): boolean {
  if (value === undefined) return true
  if (!record(value) || (value.extra_quote_required !== undefined && !wireAmount(value.extra_quote_required)))
    return false
  if (value.status === 'UNKNOWN' || value.status === 'UNSUPPORTED')
    return typeof value.reason === 'string' && value.reason.length > 0 &&
      ['sell_back', 'buy_back', 'marked_pnl', 'net_quote_cash', 'cost_vs_entry_mark'].every(k => value[k] === undefined)
  return value.status === 'SUPPORTED' &&
    ['extra_quote_required', 'net_quote_cash', 'marked_pnl', 'cost_vs_entry_mark'].every(k => wireAmount(value[k])) &&
    ([[value.sell_back, buy.sellFeeCurrency ?? buy.feeCurrency], [value.buy_back, sell.feeCurrency]] as const)
      .every(([leg, currency]) => record(leg) && wireAmount(leg.quantity) && wireAmount(leg.notional) &&
        record(leg.fee) && leg.fee.currency_code === currency && wireAmount(leg.fee.amount) &&
        record(leg.change_from_start) && wireAmount(leg.change_from_start.base_change) && wireAmount(leg.change_from_start.quote_change))
}

function validFunding(v: Record<string, unknown>, hasLossLimit: boolean): boolean {
  if (v.funding === undefined)
    return v.one_leg_limit_status === undefined && v.worst_one_leg_loss === undefined
  const f = v.funding
  if (!record(f) || !wireAmount(f.buy_quote_required) || !wireAmount(f.sell_base_required)) return false
  for (const key of ['sell_base_value_quote', 'funded_capital_quote', 'gross_turnover_quote'])
    if (f[key] !== undefined && !wireAmount(f[key])) return false
  for (const [rate, denominator] of [
    ['return_on_capital_bps', 'funded_capital_quote'], ['break_even_cost_per_leg_bps', 'gross_turnover_quote'],
  ] as const) {
    if (f[rate] !== undefined &&
        (!wireAmount(f[rate]) || f[denominator] === undefined || v.marked_net_after_rebalance === undefined)) return false
  }
  if (v.worst_one_leg_loss !== undefined &&
      (!wireAmount(v.worst_one_leg_loss) || v.buy_only === undefined || v.sell_only === undefined)) return false
  return v.one_leg_limit_status === 'UNKNOWN' ||
    (hasLossLimit && v.worst_one_leg_loss !== undefined &&
      (v.one_leg_limit_status === 'WITHIN' || v.one_leg_limit_status === 'EXCEEDED'))
}
// Date.parse gives the absolute millisecond, including the UTC offset. Compare
// the remaining digits as well so distinct book cutoffs cannot match.
function timestampNanos(value: string): bigint {
  const submillis = (value.match(/\.(\d+)/)?.[1] ?? '').padEnd(9, '0').slice(3)
  return BigInt(Date.parse(value)) * 1_000_000n + BigInt(submillis)
}
// Both endpoints come from the captured observation. The browser clock and
// time spent viewing a historical capture do not affect these intervals.
export function elapsedMillis(start: string, end: string): string {
  if (!isTimestamp(start) || !isTimestamp(end)) return 'Unknown'
  const nanos = timestampNanos(end) - timestampNanos(start)
  if (nanos < 0n) return 'Unknown'
  if (nanos > 0n && nanos < 100_000n) return '<0.1 ms'
  const tenths = (nanos + 50_000n) / 100_000n
  const fraction = tenths % 10n
  return `${tenths / 10n}${fraction ? '.' + fraction : ''} ms`
}
function sameCutoff(reply: string, requested: string): boolean {
  return isTimestamp(requested) && timestampNanos(reply) === timestampNanos(requested)
}

type BookCheckState = 'CURRENT' | 'SUPERSEDED' | 'STALE' | 'UNVERIFIED'
interface BookCheck {
  state: BookCheckState
  max_age_ms: number
  checked_at?: string
}

export async function priceDepth(portfolio: string, tenant: string, input: DepthInput, signal: AbortSignal, maxBookAgeMs?: number) {
  if (maxBookAgeMs !== undefined && (!Number.isInteger(maxBookAgeMs) || maxBookAgeMs < 1 || maxBookAgeMs > 2000 ||
      input.observation?.portfolio_id !== portfolio || input.observation.owner_tenant !== tenant ||
      input.observation.event_ids.length !== 2)) throw new Error('Select a captured pair and a book age limit of 1–2000 ms.')
  const raw = await api.post<unknown>(
    '/api/v1/portfolios/' + encodeURIComponent(portfolio) + '/scenario',
    { ...spotRequest(input), ...(maxBookAgeMs !== undefined && { spot_book_check: {
      event_ids: input.observation!.event_ids, max_age_ms: maxBookAgeMs,
    } }) },
    200,
    signal,
  )
  const r = record(raw) ? raw.spot_depth : null
  if (
    !record(raw) ||
    raw.portfolio_id !== portfolio ||
    raw.owner_tenant !== tenant ||
    !record(r) ||
    r.instrument_id !== input.instrument ||
    typeof r.input_sha256 !== 'string' ||
    !/^[a-f0-9]{64}$/.test(r.input_sha256) ||
    !isTimestamp(r.as_of) ||
    !sameCutoff(r.as_of, input.cutoff) ||
    typeof r.base_currency !== 'string' ||
    !r.base_currency ||
    typeof r.quote_currency !== 'string' ||
    !r.quote_currency ||
    !input.venues.some((v) => v.venue === 'OKX' && v.symbol === r.base_currency + '-' + r.quote_currency) ||
    !Array.isArray(r.directions) ||
    r.directions.length !== 2 ||
    !r.directions.every((v, i) => validDirection(v, input.venues[i]!, input.venues[1 - i]!, !!input.oneLegLossLimit, input.rebalanceCost))
  )
    throw new Error('The portfolio scenario response could not be verified.')
  const timing = raw.spot_depth_timing
  const serviceTiming = record(timing) && isTimestamp(timing.started_at) && isTimestamp(timing.completed_at) &&
    timestampNanos(timing.completed_at) >= timestampNanos(timing.started_at)
    ? { started_at: timing.started_at, completed_at: timing.completed_at }
    : undefined
  let bookCheck: BookCheck | undefined
  if (maxBookAgeMs !== undefined) {
    bookCheck = { state: 'UNVERIFIED', max_age_ms: maxBookAgeMs }
    const check = raw.spot_book_check
    if (serviceTiming && record(check) && check.max_age_ms === maxBookAgeMs && isTimestamp(check.checked_at) &&
        timestampNanos(check.checked_at) >= timestampNanos(serviceTiming.completed_at) &&
        timestampNanos(serviceTiming.started_at) >= timestampNanos(input.cutoff)) {
      const at = timestampNanos(check.checked_at)
      bookCheck.checked_at = check.checked_at
      if (check.state === 'SPOT_BOOK_CHECK_STATE_CURRENT' && input.venues.every(v => {
        const age = at - timestampNanos(v.observed)
        return age >= 0n && age <= BigInt(maxBookAgeMs) * 1_000_000n
      })) bookCheck.state = 'CURRENT'
      if (check.state === 'SPOT_BOOK_CHECK_STATE_SUPERSEDED') bookCheck.state = 'SUPERSEDED'
      if (check.state === 'SPOT_BOOK_CHECK_STATE_STALE') bookCheck.state = 'STALE'
    }
  }
  return { result: r as unknown as Result, serviceTiming, bookCheck }
}
export function amount(value: Decimal | undefined, unit: string) {
  const s = formatDecimal(value)
  return s === null ? 'Unknown' : s + ' ' + unit
}

export function fundingRate(value: Decimal | undefined, net: Decimal | undefined) {
  if (value?.coefficient !== undefined && net?.coefficient !== undefined &&
      BigInt(value.coefficient) === 0n && BigInt(net.coefficient) !== 0n)
    return BigInt(net.coefficient) < 0n ? 'Between −0.0000005 and 0 bps' : 'Between 0 and 0.0000005 bps'
  return amount(value, 'bps')
}

function capturedSide(rows: unknown): [string, string][] {
  if (!Array.isArray(rows) || rows.length === 0 || rows.length > 100) throw new Error('Invalid observed depth.')
  return rows.map((row) => {
    if (!record(row) || !wireAmount(row.price) || !wireAmount(row.size)) throw new Error('Invalid observed level.')
    return [formatDecimal(row.price as Decimal)!, formatDecimal(row.size as Decimal)!]
  })
}

export async function captureDepth(
  portfolio: string,
  tenant: string,
  instrument: string,
  signal: AbortSignal,
  includeDemoFunds = false,
  waitForBooks = false,
  maxAgeMs?: number,
): Promise<DepthInput> {
  if (maxAgeMs !== undefined && (!Number.isInteger(maxAgeMs) || maxAgeMs < 1 || maxAgeMs > 2000))
    throw new Error('Maximum book age must be an integer from 1 to 2000 ms.')
  const path = '/api/v1/portfolios/' + encodeURIComponent(portfolio)
  // Private reads must already be known when the risk service freezes the books.
  const funds = includeDemoFunds
    ? await Promise.allSettled(demoSpotVenues.map(venue => readDemoAccountTerms(portfolio, tenant, instrument, venue, signal)))
    : []
  const raw = await api.post<unknown>(
    path + '/scenario',
    { scenario_name: 'OPERATOR_SPOT_BOOKS', spot_book_instrument: instrument, spot_book_capture_max_age_ms: maxAgeMs, ...(waitForBooks && { wait_for_spot_books: true }) },
    200,
    signal,
  )
  if (
    !record(raw) ||
    raw.portfolio_id !== portfolio ||
    raw.owner_tenant !== tenant ||
    !record(raw.spot_books) ||
    raw.spot_books.instrument_id !== instrument
  )
    throw new Error('Could not verify book ownership.')
  const o = raw.spot_books
  if (o.status === 'UNKNOWN') {
    const venueRefusal = ['SPOT_BOOK_REFUSAL_MISSING', 'SPOT_BOOK_REFUSAL_INVALID', 'SPOT_BOOK_REFUSAL_STALE'].includes(String(o.refusal))
    if (((venueRefusal && (o.refusal_venue === 'BINANCE' || o.refusal_venue === 'OKX')) || o.refusal === 'SPOT_BOOK_REFUSAL_SKEW') &&
        isTimestamp(o.as_of) && typeof o.reason === 'string' && o.reason.length > 0 && o.reason.length <= 160)
      throw new Error(o.reason)
    throw new Error('No authorized current pair of books is available.')
  }
  if (
    o.status !== 'SUPPORTED' ||
    o.product_type !== 'CRYPTO_SPOT' ||
    !isTimestamp(o.as_of) ||
    !Array.isArray(o.venues) ||
    o.venues.length !== 2 ||
    ![o.source, o.producer_version, o.rights_reference, o.base_asset_ref, o.quote_asset_ref].every(
      (v) => typeof v === 'string' && v.length > 0,
    ) ||
    o.base_asset_ref === o.quote_asset_ref
  )
    throw new Error('The observed book reference is incomplete.')
  const ids: string[] = []
  const venues = o.venues.map((entry, i) => {
    if (
      !record(entry) ||
      !record(entry.book) ||
      typeof entry.event_id !== 'string' ||
      !entry.event_id ||
      !isTimestamp(entry.known_at)
    )
      throw new Error('Missing venue observation.')
    const b = entry.book,
      venue = ['BINANCE', 'OKX'][i]!
    if (
      b.instrument_id !== instrument ||
      b.mic !== venue ||
      typeof b.symbol !== 'string' ||
      !isTimestamp(b.event_time) ||
      typeof b.last_update_sequence !== 'string' ||
      !/^[1-9]\d{0,19}$/.test(b.last_update_sequence)
    )
      throw new Error('Invalid venue observation.')
    if (timestampNanos(b.event_time) > timestampNanos(entry.known_at) ||
        timestampNanos(entry.known_at) > timestampNanos(o.as_of as string))
      throw new Error('The venue event, risk receipt and capture times are out of order.')
    if (maxAgeMs !== undefined && timestampNanos(o.as_of as string) - timestampNanos(b.event_time) > BigInt(maxAgeMs) * 1_000_000n)
      throw new Error(venue + ' book exceeds the requested capture age of ' + maxAgeMs + ' ms.')
    ids.push(entry.event_id)
    const account = entry.portfolio_account_ref ?? ''
    if (typeof account !== 'string' || account.trim() !== account) throw new Error('Invalid portfolio account reference.')
    const routingMic = entry.portfolio_account_mic ?? ''
    if (typeof routingMic !== 'string') throw new Error('Invalid portfolio routing MIC.')
    const input: VenueInput = {
      venue,
      symbol: b.symbol,
      productType: 'CRYPTO_SPOT',
      baseAssetRef: o.base_asset_ref as string,
      quoteAssetRef: o.quote_asset_ref as string,
      observed: b.event_time,
      received: entry.known_at,
      sequence: b.last_update_sequence,
      bids: capturedSide(b.bids),
      asks: capturedSide(b.asks),
      account,
      portfolioAccount: account,
      portfolioAccountMic: routingMic,
      baseBalance: '',
      quoteBalance: '',
      feeBps: '',
      feeCurrency: '',
      sellFeeBps: '',
      sellFeeCurrency: '',
      step: '',
      minQty: '',
      minNotional: '',
    }
    const read = funds[i]
    if (read?.status === 'fulfilled') applyCapturedFunds(input, read.value, o.as_of as string)
    return input
  })
  return {
    instrument,
    cutoff: o.as_of,
    quantity: '',
    rebalanceCost: '',
    sellFillBps: '',
    adverseMoveBps: '',
    venues,
    observation: {
      portfolio_id: portfolio,
      owner_tenant: tenant,
      source: o.source as string,
      producer_version: o.producer_version as string,
      rights_reference: o.rights_reference as string,
      event_ids: ids,
      known_at_clock: 'risk_service_receipt',
    },
  }
}

export const demoSpotVenues = ['BINANCE', 'OKX'] as const
export type DemoSpotVenue = typeof demoSpotVenues[number]

export interface DemoAccountTerms {
  venue: DemoSpotVenue
  account: string
  mic: string
  symbol: string
  base: string
  quote: string
  funds: NonNullable<VenueInput['fundsObservation']>
  fees?: NonNullable<VenueInput['feeObservation']>
}

export async function readDemoAccountTerms(portfolio: string, tenant: string, instrument: string, venue: DemoSpotVenue,
  signal: AbortSignal): Promise<DemoAccountTerms> {
  const path = '/api/v1/portfolios/' + encodeURIComponent(portfolio) + '/demo-spot-funds/' + venue
  const raw = await api.get<unknown>(path + '?instrument=' + encodeURIComponent(instrument) + '&taker_fees=true', { signal, expectedStatus: 200 })
  const invalid = 'Could not verify the portfolio account observation.'
  if (!record(raw) || raw.owner_tenant !== tenant || raw.portfolio_id !== portfolio ||
      raw.market_venue !== venue || !record(raw.funds)) throw new Error(invalid)
  const f = raw.funds
  if (f.owner_tenant !== tenant || typeof f.venue_account_id !== 'string' || !f.venue_account_id.trim() ||
      typeof f.mic !== 'string' || (f.mic !== venue && f.mic !== (venue === 'BINANCE' ? 'XBIN' : 'XOKX')) ||
      typeof f.base_currency !== 'string' || !f.base_currency || typeof f.quote_currency !== 'string' ||
      !f.quote_currency || f.base_currency === f.quote_currency || f.base_currency + '-' + f.quote_currency !== instrument ||
      f.instrument_id !== instrument || typeof f.venue_symbol !== 'string' || f.venue_symbol !== (venue === 'OKX' ? instrument : f.base_currency + f.quote_currency) ||
      !isTimestamp(f.request_started_at) || !isTimestamp(f.received_at) ||
      !wireAmount(f.available_base) || !wireAmount(f.available_quote)) throw new Error(invalid)
  const start = timestampNanos(f.request_started_at), end = timestampNanos(f.received_at)
  if (end < start || end - start > 6_000_000_000n) throw new Error(invalid)
  const base = formatDecimal(f.available_base as Decimal), quote = formatDecimal(f.available_quote as Decimal)
  if (base === null || quote === null || base.startsWith('-') || quote.startsWith('-')) throw new Error(invalid)
  const observation: DemoAccountTerms = {
    venue, account: f.venue_account_id, mic: f.mic, symbol: f.venue_symbol, base: f.base_currency, quote: f.quote_currency,
    funds: { request_started_at: f.request_started_at, received_at: f.received_at, available_base: base, available_quote: quote,
      source: venue === 'BINANCE' ? 'Binance Spot Testnet account' : 'OKX Demo spot cash capacity' },
  }
  const schedule = f.taker_schedule
  if (!record(schedule) || !isTimestamp(schedule.request_started_at) || !isTimestamp(schedule.received_at)) return observation
  const feeStart = timestampNanos(schedule.request_started_at), feeEnd = timestampNanos(schedule.received_at)
  if (feeStart < end || feeEnd < feeStart || feeEnd - start > 6_000_000_000n) return observation
  const side = (raw: unknown) => {
    if (!record(raw) || typeof raw.currency !== 'string' ||
        (raw.currency !== f.base_currency && raw.currency !== f.quote_currency) || !wireAmount(raw.bps)) return
    const bps = formatDecimal(raw.bps as Decimal)
    if (bps === null || bps.replace('-', '').split('.')[0]!.length > 4) return
    return { bps, currency: raw.currency }
  }
  const buy = side(schedule.buy), sell = side(schedule.sell)
  if (!buy || !sell) return observation
  observation.fees = {
    request_started_at: schedule.request_started_at, received_at: schedule.received_at,
    buyBps: buy.bps, buyCurrency: buy.currency, sellBps: sell.bps, sellCurrency: sell.currency,
    source: venue === 'BINANCE' ? 'Binance account commission' : 'OKX account fee group',
  }
  return observation
}

function applyCapturedFunds(v: VenueInput, observation: DemoAccountTerms, cutoff: string) {
  const { funds, fees } = observation
  const at = timestampNanos(cutoff)
  if (observation.venue !== v.venue || observation.symbol !== v.symbol ||
      (v.portfolioAccount && observation.account !== v.portfolioAccount) ||
      (v.portfolioAccountMic && observation.mic !== v.portfolioAccountMic) ||
      timestampNanos(funds.received_at) > at || at - timestampNanos(funds.request_started_at) > 6_000_000_000n) return
  v.portfolioAccount = v.account = observation.account
  v.portfolioAccountMic = observation.mic
  v.baseBalance = funds.available_base
  v.quoteBalance = funds.available_quote
  v.fundsObservation = funds
  if (!fees || timestampNanos(fees.received_at) > at) return
  v.feeObservation = fees
  // OKX omits zero-fee campaigns from this API. Its group rate may be
  // selected as an assumption, never automatically as the applicable fee.
  if (v.venue === 'BINANCE') useScheduledFees(v)
}

export function useScheduledFees(v: VenueInput) {
  const fees = v.feeObservation
  if (!fees) return
  v.feeBps = fees.buyBps
  v.feeCurrency = fees.buyCurrency
  v.sellFeeBps = fees.sellBps
  v.sellFeeCurrency = fees.sellCurrency
}
