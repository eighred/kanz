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

/**
 * MAX_RAT_DIGITS bounds the work a wire rational may ask of this tab.
 *
 * It is not arbitrary: a price datamaster accepted has already been asserted to
 * survive a common.v1.Decimal round trip, whose coefficient is an int64 (19
 * digits) and whose exponent this module bounds at MAX_SAFE_EXPONENT — so 128
 * digits is comfortably past anything the platform can carry, and a longer
 * numeral is by construction not a price. Refusing it costs nothing real and
 * keeps a hostile or broken upstream from handing the browser arbitrary work.
 */
export const MAX_RAT_DIGITS = 128

/**
 * formatRational renders Go's `big.Rat.RatString()` as an exact decimal, or null.
 *
 * # WHY THIS EXISTS RATHER THAN formatDecimal
 *
 * datamaster's pricing-override surface does NOT speak common.v1.Decimal. It
 * stores the operator's chosen price as a *big.Rat and renders it with
 * RatString(), which emits "130" for an integer and "a/b" — "261/2" for 130.5 —
 * for everything else. That is a different wire form from the {coefficient,
 * exponent} object protojson sends, and reading one as the other renders a price
 * as nothing. Both arrive as STRINGS, which is the property that matters.
 *
 * # NO FLOAT, AND NO ROUNDING EITHER
 *
 * parseFloat("261/2") is NaN and Number("9007199254740993") loses a digit, so
 * neither is available even as a shortcut. The division is done in BigInt and
 * only when it is EXACT: a denominator with any prime factor other than 2 or 5
 * has no terminating decimal, and this returns null rather than a truncation.
 *
 * NULL IS NOT ZERO AND IT IS NOT AN APPROXIMATION. It is the same contract
 * formatDecimal takes, for the same reason internal/dec gives: a figure that
 * cannot be rendered must look unrenderable. On this surface the figure is the
 * price a second person is about to sign for, and a quietly rounded one is the
 * signature covering a value nobody read.
 */
export function formatRational(s: string | undefined | null): string | null {
  if (!s) return null
  const slash = s.indexOf('/')
  const numerator = slash < 0 ? s : s.slice(0, slash)
  const denominator = slash < 0 ? '1' : s.slice(slash + 1)

  // A NEGATIVE DENOMINATOR IS REFUSED RATHER THAN NORMALISED. big.Rat keeps the
  // sign on the numerator, so "1/-2" is not something RatString emits — it is
  // something else answering, and guessing at its meaning is how a price ends up
  // rendered with the wrong sign.
  if (!/^-?\d+$/.test(numerator) || !/^\d+$/.test(denominator)) return null
  if (numerator.length > MAX_RAT_DIGITS || denominator.length > MAX_RAT_DIGITS) return null

  let den = BigInt(denominator)
  if (den === 0n) return null
  const negative = numerator.startsWith('-')
  const num = BigInt(negative ? numerator.slice(1) : numerator)

  // Strip the factors a decimal can express. Whatever is left decides whether
  // this value terminates at all; the loops are bounded by the digit count above.
  let twos = 0
  let fives = 0
  while (den % 2n === 0n) {
    den /= 2n
    twos++
  }
  while (den % 5n === 0n) {
    den /= 5n
    fives++
  }
  if (den !== 1n) return null // 1/3 has no exact decimal, and 0.333… is a lie
  const scale = Math.max(twos, fives)
  if (scale > MAX_SAFE_EXPONENT) return null

  // 10^scale / (2^twos · 5^fives) is an integer by construction, so this is a
  // multiplication and never a division that could round.
  const digitsOf = num * 2n ** BigInt(scale - twos) * 5n ** BigInt(scale - fives)
  let digits = digitsOf.toString()
  if (scale === 0) return sign(negative, digits)
  if (digits.length <= scale) {
    digits = digits.padStart(scale + 1, '0')
  }
  const cut = digits.length - scale
  return sign(negative, `${digits.slice(0, cut)}.${digits.slice(cut)}`)
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
