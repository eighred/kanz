import { api, ApiError } from './client'

// The tradeable-pair catalogue, as the browser sees it (#406).
//
// It is what a pair picker offers, and it is the ONLY honest source for one:
// nothing may hardcode a list of pairs in the frontend, because the set is a
// property of the deployment — which venue adapters it runs and which symbol
// maps they hold. A hardcoded list would offer a pair this deployment cannot
// route, and the order would be refused at admission with VENUE_NOT_CONFIGURED,
// which an operator reads as a platform fault rather than as a stale UI.
//
// Same serialisation as risk.ts (snake_case, zeros present) — this rides the
// same half of /v1.

/** TradeableInstrument is one pair at one venue. */
export interface TradeableInstrument {
  /** The platform's canonical id — what an order carries, e.g. "BTC-USD". */
  instrument_id: string
  /**
   * What the EXCHANGE calls the same thing, e.g. "BTCUSDT".
   *
   * SHOW IT. The canonical "BTC-USD" maps to a USDT-quoted symbol on both live
   * venues, so a user picking "BTC-USD" is buying a stablecoin-quoted pair. Until
   * instruments carry base and quote explicitly (#407) this field is the only
   * place that is visible, and a picker that hid it would tell a user they were
   * buying dollars.
   */
  venue_symbol: string
  /** The venue it trades on. An order names a venue, so a pair without one cannot be acted on. */
  mic: string
}

export interface InstrumentsResponse {
  instruments?: TradeableInstrument[]
  owner_tenant?: string
}

export const instruments = {
  /**
   * list reads every pair this deployment can route.
   *
   * The route exists only when the gateway fronts an OMS read surface, so a 404
   * can also mean "not served here" — describeInstruments separates that from an
   * estate that genuinely trades nothing.
   */
  list: () => api.get<InstrumentsResponse>('/api/v1/instruments'),
}

/**
 * describeInstruments turns a failure into something a reader can act on.
 *
 * THE EMPTY LIST IS NOT AN ERROR and is deliberately not handled here: a
 * deployment whose adapters hold no symbol map really can trade nothing, and a
 * screen must say that rather than spin. Only a refusal reaches this function.
 */
export function describeInstruments(err: unknown): string {
  if (err instanceof ApiError && err.status === 404) {
    return (
      'This deployment does not serve a tradeable-pair catalogue. That is a configuration ' +
      'answer, not an empty one — it is not the same as the platform having nothing to trade.'
    )
  }
  if (err instanceof ApiError && err.status === 403) {
    return 'You are not permitted to read this catalogue.'
  }
  return 'The tradeable-pair catalogue could not be read.'
}

/**
 * byInstrument groups the flat list into one row per canonical id, carrying every
 * venue that lists it.
 *
 * THE SAME PAIR AT TWO VENUES IS TWO ROUTES, NOT A DUPLICATE. "BTC-USD" trades at
 * both XBIN and XOKX under different exchange symbols, and an order must name one
 * of them. Collapsing them would offer a pair with no venue to send it to; listing
 * them as unrelated rows would read as two different instruments.
 */
export function byInstrument(
  list: TradeableInstrument[],
): { instrument_id: string; venues: TradeableInstrument[] }[] {
  const groups = new Map<string, TradeableInstrument[]>()
  for (const in_ of list) {
    const got = groups.get(in_.instrument_id)
    if (got) got.push(in_)
    else groups.set(in_.instrument_id, [in_])
  }
  return [...groups.entries()].map(([instrument_id, venues]) => ({
    instrument_id,
    venues,
  }))
}
