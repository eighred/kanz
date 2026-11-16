import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { useSession } from './session'
import { Unauthenticated } from '../api/client'
import * as client from '../api/client'

const identity = { subject: 'user:alice', tenant: 'acme', expires_at: '2099-01-01T00:00:00Z' }

beforeEach(() => {
  setActivePinia(createPinia())
})

describe('resolving who this browser is', () => {
  it('treats a 401 from /auth/me as signed out, not as an error to surface', async () => {
    // Every signed-out visitor gets a 401 here. Letting it propagate would put
    // an error banner in front of the sign-in form.
    vi.spyOn(client.auth, 'me').mockRejectedValue(new Unauthenticated())
    const s = useSession()
    await s.resolve()
    expect(s.signedIn).toBe(false)
    expect(s.resolved).toBe(true)
  })

  it('propagates a real failure rather than reporting the user as signed out', async () => {
    // A 502 means the BFF could not answer. Reporting that as "signed out"
    // would send the operator to sign in again during an outage, and the
    // successful-looking login that follows would fail for a different reason.
    vi.spyOn(client.auth, 'me').mockRejectedValue(new client.ApiError(502, 'gateway down'))
    const s = useSession()
    await expect(s.resolve()).rejects.toBeInstanceOf(client.ApiError)
  })

  it('asks the server only once', async () => {
    const me = vi.spyOn(client.auth, 'me').mockResolvedValue(identity)
    const s = useSession()
    await s.resolve()
    await s.resolve()
    expect(me).toHaveBeenCalledOnce()
  })
})

describe('signing in and out', () => {
  it('records identity after a credential sign-in', async () => {
    vi.spyOn(client.auth, 'login').mockResolvedValue(identity)
    const s = useSession()
    await s.signIn('user:alice', 'a-long-enough-passphrase')
    expect(s.signedIn).toBe(true)
    expect(s.identity?.tenant).toBe('acme')
  })

  it('records identity after accepting an invitation, without a second sign-in', async () => {
    // Redeeming sets the credential AND signs in. Asking for the password one
    // second after choosing it is the kind of step people abandon.
    vi.spyOn(client.auth, 'redeem').mockResolvedValue(identity)
    const s = useSession()
    await s.redeem('an-invite-token', 'a-long-enough-passphrase')
    expect(s.signedIn).toBe(true)
  })

  it('forgets the identity without calling the server', async () => {
    // forget() is what a 401 triggers: the server has already disowned this
    // session, so calling logout would be a request that cannot succeed.
    const logout = vi.spyOn(client.auth, 'logout').mockResolvedValue(undefined)
    vi.spyOn(client.auth, 'login').mockResolvedValue(identity)
    const s = useSession()
    await s.signIn('user:alice', 'a-long-enough-passphrase')

    s.forget()
    expect(s.signedIn).toBe(false)
    expect(logout).not.toHaveBeenCalled()
  })

  it('stays signed out after a failed sign-in', async () => {
    vi.spyOn(client.auth, 'login').mockRejectedValue(new Unauthenticated())
    const s = useSession()
    await expect(s.signIn('user:alice', 'wrong')).rejects.toBeInstanceOf(Unauthenticated)
    expect(s.signedIn).toBe(false)
  })
})

// THE STORE HOLDS IDENTITY, NEVER AUTHORITY.
//
// The gateway's capability mux decides what this caller may do, on every call.
// If a role or permission list ever appears in this store, some screen will
// start deciding access from it — and a client-side check is a courtesy, not a
// control. This test exists to make that drift fail loudly.
it('carries no roles, permissions or token', async () => {
  vi.spyOn(client.auth, 'login').mockResolvedValue(identity)
  const s = useSession()
  await s.signIn('user:alice', 'a-long-enough-passphrase')

  const keys = Object.keys(s.identity ?? {})
  for (const forbidden of ['roles', 'permissions', 'scopes', 'token', 'access_token']) {
    expect(keys).not.toContain(forbidden)
  }
})
