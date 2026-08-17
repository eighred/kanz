import { api, ApiError } from './client'
import type { Decimal, Money } from './decimal'

// The risk read surface, as the browser sees it (#399).
//
// A SEPARATE MODULE FROM control.ts, AND NOT BY ACCIDENT. The two halves of /v1
// serialise differently and control.ts says so in its own header: the control
// routes are protojson with default options — lowerCamelCase, zero values
// OMITTED — while these use UseProtoNames + EmitDefaultValues, so they are
// snake_case with zeros PRESENT.
//
// That difference changes how a field must be read. In control.ts every
// optional is genuinely optional and `?? false` is required rather than
// defensive; here a zero arrives as an explicit 0 or "". Sharing one set of
// types across both would make one of those readings wrong somewhere, silently.

/** PortfolioSummary is one row of GET /v1/portfolios. */
export interface PortfolioSummary {
  portfolio_id: string
  display_name: string
  base_currency: string
  /** RFC3339. The state timestamp this portfolio was last folded to. */
  as_of: string
  position_count: number
}

/** ExposureDimension is the axis risk is decomposed along, as its enum name. */
export type ExposureDimension =
  | 'EXPOSURE_DIMENSION_UNSPECIFIED'
  | 'EXPOSURE_DIMENSION_ASSET_CLASS'
  | 'EXPOSURE_DIMENSION_SECTOR'
  | 'EXPOSURE_DIMENSION_CURRENCY'
  | 'EXPOSURE_DIMENSION_RISK_FACTOR'
  | 'EXPOSURE_DIMENSION_ISSUER'
  | 'EXPOSURE_DIMENSION_INSTRUMENT'

export interface ExposureState {
  dimension: ExposureDimension
  bucket: string
  gross?: Money
  net?: Money
}

/**
 * QualityFlag is a query-result concern, and two of the four change what the
 * numbers MEAN rather than how fresh they are.
 *
 * CURRENCY_EXCLUDED and INPUTS_UNRESOLVED are the two to respect, and they are
 * DIFFERENT findings. The first means the engine deliberately left positions
 * out because it has no FX layer. The second means data it expected was simply
 * not there — contract terms nobody loaded, a curve nobody calibrated — so a
 * measure may have been computed over NOTHING and its zero is not a claim about
 * the book. Only the second is somebody's bug.
 *
 * Neither says which way the number is wrong. A total goes down when positions
 * are dropped; an average or a VaR can go either way, because dropping a
 * below-average holding raises an average and dropping one leg of a hedge
 * removes its offset. A screen cannot refuse on the reader's behalf — but it
 * must not present either as a number that stands on its own.
 */
export type QualityFlag =
  | 'QUALITY_FLAG_UNSPECIFIED'
  | 'QUALITY_FLAG_DEGRADED'
  | 'QUALITY_FLAG_STALE'
  | 'QUALITY_FLAG_CURRENCY_EXCLUDED'
  | 'QUALITY_FLAG_INPUTS_UNRESOLVED'

/**
 * MEANING_CHANGING are the flags that change what the numbers ARE, as opposed
 * to how fresh they are. They get the error treatment; DEGRADED and STALE get a
 * hint. Listed once, because the alternative is every screen re-deciding — and
 * the failure mode of that is a new flag rendering as a low-severity hint
 * simply because nobody updated a comparison in a template.
 */
export const MEANING_CHANGING: readonly QualityFlag[] = [
  'QUALITY_FLAG_CURRENCY_EXCLUDED',
  'QUALITY_FLAG_INPUTS_UNRESOLVED',
]

/** changesMeaning reports whether a flag makes the number beside it unusable. */
export function changesMeaning(f: QualityFlag): boolean {
  return MEANING_CHANGING.includes(f)
}

export interface ExposureResponse {
  portfolio_id: string
  as_of: string
  set?: { exposures?: ExposureState[] }
  quality_flags?: QualityFlag[]
}

/** describeFlag says what a flag means for the number beside it. */
export function describeFlag(f: QualityFlag): string {
  switch (f) {
    case 'QUALITY_FLAG_CURRENCY_EXCLUDED':
      return 'Positions in a currency the engine could not convert are MISSING from these totals. The real exposure is larger than what is shown — do not read these as a complete book.'
    case 'QUALITY_FLAG_INPUTS_UNRESOLVED':
      return 'Data the engine needed was not there, so at least one figure was computed over part of the book — possibly over none of it. A zero here does not mean zero risk. Do not act on these numbers.'
    case 'QUALITY_FLAG_DEGRADED':
      return 'The engine was degraded when this was computed: these are cached, last-known values rather than live ones.'
    case 'QUALITY_FLAG_STALE':
      return 'This data is older than the request asked for, though the engine itself is healthy.'
    default:
      return f
  }
}

/** OrderStatus is the protobuf enum, serialised as its name. */
export type OrderStatus =
  | 'ORDER_STATUS_UNSPECIFIED'
  | 'ORDER_STATUS_PENDING_NEW'
  | 'ORDER_STATUS_ROUTED'
  | 'ORDER_STATUS_PARTIALLY_FILLED'
  | 'ORDER_STATUS_FILLED'
  | 'ORDER_STATUS_CANCELLED'
  | 'ORDER_STATUS_REJECTED'
  | 'ORDER_STATUS_EXPIRED'

/** OrderSummary is order.v1.OrderState, as much of it as this screen reads. */
export interface OrderSummary {
  order_id: string
  instrument_id?: string
  side?: string
  status?: OrderStatus
  quantity?: Decimal
  filled_quantity?: Decimal
  limit_price?: Money
  average_fill_price?: Money
  venue?: string
  as_of?: string
}

export interface OrdersResponse {
  orders?: OrderSummary[]
  /**
   * unindexed is how many of this tenant's orders CANNOT appear in any page.
   *
   * They predate the OMS's portfolio index and carry no portfolio_id — their
   * real one is inside a marshaled blob no index can see. It arrives as a
   * STRING: it is an int64, and 64 bits do not survive a JSON number.
   */
  unindexed?: string
}

export const risk = {
  /**
   * portfolios lists what the risk engine can answer for.
   *
   * IT IS NOT THE FUND'S BOOK OF RECORD. The engine returns the portfolios it
   * has folded state for, so one that exists but has had no event reach the
   * engine is absent — which is the honest scope for a picker whose every
   * destination is a risk read. A screen must not describe this as "your
   * portfolios" without that qualification.
   */
  portfolios: () =>
    api.get<{ portfolios?: PortfolioSummary[] }>('/api/v1/portfolios').then((r) => r.portfolios ?? []),

  /**
   * orders reads one portfolio's trading history, newest first.
   *
   * The route is registered only when the gateway fronts an OMS read surface, so
   * a 404 here can also mean "this deployment serves no order history" — which
   * is not "this portfolio has never traded". describeOrders separates them.
   */
  orders: (id: string, limit?: number) =>
    api.get<OrdersResponse>(
      `/api/v1/portfolios/${encodeURIComponent(id)}/orders` + (limit ? `?limit=${limit}` : ''),
    ),

  /** exposure reads one portfolio's decomposition. */
  exposure: (id: string) =>
    api.get<ExposureResponse>(`/api/v1/portfolios/${encodeURIComponent(id)}/exposure`),
}

/**
 * describeRisk turns a failure on these routes into something a reader can act
 * on.
 *
 * THE 404 IS THE ONE THAT MATTERS, and it means something different here than
 * it does on the control routes. The gateway answers 404 for BOTH "no such
 * portfolio" and "not yours" — deliberately, so the route cannot be used to
 * enumerate other tenants' portfolios by status code. On the LIST route it
 * additionally covers "this gateway's engine serves a different tenant". None
 * of those is "you have no portfolios", and rendering an empty table would say
 * exactly that.
 */
export function describeRisk(e: unknown): string {
  if (!(e instanceof ApiError)) {
    return e instanceof Error ? e.message : 'the request failed'
  }
  switch (e.status) {
    case 403:
      return 'Your account does not carry read authority for risk data.'
    case 404:
      return 'No portfolios are visible to you here. That is not the same as the fund having none — this gateway may front a risk engine for another tenant, or your account may be scoped to portfolios it cannot see.'
    case 502:
    case 503:
      return 'The risk engine is unreachable. This says nothing about the portfolios themselves.'
    default:
      return e.message
  }
}
