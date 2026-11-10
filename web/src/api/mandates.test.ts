import { describe, it, expect } from 'vitest'

import {
  renderVersion,
  renderRuleCount,
  describeVersionRefusal,
  type PendingMandateChange,
} from './mandates'

// THE MANDATE VERSION RIDES THE WIRE AS A STRING (#606).
//
// compliance's proposalJSON emits it through jsonUint64 — `strconv.FormatUint` —
// on all four bodies that carry a mandate. That is the whole point of the
// server-side repair: a uint64 cannot ride a JSON number, because JSON.parse is
// the only decoder a browser has and it parses every number as a float64, so
// 18446744073709551615 arrives as 18446744073709552000 with the true value
// unrecoverable.
//
// This client was never updated. It still declared `version?: number` and opened
// renderVersion with `typeof v !== 'number'`, which against a string is ALWAYS
// true — so every row on the signatory's queue rendered "VERSION NOT RENDERABLE",
// not just the ones above 2^53 the check was written to protect.
//
// These tests are written against what compliance actually sends.

function change(over: Partial<PendingMandateChange> = {}): PendingMandateChange {
  return {
    proposal_id: 'prop-1',
    portfolio_id: 'PF1',
    proposer: 'user:bob',
    version: '7',
    rule_count: 3,
    ...over,
  }
}

describe('renderVersion reads the version compliance actually sends', () => {
  it('renders an ordinary version', () => {
    expect(renderVersion(change({ version: '7' }))).toBe('7')
  })

  it('renders a version above 2^53 EXACTLY, which is why the server sends a string', () => {
    // uint64 max. As a JSON number this was destroyed before any client code ran.
    // As a string it is exact, and refusing it now would throw away the repair.
    expect(renderVersion(change({ version: '18446744073709551615' }))).toBe('18446744073709551615')
  })

  it('renders version zero as "0" rather than as absent', () => {
    // Same falsy trap renderRuleCount exists to avoid: 0 is a value, not a gap.
    expect(renderVersion(change({ version: '0' }))).toBe('0')
  })

  it('says nothing was stated when the key is absent', () => {
    expect(renderVersion(change({ version: undefined }))).toBeNull()
  })

  it('REFUSES a JSON number rather than coercing it', () => {
    // An older or proxied compliance still emitting a bare uint64. Number 7 is
    // harmless, but the SAME path carries values above 2^53 that are already
    // damaged, and this client cannot tell the two apart. Refusing is the only
    // answer that never prints a wrong version as fact — the discipline
    // renderRuleCount already applies to a stringly-typed count.
    expect(renderVersion(change({ version: 7 as unknown as string }))).toBeNull()
    expect(renderVersion(change({ version: 18446744073709551615 as unknown as string }))).toBeNull()
  })

  it('refuses anything that is not a canonical decimal', () => {
    // strconv.FormatUint emits no sign, no padding, no exponent and no spaces,
    // so each of these means something other than compliance answered.
    for (const bad of ['', ' 7', '7 ', '007', '-1', '7.0', '1e3', '0x7', 'seven', '٧']) {
      expect(renderVersion(change({ version: bad }))).toBeNull()
    }
  })

  it('refuses a value outside the uint64 domain', () => {
    // One past uint64 max. FormatUint cannot produce it, so it is not a version
    // this platform issued, and printing it would assert a fact nobody stated.
    expect(renderVersion(change({ version: '18446744073709551616' }))).toBeNull()
  })
})

describe('renderRuleCount is unchanged and still a JSON number', () => {
  // rule_count is `len(p.Mandate.GetRules())` — a Go int, correctly a JSON
  // number, and NOT part of the uint64 repair. Pinned here so a future sweep of
  // "make the integers strings" does not take it along: the two fields are
  // different types on the wire for a reason.
  it('renders a count, including a legal zero', () => {
    expect(renderRuleCount(change({ rule_count: 3 }))).toBe('3')
    expect(renderRuleCount(change({ rule_count: 0 }))).toBe('0')
  })

  it('refuses a stringly-typed count', () => {
    expect(renderRuleCount(change({ rule_count: '3' as unknown as number }))).toBeNull()
  })
})

describe('describeVersionRefusal names the actual reason, not a guessed one', () => {
  // THE MESSAGE IS THE REPAIR INSTRUCTION. "did not survive as an exact integer"
  // was true of the old number-on-the-wire contract and is now wrong for every
  // case that can actually occur: an operator reading it would go looking for a
  // rounding bug when what they have is a stale compliance or a proxy rewriting
  // the body. A refusal that misnames its cause sends the fix to the wrong place.

  it('is null while the version renders, so the caller cannot show both', () => {
    expect(describeVersionRefusal(change({ version: '7' }))).toBeNull()
  })

  it('says nothing was sent when the key is absent', () => {
    expect(describeVersionRefusal(change({ version: undefined }))).toContain('sent no version')
  })

  it('names a JSON number as the wire-type fault it is', () => {
    const msg = describeVersionRefusal(change({ version: 7 as unknown as string }))
    expect(msg).toContain('JSON number')
    // and it must not claim the value was rounded: 7 plainly was not.
    expect(msg).not.toContain('did not survive')
  })

  it('quotes a malformed string back so it can be reported', () => {
    const msg = describeVersionRefusal(change({ version: '00seven' }))
    expect(msg).toContain('00seven')
    expect(msg).toContain('not a uint64')
  })

  it('names an out-of-domain value as out of range, not as malformed', () => {
    const msg = describeVersionRefusal(change({ version: '18446744073709551616' }))
    expect(msg).toContain('larger than a uint64')
  })
})
