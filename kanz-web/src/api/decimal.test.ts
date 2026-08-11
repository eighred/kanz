import { describe, expect, it } from 'vitest'
import { MAX_SAFE_EXPONENT, formatDecimal, formatMoney, isNegative } from './decimal'

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
