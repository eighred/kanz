import { beforeEach, expect, it, vi } from 'vitest'
import { api, auth } from './client'
import * as web from './webauthn'
beforeEach(() => vi.restoreAllMocks())
it('finishes MFA before returning a browser identity', async () => {
  const ceremony = { id: 'challenge', options: { publicKey: {} } }
  const identity = { subject: 'person', tenant: 'tenant', expires_at: '2099-01-01' }
  const post = vi.spyOn(api, 'post').mockResolvedValueOnce({ mfa: ceremony }).mockResolvedValueOnce(identity)
  const proof = vi.spyOn(web, 'webAuthn').mockResolvedValue({ signed: 'test-proof' })
  expect(await auth.login('person', 'synthetic-password')).toEqual(identity)
  expect(proof).toHaveBeenCalledWith(ceremony, false)
  expect(post).toHaveBeenLastCalledWith('/auth/mfa/login/finish', { id: 'challenge', credential: { signed: 'test-proof' } })
})
it('cancellation creates no login completion or retry', async () => {
  const post = vi.spyOn(api, 'post').mockResolvedValue({ mfa: { id: 'challenge', options: { publicKey: {} } } })
  vi.spyOn(web, 'webAuthn').mockRejectedValue(new web.WebAuthnError())
  await expect(auth.login('person', 'synthetic-password')).rejects.toBeInstanceOf(web.WebAuthnError)
  expect(post).toHaveBeenCalledTimes(1)
})
