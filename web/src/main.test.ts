// THE WIRING NOTHING TESTED (#532).
//
// Three tests already prove the three halves of session expiry:
//
//   api/client.test.ts   a 401 calls the registered handler
//   stores/session.test.ts  forget() clears the identity
//   router/guard.test.ts    a signed-out visitor is sent to /login
//
// and none of them executes the path that joins them. main.ts is the only place
// that connects the client's 401 detection to the store reset and the
// navigation, and it had no test at all. Delete the setUnauthenticatedHandler
// registration and every one of those three suites stays green while the
// application does exactly what the comment in main.ts says it must not: leaves
// the store saying "signed in", so the guard waves the user through and every
// call on the page fails.
//
// That matters more now than it did. WEB_BFF_SESSION_TTL clamps a session to an
// hour, so this path runs for every user every hour — it is the ordinary case,
// not the edge one.
//
// IT IMPORTS main.ts RATHER THAN A FUNCTION EXTRACTED FROM IT. Extracting the
// closure to make it testable would move the untested line rather than test it:
// whatever main.ts is left calling is then the thing no test executes. happy-dom
// gives us a document, so the composition root can be run as it actually runs.

import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest'

const identity = { subject: 'user:alice', tenant: 'acme', expires_at: '2099-01-01T00:00:00Z' }

/** answer is the next fetch response the SPA sees. */
function answer(status: number, body: unknown = {}) {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
  } as unknown as Response
}

beforeEach(() => {
  document.body.innerHTML = '<div id="app"></div>'
  vi.resetModules()
})

afterEach(() => {
  vi.unstubAllGlobals()
  document.body.innerHTML = ''
})

describe('the composition root wires session expiry', () => {
  it('sends a signed-in user to /login when a call is refused', async () => {
    // Signed in when the app boots: /auth/me answers with an identity.
    const fetchMock = vi.fn().mockResolvedValue(answer(200, identity))
    vi.stubGlobal('fetch', fetchMock)

    const { router } = await import('./router')
    const { useSession } = await import('./stores/session')
    const { api } = await import('./api/client')
    await import('./main') // registers the handler and mounts

    await router.isReady()
    const session = useSession()
    await session.resolve()
    expect(session.signedIn).toBe(true)

    await router.push('/portfolios')
    await router.isReady()

    // The session ends server-side while the page sits open — a token that
    // expired, or an account an operator disabled.
    fetchMock.mockResolvedValue(answer(401, { error: 'not authenticated' }))
    await expect(api.get('/api/v1/portfolios')).rejects.toThrow()

    expect(session.signedIn).toBe(false)
    // The handler navigates without awaiting — `void router.push(...)` — because
    // it runs inside a synchronous 401 callback. So the assertion waits for the
    // navigation the production code deliberately does not.
    await vi.waitFor(() => expect(router.currentRoute.value.name).toBe('login'))
    expect(router.currentRoute.value.query.next).toBe('/portfolios')
  })

  it('does not fight the guard when a signed-out visitor is refused', async () => {
    // /auth/me answering 401 is the ORDINARY case for a visitor with no
    // session. A redirect from inside that call would fight the router guard
    // that issued it, so the handler must stay quiet.
    const fetchMock = vi.fn().mockResolvedValue(answer(401, { error: 'not authenticated' }))
    vi.stubGlobal('fetch', fetchMock)

    const { router } = await import('./router')
    const { useSession } = await import('./stores/session')
    await import('./main')

    await router.push('/login')
    await router.isReady()

    const session = useSession()
    await session.resolve()

    expect(session.signedIn).toBe(false)
    expect(router.currentRoute.value.name).toBe('login')
    // Deliberately NOT asserting the absence of `next` here. The router is a
    // module singleton and the case above leaves history behind it, so that
    // assertion would be reading the previous test rather than this one — a
    // test that can fail for a reason it does not name is worse than one
    // assertion short.
  })
})
