import { describe, expect, it } from 'vitest'
import { ApiError } from './client'
import { describe as describeError, ready, type Node } from './control'

// A NODE IS READY ONLY IF IT SAYS SO.
//
// protojson omits zero values, so a node whose Ready condition is absent or
// Unknown arrives with NO status field at all. Every shape below that is not an
// explicit READY must render as not-ready — an unknown node shown as green is
// the one error on this screen with an operational cost.
describe('node readiness is deny-by-default', () => {
  const cases: Array<[string, Node, boolean]> = [
    ['explicitly ready', { name: 'a', status: 'NODE_STATUS_READY' }, true],
    ['explicitly not ready', { name: 'b', status: 'NODE_STATUS_NOT_READY' }, false],
    ['unspecified', { name: 'c', status: 'NODE_STATUS_UNSPECIFIED' }, false],
    ['status omitted entirely (the protojson zero value)', { name: 'd' }, false],
  ]
  for (const [name, node, want] of cases) {
    it(`${name} -> ${want ? 'ready' : 'not ready'}`, () => {
      expect(ready(node)).toBe(want)
    })
  }
})

describe('failures an operator can act on', () => {
  // THE IMPORTANT ONE. The gateway registers control routes only when it has a
  // control plane; without one they do not exist. Drawn as an empty table, that
  // reads as a healthy estate with no nodes.
  it('says a 404 means no control plane is configured, not an empty estate', () => {
    const msg = describeError(new ApiError(404, 'not found'))
    expect(msg).toMatch(/no control plane/i)
    expect(msg).not.toMatch(/^not found$/)
  })

  it('says a 403 is missing authority, not a broken estate', () => {
    expect(describeError(new ApiError(403, 'insufficient capability'))).toMatch(/operator authority/i)
  })

  it('distinguishes an unreachable control plane from a healthy empty one', () => {
    expect(describeError(new ApiError(503, 'upstream unavailable'))).toMatch(/says nothing about the estate/i)
  })

  it('passes through an unrecognised status rather than inventing an explanation', () => {
    expect(describeError(new ApiError(418, 'teapot'))).toBe('teapot')
  })

  it('handles a non-ApiError without claiming to know what happened', () => {
    expect(describeError(new TypeError('network down'))).toBe('network down')
    expect(describeError('nonsense')).toBe('the request failed')
  })
})
