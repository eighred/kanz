import { describe, expect, it } from 'vitest'
import {
  MAX_RAT_DIGITS,
  MAX_SAFE_EXPONENT,
  formatDecimal,
  formatMoney,
  formatRational,
  isNegative,
} from './decimal'

// THE CLAIM THIS MODULE MAKES IS "NO FLOAT, EVER", and it is a correctness claim
// about money rather than a formatting preference. Everything below is a way for
// a valuation to be silently wrong, which is worse than one that fails to render.

describe('a 64-bit coefficient survives intact', () => {
  // THE ONE THAT MATTERS. protojson sends the sint64 coefficient as a STRING
  // because 64 bits do not survive a JSON number. Anything that routes it
  // through parseFloat/Number loses digits above 2^53 — silently, and only for
  // large values, so it would pass every small test and fail on the real book.
  it('renders a coefficient beyond 2^53 exactly', () => {
    const big = '9007199254740993' // 2^53 + 1: the first integer a double cannot hold
    expect(formatDecimal({ coefficient: big, exponent: 0 })).toBe(big)
    expect(Number(big).toString()).not.toBe(big) // the failure this avoids, demonstrated
  })

  it('renders a large negative coefficient with a fraction exactly', () => {
    expect(formatDecimal({ coefficient: '-123456789012345678', exponent: -2 }))
      .toBe('-1234567890123456.78')
  })
})

describe('the decimal point moves rather than the value being multiplied', () => {
  const cases: Array<[string, number, string]> = [
    ['12345', -2, '123.45'],
    ['12345', 0, '12345'],
    ['12345', 2, '1234500'],
    ['5', -3, '0.005'], // fewer digits than the shift ⇒ padded, not truncated
    ['-5', -3, '-0.005'],
    ['0', -2, '0.00'],
    ['100', -2, '1.00'], // trailing zeros are significant digits the sender chose
  ]
  for (const [coefficient, exponent, want] of cases) {
    it(`{${coefficient}, ${exponent}} -> ${want}`, () => {
      expect(formatDecimal({ coefficient, exponent })).toBe(want)
    })
  }

  // "-0" is not a value anyone means.
  it('does not render a negative zero', () => {
    expect(formatDecimal({ coefficient: '-0', exponent: -2 })).toBe('0.00')
  })
})

// THE BROWSER TWIN OF #95. An unvalidated wire exponent reaching
// 10^abs(exponent) never returns; in Go that hung a fold, here it hangs the tab.
// The bound mirrors internal/dec's maxSafeExponent.
describe('an out-of-domain exponent is refused, not materialised', () => {
  it('refuses an exponent past the bound instead of allocating', () => {
    const started = Date.now()
    expect(formatDecimal({ coefficient: '1', exponent: 2_000_000_000 })).toBeNull()
    expect(formatDecimal({ coefficient: '1', exponent: -2_000_000_000 })).toBeNull()
    // If this ever starts materialising the exponent it will not return at all;
    // the assertion is here so the failure is a slow test rather than a hung
    // browser tab in front of a user.
    expect(Date.now() - started).toBeLessThan(1_000)
  })

  it('accepts exactly the bound and refuses one past it', () => {
    expect(formatDecimal({ coefficient: '1', exponent: MAX_SAFE_EXPONENT })).not.toBeNull()
    expect(formatDecimal({ coefficient: '1', exponent: MAX_SAFE_EXPONENT + 1 })).toBeNull()
  })

  // A REFUSAL IS NULL, NEVER ZERO. internal/dec makes the same rule at the same
  // boundary: a zero price can be read as "flat" and dropped from downstream
  // checks, so it is never a safe fallback.
  it('refuses a malformed coefficient rather than reading it as zero', () => {
    for (const coefficient of ['', 'abc', '1.5', '1e3', ' 12']) {
      expect(formatDecimal({ coefficient, exponent: 0 })).toBeNull()
    }
  })
})

describe('money carries its currency', () => {
  it('appends the code rather than a symbol', () => {
    expect(formatMoney({ amount: { coefficient: '12345', exponent: -2 }, currency_code: 'USD' }))
      .toBe('123.45 USD')
  })

  // An unrenderable amount must not render as a bare currency, which would read
  // as a zero balance in that currency.
  it('is null when the amount cannot be rendered', () => {
    expect(formatMoney({ amount: { coefficient: 'oops' }, currency_code: 'USD' })).toBeNull()
    expect(formatMoney(undefined)).toBeNull()
  })

  it('renders an amount with no currency rather than losing it', () => {
    expect(formatMoney({ amount: { coefficient: '1', exponent: 0 } })).toBe('1')
  })
})

describe('sign is read from the string, never from a parsed number', () => {
  it('reports a negative beyond 2^53', () => {
    expect(isNegative({ coefficient: '-9007199254740993' })).toBe(true)
    expect(isNegative({ coefficient: '9007199254740993' })).toBe(false)
    expect(isNegative(undefined)).toBe(false)
  })
})

// THE OTHER WIRE FORM FOR MONEY ON THIS PLATFORM (#371 act one).
//
// datamaster's pricing-override surface does not speak common.v1.Decimal: it
// holds the operator's chosen price as a *big.Rat and renders it with
// RatString(), which emits "130" for an integer and "a/b" — "261/2" for 130.5 —
// otherwise. It is the value a second person signs for, into an append-only
// compliance record, so the claim is the same one the rest of this module makes:
// exact, or nothing.
describe('a rational chosen price renders exactly or not at all', () => {
  const exact: Array<[string, string]> = [
    ['130', '130'],
    ['-130', '-130'],
    ['261/2', '130.5'], // the form 130.5 actually takes on the wire
    ['-261/2', '-130.5'],
    ['1/8', '0.125'], // fewer digits than the shift ⇒ padded, not truncated
    ['-1/8', '-0.125'],
    ['1/4', '0.25'],
    ['1/5', '0.2'],
    ['3/1', '3'], // a denominator of one is still a denominator
    ['0', '0'],
    ['123/1000', '0.123'],
  ]
  for (const [wire, want] of exact) {
    it(`${wire} -> ${want}`, () => {
      expect(formatRational(wire)).toBe(want)
    })
  }

  // THE ONE THAT MATTERS, and the reason Number() is unavailable even as a
  // shortcut on the integer case.
  it('renders a price beyond 2^53 digit for digit', () => {
    const big = '9007199254740993' // 2^53 + 1: the first integer a double cannot hold
    expect(formatRational(big)).toBe(big)
    expect(Number(big).toString()).not.toBe(big) // the failure this avoids, demonstrated
  })

  it('renders a large numerator over a power of ten without losing digits', () => {
    expect(formatRational('9007199254740993/100')).toBe('90071992547409.93')
  })

  // NULL IS NOT ZERO AND IT IS NOT AN APPROXIMATION. A price that cannot be
  // rendered must look unrenderable: rounding it would put a figure into an
  // append-only record that nobody read.
  it('is null when the value has no exact decimal', () => {
    expect(formatRational('1/3')).toBeNull()
    expect(formatRational('22/7')).toBeNull()
    expect(formatRational('1/6')).toBeNull()
  })

  it('is null rather than NaN, Infinity or a guess on anything malformed', () => {
    expect(formatRational('')).toBeNull()
    expect(formatRational(undefined)).toBeNull()
    expect(formatRational(null)).toBeNull()
    expect(formatRational('1/0')).toBeNull()
    expect(formatRational('about a hundred')).toBeNull()
    expect(formatRational('130.5')).toBeNull() // RatString never emits a point
    expect(formatRational('1/2/3')).toBeNull()
    expect(formatRational('1e3')).toBeNull()
    expect(formatRational(' 130')).toBeNull()
  })

  // big.Rat keeps the sign on the numerator, so a negative denominator is not
  // something RatString emits — it is something else answering, and guessing at
  // it is how a price renders with the wrong sign.
  it('refuses a negative denominator rather than normalising it', () => {
    expect(formatRational('1/-2')).toBeNull()
  })

  // The browser twin of #95: unbounded work from a wire value hangs the TAB.
  it('refuses a denominator whose scale exceeds the module bound', () => {
    expect(formatRational(`1/${2n ** BigInt(MAX_SAFE_EXPONENT + 1)}`)).toBeNull()
    // And the bound is a real edge rather than a decoration: one at it renders.
    expect(formatRational(`1/${2n ** BigInt(MAX_SAFE_EXPONENT)}`)).not.toBeNull()
  })

  it('refuses a numeral longer than any price this platform can carry', () => {
    expect(formatRational('9'.repeat(MAX_RAT_DIGITS))).not.toBeNull()
    expect(formatRational('9'.repeat(MAX_RAT_DIGITS + 1))).toBeNull()
  })

  it('does not produce a negative zero', () => {
    expect(formatRational('-0')).toBe('0')
    expect(formatRational('-0/5')).toBe('0.0')
  })
})
