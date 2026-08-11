// Rendering common.v1.Decimal in a browser (#399).
//
// A Decimal is {coefficient, exponent} and it is NOT a number here. protojson
// emits the sint64 coefficient as a JSON STRING, because 64 bits do not survive
// a JSON number — so it arrives as "123456789012345678" and the one thing this
// module must never do is parseFloat it. Above 2^53 that loses digits silently,
// and everything on this surface is money: a valuation that is quietly wrong is
// worse than one that fails to render.
//
// So the coefficient is handled as a string and a BigInt, and the exponent is
// applied by moving the decimal point rather than by multiplying. No float is
// constructed anywhere in this file, which is the whole property.
//
// THE EXPONENT IS BOUNDED, AND THAT IS NOT DEFENSIVENESS. It is the browser twin
// of #95: an unvalidated wire exponent reaching 10^abs(exponent) never returns.
// In Go that hung a fold; here `'0'.repeat(exponent)` hangs the TAB, which is the
// same incident with a worse audience. The bound matches internal/dec's
// maxSafeExponent, and test/arch/decimal_domain_test.go is what keeps the two
// equal — a comment naming a sibling is not a check.

/** MAX_SAFE_EXPONENT mirrors internal/dec's maxSafeExponent. */
export const MAX_SAFE_EXPONENT = 64

/** Decimal is common.v1.Decimal as protojson renders it. */
export interface Decimal {
  /** sint64, and therefore a STRING on the wire. */
  coefficient?: string
  /** sint32, and therefore a number. */
  exponent?: number
}

/** Money is common.v1.Money as protojson renders it. */
export interface Money {
  amount?: Decimal
  currency_code?: string
}

/**
 * formatDecimal renders a Decimal exactly, or returns null when it cannot.
 *
 * NULL IS NOT ZERO, AND CALLERS MUST NOT SUBSTITUTE ONE. internal/dec says the
 * same thing at the same boundary: "a zero price is not a safe fallback anywhere
 * on this platform: it can be silently treated as flat and dropped from
 * downstream checks". A figure that cannot be rendered must look unrenderable.
 */
export function formatDecimal(d: Decimal | undefined | null): string | null {
  if (!d) return null
  const raw = d.coefficient ?? '0'
  const exponent = d.exponent ?? 0

  if (!/^-?\d+$/.test(raw)) return null
  if (!Number.isInteger(exponent) || Math.abs(exponent) > MAX_SAFE_EXPONENT) return null

  const negative = raw.startsWith('-')
  let digits = negative ? raw.slice(1) : raw

  if (exponent >= 0) {
    // Bounded by MAX_SAFE_EXPONENT above, so this cannot become the incident.
    digits += '0'.repeat(exponent)
    return sign(negative, stripLeadingZeros(digits))
  }

  const shift = -exponent
  if (digits.length <= shift) {
    digits = digits.padStart(shift + 1, '0')
  }
  const cut = digits.length - shift
  const whole = stripLeadingZeros(digits.slice(0, cut))
  const frac = digits.slice(cut)
  // The fraction keeps its trailing zeros: they are significant digits the
  // sender chose to transmit, and 1.50 is a different statement from 1.5 about
  // precision. Trimming them here would be this module deciding that.
  return sign(negative, `${whole}.${frac}`)
}

/**
 * formatMoney renders a Money, or null when the amount cannot be rendered.
 *
 * The currency is appended rather than symbolised. A symbol table is a place for
 * two currencies to share a glyph, and on a multi-currency book the code is the
 * unambiguous thing.
 */
export function formatMoney(m: Money | undefined | null): string | null {
  if (!m) return null
  const amount = formatDecimal(m.amount)
  if (amount === null) return null
  return m.currency_code ? `${amount} ${m.currency_code}` : amount
}

/** isNegative reports the sign without parsing the value as a number. */
export function isNegative(d: Decimal | undefined | null): boolean {
  return (d?.coefficient ?? '0').startsWith('-')
}

function stripLeadingZeros(s: string): string {
  const t = s.replace(/^0+/, '')
  return t === '' ? '0' : t
}

function sign(negative: boolean, body: string): string {
  // "-0" is not a value anyone means; it appears when a negative coefficient
  // renders to zero after the point is moved.
  if (!negative) return body
  return /^0(\.0*)?$/.test(body) ? body : `-${body}`
}
