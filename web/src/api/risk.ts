import { api, ApiError } from './client'
import { isNonnegativeInt64 } from './int64'
import { checkWireDecimal, checkWireMoney } from './financial-wire'
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

/** Only known freshness flags can be presented as ordinary age warnings. */
export function changesMeaning(f: QualityFlag): boolean {
  return f !== 'QUALITY_FLAG_STALE' && f !== 'QUALITY_FLAG_DEGRADED'
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
      return 'Positions in a currency the engine could not convert are missing from these figures. Omitted positions may increase or decrease net exposure.'
    case 'QUALITY_FLAG_INPUTS_UNRESOLVED':
      return 'Data the engine needed was not there, so at least one figure was computed over part of the book — possibly over none of it. A zero here does not mean zero risk. Do not act on these numbers.'
    case 'QUALITY_FLAG_DEGRADED':
      return 'The engine was degraded when this was computed: these are cached, last-known values rather than live ones.'
    case 'QUALITY_FLAG_STALE':
      return 'This data is older than the request asked for, though the engine itself is healthy.'
    default:
      return `The engine returned an unrecognized quality flag (${f}). The reliability of these figures is unknown.`
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
  | 'ORDER_STATUS_WORKING_SCHEDULED'

/** OrderSummary is order.v1.OrderState, as much of it as this screen reads. */
export interface OrderSummary {
  portfolio_id: string
  order_id: string
  instrument_id?: string
  side?: string | number
  status?: OrderStatus | number
  ordered_quantity?: Decimal
  filled_quantity?: Decimal
  limit_price?: Decimal
  average_fill_price?: Decimal
  venue?: string
  as_of?: string
}

export interface OrdersResponse {
  orders: OrderSummary[]
  /**
   * unindexed is how many of this tenant's orders CANNOT appear in any page.
   *
   * They predate the OMS's portfolio index and carry no portfolio_id — their
   * real one is inside a marshaled blob no index can see. It arrives as a
   * STRING: it is an int64, and 64 bits do not survive a JSON number.
   */
  unindexed: string
}

// ProtoJSON keeps unrecognized enum values as signed 32-bit integers.
function isWireEnum(value: unknown): boolean {
  return value === undefined || typeof value === 'string' || (typeof value === 'number' &&
    Number.isInteger(value) && value >= -2147483648 && value <= 2147483647)
}

export const orderHistoryLimit = 200

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
  async orders(id: string, tenant: string, signal?: AbortSignal): Promise<OrdersResponse> {
    if (!id || !tenant || id === '.' || id === '..') throw new Error('Order history requires a portfolio and tenant.')
    const page = await api.get<OrdersResponse & { owner_tenant: string }>(
      `/api/v1/portfolios/${encodeURIComponent(id)}/orders?limit=${orderHistoryLimit}`, { signal, expectedStatus: 200 },
    )
    if (page?.owner_tenant !== tenant || !Array.isArray(page.orders) || page.orders.length > orderHistoryLimit ||
        !isNonnegativeInt64(page.unindexed)) throw new Error('The order history could not be verified.')
    const ids = new Set<string>()
    for (const order of page.orders) {
      if (!order || order.portfolio_id !== id || typeof order.order_id !== 'string' || !order.order_id.trim() ||
          ids.has(order.order_id) || [order.instrument_id, order.venue].some(value => value !== undefined && typeof value !== 'string') ||
          !isWireEnum(order.side) || !isWireEnum(order.status)) throw new Error('The order history could not be verified.')
      ids.add(order.order_id)
      checkWireDecimal(order.ordered_quantity)
      checkWireDecimal(order.filled_quantity)
      checkWireDecimal(order.limit_price)
      checkWireDecimal(order.average_fill_price)
    }
    return page
  },

  /** exposure reads one portfolio's decomposition. */
  async exposure(id: string) {
    const response = await api.get<ExposureResponse>(`/api/v1/portfolios/${encodeURIComponent(id)}/exposure`)
    for (const row of response.set?.exposures ?? []) {
      checkWireMoney(row.gross)
      checkWireMoney(row.net)
    }
    return response
  },
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
