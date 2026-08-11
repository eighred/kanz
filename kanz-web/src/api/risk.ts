import { api, ApiError } from './client'
import type { Money } from './decimal'

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
 * QualityFlag is a query-result concern, and two of the three change what the
 * numbers MEAN rather than how fresh they are.
 *
 * CURRENCY_EXCLUDED is the one to respect. The proto is explicit: positions in
 * an unconvertible currency are omitted, so "a concentration or exposure limit
 * checked against them can pass when the full book would breach", and a gate
 * that must not under-report MUST refuse to act on a response carrying it. A
 * screen cannot refuse on the reader's behalf — but it must not present an
 * under-reporting total as a total.
 */
export type QualityFlag =
  | 'QUALITY_FLAG_UNSPECIFIED'
  | 'QUALITY_FLAG_DEGRADED'
  | 'QUALITY_FLAG_STALE'
  | 'QUALITY_FLAG_CURRENCY_EXCLUDED'

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
    case 'QUALITY_FLAG_DEGRADED':
      return 'The engine was degraded when this was computed: these are cached, last-known values rather than live ones.'
    case 'QUALITY_FLAG_STALE':
      return 'This data is older than the request asked for, though the engine itself is healthy.'
    default:
      return f
  }
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
