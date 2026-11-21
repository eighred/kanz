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
        ['maxQty', 'limitCurrency', 'limitReferencePrice', 'minMarketValue', 'maxMarketValue']
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
  const bookSide = (side: [string, string][]) =>
    side.map(([price, size]) => ({ price: decimal(price), size: decimal(size) }))
  return {
    scenario_name: 'OPERATOR_SPOT_DEPTH',
    spot_depth: {
      as_of: s.cutoff,
      requested_buy_quantity: decimal(s.quantity),
      rebalance_cost: decimal(s.rebalanceCost),
      adverse_move_bps: decimal(s.adverseMoveBps),
      sell_fill_bps: decimal(s.sellFillBps),
      venues: s.venues.map((v) => ({
        account_label: v.account,
        known_at: v.received,
        product_type: v.productType,
        base_asset_ref: v.baseAssetRef,
        quote_asset_ref: v.quoteAssetRef,
        available_base: decimal(v.baseBalance),
        available_quote: decimal(v.quoteBalance),
        quantity_step: decimal(v.step),
        minimum_quantity: decimal(v.minQty),
        minimum_notional: decimal(v.minNotional),
        maximum_quantity: decimal(v.maxQty ?? ''),
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
  gross_quote_cash_change?: Decimal
  total_fees_in_quote?: Decimal
  buy_venue_change?: { base_change: Decimal; quote_change: Decimal }
  sell_venue_change?: { base_change: Decimal; quote_change: Decimal }
  buy_only?: Stress
  sell_only?: Stress
  partial_sell?: Stress
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
function validDirection(v: unknown, buy: VenueInput, sell: VenueInput): boolean {
  if (!record(v) || v.buy_venue !== buy.venue || v.sell_venue !== sell.venue || typeof v.reason !== 'string')
    return false
  if (v.status === 'UNKNOWN') return v.reason.length > 0
  if (v.status === 'UNSUPPORTED') return v.reason.length > 0 && wireAmount(v.shortfall)
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
// Date.parse gives the absolute millisecond, including the UTC offset. Compare
// the remaining digits as well so distinct book cutoffs cannot match.
function timestampNanos(value: string): bigint {
  const submillis = (value.match(/\.(\d+)/)?.[1] ?? '').padEnd(9, '0').slice(3)
  return BigInt(Date.parse(value)) * 1_000_000n + BigInt(submillis)
}
function sameCutoff(reply: string, requested: string): boolean {
  return isTimestamp(requested) && timestampNanos(reply) === timestampNanos(requested)
}

export async function priceDepth(portfolio: string, tenant: string, input: DepthInput, signal: AbortSignal) {
  const raw = await api.post<unknown>(
    '/api/v1/portfolios/' + encodeURIComponent(portfolio) + '/scenario',
    spotRequest(input),
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
    !r.directions.every((v, i) => validDirection(v, input.venues[i]!, input.venues[1 - i]!))
  )
    throw new Error('The portfolio scenario response could not be verified.')
  return r as unknown as Result
}
export function amount(value: Decimal | undefined, unit: string) {
  const s = formatDecimal(value)
  return s === null ? 'Unknown' : s + ' ' + unit
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
): Promise<DepthInput> {
  const path = '/api/v1/portfolios/' + encodeURIComponent(portfolio)
  // Private reads must already be known when the risk service freezes the books.
  const funds = includeDemoFunds
    ? await Promise.allSettled(['BINANCE', 'OKX'].map((mic) =>
        api.get<unknown>(path + '/demo-spot-funds/' + mic + '?instrument=' + encodeURIComponent(instrument) + '&taker_fees=true', { signal }),
      ))
    : []
  const raw = await api.post<unknown>(
    path + '/scenario',
    { scenario_name: 'OPERATOR_SPOT_BOOKS', spot_book_instrument: instrument },
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
  if (o.status === 'UNKNOWN') throw new Error('No authorized current pair of books is available.')
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
    if (read?.status === 'fulfilled')
      applyCapturedFunds(input, read.value, portfolio, tenant, instrument, o.as_of as string)
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

function applyCapturedFunds(v: VenueInput, raw: unknown, portfolio: string, tenant: string, instrument: string, cutoff: string) {
  if (!record(raw) || raw.owner_tenant !== tenant || raw.portfolio_id !== portfolio ||
      raw.market_venue !== v.venue || !record(raw.funds)) return
  const f = raw.funds
  if (f.owner_tenant !== tenant || typeof f.venue_account_id !== 'string' || !f.venue_account_id.trim() ||
      (v.portfolioAccount && f.venue_account_id !== v.portfolioAccount) ||
      (f.mic !== v.venue && f.mic !== (v.venue === 'BINANCE' ? 'XBIN' : 'XOKX')) ||
      (v.portfolioAccountMic && v.portfolioAccountMic !== f.mic) ||
      f.venue_symbol !== v.symbol || f.instrument_id !== instrument ||
      typeof f.base_currency !== 'string' || typeof f.quote_currency !== 'string' ||
      f.base_currency + '-' + f.quote_currency !== instrument ||
      !isTimestamp(f.request_started_at) || !isTimestamp(f.received_at) ||
      !wireAmount(f.available_base) || !wireAmount(f.available_quote)) return
  const start = timestampNanos(f.request_started_at), end = timestampNanos(f.received_at), at = timestampNanos(cutoff)
  // The exchange may have observed funds any time after the signed read began.
  if (end < start || at < end || at - start > 6_000_000_000n) return
  const base = formatDecimal(f.available_base as Decimal), quote = formatDecimal(f.available_quote as Decimal)
  if (base === null || quote === null || base.startsWith('-') || quote.startsWith('-')) return
  v.portfolioAccount = v.account = f.venue_account_id
  v.portfolioAccountMic = f.mic as string
  v.baseBalance = base
  v.quoteBalance = quote
  v.fundsObservation = {
    request_started_at: f.request_started_at, received_at: f.received_at,
    available_base: base, available_quote: quote,
    source: v.venue === 'BINANCE' ? 'Binance Spot Testnet account' : 'OKX Demo spot cash capacity',
  }
  const schedule = f.taker_schedule
  if (!record(schedule) || !isTimestamp(schedule.request_started_at) || !isTimestamp(schedule.received_at)) return
  const feeStart = timestampNanos(schedule.request_started_at), feeEnd = timestampNanos(schedule.received_at)
  if (feeStart < end || feeEnd < feeStart || feeEnd > at) return
  const side = (raw: unknown) => {
    if (!record(raw) || typeof raw.currency !== 'string' ||
        (raw.currency !== f.base_currency && raw.currency !== f.quote_currency) || !wireAmount(raw.bps)) return
    const bps = formatDecimal(raw.bps as Decimal)
    if (bps === null || bps.replace('-', '').split('.')[0]!.length > 4) return
    return { bps, currency: raw.currency }
  }
  const buy = side(schedule.buy), sell = side(schedule.sell)
  if (!buy || !sell) return
  v.feeObservation = {
    request_started_at: schedule.request_started_at, received_at: schedule.received_at,
    buyBps: buy.bps, buyCurrency: buy.currency, sellBps: sell.bps, sellCurrency: sell.currency,
    source: v.venue === 'BINANCE' ? 'Binance account commission' : 'OKX account fee group',
  }
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
