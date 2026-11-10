import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from './client'
import { control, describe as describeError, ready, settled, type Node, type Provision } from './control'

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

// THE ACTION ROUTES ADDRESS A NODE BY PATH, AND THE PATH IS THE ONLY IDENTITY.
//
// The gateway takes the name from the URL and overwrites whatever the body
// carried — "two spellings of the same identity is a way to drain the node you
// were not looking at". These pin the client to the same single spelling, and
// pin the ONE action that carries a body to carrying only its region.
describe('node actions address the node through the path', () => {
  let calls: Array<{ method: string; url: string; body?: string }>

  beforeEach(() => {
    calls = []
    vi.stubGlobal('fetch', (url: string, init: RequestInit) => {
      calls.push({ method: init.method ?? 'GET', url, body: init.body as string | undefined })
      return Promise.resolve(new Response('{}', { status: 200, headers: { 'content-type': 'application/json' } }))
    })
  })
  afterEach(() => vi.unstubAllGlobals())

  it('posts each action to the node it names', async () => {
    await control.cordon('worker-1')
    await control.uncordon('worker-1')
    await control.drain('worker-1')

    expect(calls.map((c) => `${c.method} ${c.url}`)).toEqual([
      'POST /api/v1/control/nodes/worker-1/cordon',
      'POST /api/v1/control/nodes/worker-1/uncordon',
      'POST /api/v1/control/nodes/worker-1/drain',
    ])
  })

  // The three reversible-or-inverse actions send NO body. A body would be a
  // second place for a node name to live, which is exactly what the gateway
  // guards against by overwriting it.
  it('sends no body for cordon, uncordon or drain', async () => {
    await control.cordon('worker-1')
    await control.drain('worker-1')
    expect(calls.every((c) => c.body === undefined)).toBe(true)
  })

  it('sends only the region on a move, never the node name', async () => {
    await control.setRegion('worker-1', 'eu-west')

    expect(calls[0]?.url).toBe('/api/v1/control/nodes/worker-1/region')
    expect(JSON.parse(calls[0]?.body as string)).toEqual({ region: 'eu-west' })
  })

  // A node name is a Kubernetes object name today, so nothing needs escaping.
  // Relying on that silently is how it stops being true.
  it('encodes the name rather than trusting it to be path-safe', async () => {
    await control.cordon('worker/../other')
    expect(calls[0]?.url).toBe('/api/v1/control/nodes/worker%2F..%2Fother/cordon')
  })
})

// A PROVISION THAT HAS NOT SETTLED IS STILL RUNNING, WHATEVER IT SAYS.
//
// protojson omits the zero value, so a run whose status is UNSPECIFIED may carry
// no status field at all — and a value added to the enum later arrives as a name
// this build has never seen. Both must count as in flight: a run shown as
// finished when nobody knows is how a half-joined node gets forgotten.
describe('provision settlement is deny-by-default', () => {
  const cases: Array<[string, Provision, boolean]> = [
    ['joined', { id: '1', status: 'PROVISION_STATUS_JOINED' }, true],
    ['failed', { id: '2', status: 'PROVISION_STATUS_FAILED' }, true],
    ['installing', { id: '3', status: 'PROVISION_STATUS_INSTALLING' }, false],
    ['pending', { id: '4', status: 'PROVISION_STATUS_PENDING' }, false],
    ['unspecified', { id: '5', status: 'PROVISION_STATUS_UNSPECIFIED' }, false],
    ['status omitted entirely (the protojson zero value)', { id: '6' }, false],
    // A future enum value this build predates.
    ['a status this build does not know', { id: '7', status: 'PROVISION_STATUS_CANCELLED' as never }, false],
  ]
  for (const [name, p, want] of cases) {
    it(`${name} -> ${want ? 'settled' : 'still running'}`, () => {
      expect(settled(p)).toBe(want)
    })
  }
})
