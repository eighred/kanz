import { api, ApiError } from './client'

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
