import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { router } from './index'
import * as client from '../api/client'
import { Unauthenticated } from '../api/client'

const identity = { subject: 'user:alice', tenant: 'acme', expires_at: '2099-01-01T00:00:00Z' }

/** signedOut installs the answer /auth/me gives a visitor with no session. */
function signedOut() {
  return vi.spyOn(client.auth, 'me').mockRejectedValue(new Unauthenticated())
}

function signedIn() {
  return vi.spyOn(client.auth, 'me').mockResolvedValue(identity)
}

beforeEach(async () => {
  setActivePinia(createPinia())
  // Park on a PUBLIC route first. Landing anywhere guarded would run the guard
  // before a test has said who this browser is, and the guard would reach for a
  // real network.
  signedOut()
  await router.replace('/login')
  await router.isReady()
  vi.restoreAllMocks()
})

describe('the sign-in guard', () => {
  it.each(['/overview', '/audit', '/missing-page'])('protects %s from signed-out access', async (path) => {
    signedOut()
    await router.push(path)
    expect(router.currentRoute.value.name).toBe('login')
    expect(router.currentRoute.value.query.next).toBe(path)
  })

  it('shows a recovery page for an unknown signed-in route', async () => {
    signedIn()
    await router.push('/missing-page')
    expect(router.currentRoute.value.name).toBe('not-found')
  })

  it('sends a signed-out visitor to /login and remembers where they were going', async () => {
    signedOut()
    await router.push('/venues')
    expect(router.currentRoute.value.name).toBe('login')
    expect(router.currentRoute.value.query.next).toBe('/venues')
  })

  it('lets a signed-in operator through', async () => {
    signedIn()
    await router.push('/venues')
    expect(router.currentRoute.value.path).toBe('/venues')
  })

  it('lets a signed-out visitor reach /redeem, which is public by necessity', async () => {
    // Whoever opens this holds an invitation and nothing else. Requiring a
    // session to accept one would be a loop with no entry.
    const me = signedOut()
    await router.push('/redeem?token=abc')
    expect(router.currentRoute.value.name).toBe('redeem')
    // And it does not even ask the server who they are — there is no point.
    expect(me).not.toHaveBeenCalled()
  })

  it('sends / to the workspace', async () => {
    signedIn()
    await router.push('/')
    expect(router.currentRoute.value.path).toBe('/overview')
  })

  // ONE QUESTION PER NAVIGATION, not one per guarded route. resolve() caches,
  // so a session lookup is not repeated on every click — and a guard that
  // re-asked would make the whole app wait on the network to change screens.
  it('asks the server who this is only once across several navigations', async () => {
    const me = signedIn()
    await router.push('/venues')
    await router.push('/login')
    await router.push('/venues')
    expect(me).toHaveBeenCalledOnce()
  })
})
