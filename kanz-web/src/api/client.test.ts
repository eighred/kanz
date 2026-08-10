import { beforeEach, describe, expect, it, vi } from 'vitest'
import { api, auth, ApiError, Unauthenticated, setUnauthenticatedHandler } from './client'

// WHAT IS ACTUALLY WORTH TESTING IN A BFF CLIENT is not that fetch was called —
// it is the properties the whole session design rests on. Each test here fails
// only if one of those is broken.

function respond(status: number, body: unknown, ok = status >= 200 && status < 300) {
  return {
    status,
    ok,
    json: async () => body,
  } as Response
}

let fetchMock: ReturnType<typeof vi.fn>

beforeEach(() => {
  fetchMock = vi.fn()
  vi.stubGlobal('fetch', fetchMock)
  setUnauthenticatedHandler(() => {})
})

describe('the browser never holds a token', () => {
  // THE CENTRAL PROPERTY OF THE BFF DESIGN. The session is an httpOnly cookie
  // the browser cannot read; the bearer is attached server-side when the BFF
  // proxies to the gateway. A token this code could set is a token an XSS could
  // steal — and a kanz token carries kanz-trader, which places orders.
  it('never sends an Authorization header', async () => {
    fetchMock.mockResolvedValue(respond(200, { ok: true }))
    await api.get('/api/v1/anything')
    await auth.login('user:alice', 'a-long-enough-passphrase')

    for (const [, init] of fetchMock.mock.calls) {
      const headers = (init?.headers ?? {}) as Record<string, string>
      const names = Object.keys(headers).map((h) => h.toLowerCase())
      expect(names).not.toContain('authorization')
    }
  })

  it('sends credentials same-origin, so the cookie travels without CORS', async () => {
    fetchMock.mockResolvedValue(respond(200, {}))
    await api.get('/api/v1/anything')
    const init = fetchMock.mock.calls[0]?.[1] as RequestInit | undefined
    expect(init).toMatchObject({ credentials: 'same-origin' })
  })
})

describe('refusals', () => {
  it('raises Unauthenticated on 401 and tells the session holder', async () => {
    // A 401 can arrive at any moment — a session expires, or is revoked while a
    // page sits open. If nothing is told, the app keeps claiming to be signed in
    // and every call fails: an application that looks broken rather than one
    // that asks the user to sign in again.
    const forget = vi.fn()
    setUnauthenticatedHandler(forget)
    fetchMock.mockResolvedValue(respond(401, {}, false))

    await expect(api.get('/api/v1/anything')).rejects.toBeInstanceOf(Unauthenticated)
    expect(forget).toHaveBeenCalledOnce()
  })

  it('surfaces the server error message when it is JSON we recognise', async () => {
    // Redemption relays a real reason ("a credential must be at least 12
    // characters"). Losing it here would put the invitee back to guessing.
    fetchMock.mockResolvedValue(respond(400, { error: 'a credential must be at least 12 characters' }, false))

    await expect(auth.redeem('t', 'short')).rejects.toMatchObject({
      status: 400,
      message: 'a credential must be at least 12 characters',
    })
  })

  it('does NOT surface a body that is not the shape we expect', async () => {
    // An upstream error page is not a message for a user, and may carry
    // internals. The generic message is the correct answer.
    fetchMock.mockResolvedValue({
      status: 502,
      ok: false,
      json: async () => {
        throw new SyntaxError('not json')
      },
    } as unknown as Response)

    const err = (await api.get('/api/v1/anything').catch((e) => e)) as ApiError
    expect(err).toBeInstanceOf(ApiError)
    expect(err.message).toBe('request failed (502)')
  })

  it('distinguishes a throttle from a rejection, so a correct password is not called wrong', async () => {
    fetchMock.mockResolvedValue(respond(429, { error: 'too many attempts, try again shortly' }, false))
    const err = (await auth.login('user:alice', 'a-long-enough-passphrase').catch((e) => e)) as ApiError
    expect(err).toBeInstanceOf(ApiError)
    expect(err).not.toBeInstanceOf(Unauthenticated)
    expect(err.status).toBe(429)
  })
})
