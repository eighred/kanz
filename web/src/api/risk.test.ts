import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from './client'
import { describeRisk, risk } from './risk'

// THE RISK ROUTES SERIALISE DIFFERENTLY FROM THE CONTROL ROUTES, and this file
// exists partly to pin that. control.ts is protojson with default options —
// lowerCamelCase, zero values omitted — while these use UseProtoNames +
// EmitDefaultValues, so they are snake_case with zeros present. A shared type
// across both would make one of those readings wrong somewhere, silently.

describe('the portfolios list reads the risk surface shape', () => {
  let body: string
  beforeEach(() => {
    vi.stubGlobal('fetch', () =>
      Promise.resolve(new Response(body, { status: 200, headers: { 'content-type': 'application/json' } })),
    )
  })
  afterEach(() => vi.unstubAllGlobals())

  it('decodes snake_case fields with zeros present', async () => {
    body = JSON.stringify({
      portfolios: [
        { portfolio_id: 'PF1', display_name: 'Flagship', base_currency: 'USD', as_of: '2026-08-11T00:00:00Z', position_count: 0 },
      ],
      owner_tenant: 'acme',
    })

    const rows = await risk.portfolios()
    expect(rows).toHaveLength(1)
    expect(rows[0]?.portfolio_id).toBe('PF1')
    // A zero position count ARRIVES as 0 on this surface rather than being
    // omitted. Reading it with `?? '—'` — correct on the control routes — would
    // render an empty portfolio as unknown.
    expect(rows[0]?.position_count).toBe(0)
  })

  // An engine that has folded nothing returns no portfolios field at all.
  // Treating that as an error would put a message where a legitimate empty
  // state belongs.
  it('reads an absent portfolios field as an empty list', async () => {
    body = JSON.stringify({ owner_tenant: 'acme' })
    await expect(risk.portfolios()).resolves.toEqual([])
  })
})

describe('failures a reader can act on', () => {
  // THE IMPORTANT ONE. The gateway answers 404 for "no such portfolio", for
  // "not yours", AND — on the list route — for an engine serving another
  // tenant. It is one status deliberately, so it cannot be used to enumerate.
  // None of those is "you have no portfolios", and an empty table would say
  // exactly that.
  it('does not let a 404 read as an empty book', () => {
    const msg = describeRisk(new ApiError(404, 'portfolio not found'))
    expect(msg).toMatch(/not the same as the fund having none/i)
    expect(msg).not.toBe('portfolio not found')
  })

  it('separates missing authority from missing data', () => {
    expect(describeRisk(new ApiError(403, 'insufficient capability'))).toMatch(/read authority/i)
  })

  it('says an unreachable engine says nothing about the portfolios', () => {
    expect(describeRisk(new ApiError(503, 'upstream unavailable'))).toMatch(/says nothing about the portfolios/i)
  })

  it('passes an unrecognised status through rather than inventing an explanation', () => {
    expect(describeRisk(new ApiError(418, 'teapot'))).toBe('teapot')
  })
})
