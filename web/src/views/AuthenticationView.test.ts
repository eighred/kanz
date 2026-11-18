import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import AuthenticationView from './AuthenticationView.vue'
import { api, ApiError } from '../api/client'
import * as web from '../api/webauthn'
import { useSession } from '../stores/session'

const options = { global: { stubs: { RouterLink: true } } }
beforeEach(() => { vi.restoreAllMocks(); setActivePinia(createPinia()) })
afterEach(() => { vi.useRealTimers() })
it('requires fresh verification to add or remove factors and refuses last-factor removal', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({ enabled: true, recent: false, factors: [{ id: 'key', name: 'Primary', created_at: '2026-09-01T00:00:00Z', last_used_at: null }] })
  const w = mount(AuthenticationView, options); await flushPromises()
  await w.findAll('input')[0]!.setValue('Spare'); await w.findAll('input')[1]!.setValue('synthetic-password')
  expect(w.get('button[type="submit"]').attributes('disabled')).toBeDefined()
  expect(w.get('[aria-label="Remove Primary"]').attributes('disabled')).toBeDefined()
  expect(w.text()).toContain('Email recovery cannot remove MFA')
  w.unmount()
})
it.each([
  null, {}, { enabled: false, factors: [] }, { enabled: 'false', recent: false, factors: [] },
  { enabled: true, recent: false, factors: [null] },
  { enabled: true, recent: true, factors: [] },
  { enabled: true, recent: true, verified_until: 'not-a-date', factors: [] },
  { enabled: true, recent: true, verified_until: '2099-01-01T00:00:00Z', factors: [] },
])('withholds factor controls for malformed status', async body => {
  vi.spyOn(api, 'get').mockResolvedValue(body)
  const w = mount(AuthenticationView, options)
  await flushPromises()
  expect(w.get('[role="alert"]').text()).toContain('Authentication settings are unavailable')
  expect(w.find('form').exists()).toBe(false)
  expect(w.text()).not.toContain('Recent verification is active')
  w.unmount()
})

it('keeps an expired in-flight verification inactive and clears prior status on a failed refresh', async () => {
  const get = vi.spyOn(api, 'get').mockResolvedValueOnce({ enabled: true, recent: true, verified_until: '2020-01-01T00:00:00Z', factors: [] })
  vi.spyOn(api, 'post').mockResolvedValueOnce({ id: 'challenge', options: {} }).mockResolvedValueOnce({})
  vi.spyOn(web, 'webAuthn').mockResolvedValue({ proof: 'public-test-proof' })
  vi.spyOn(useSession(), 'resolve').mockResolvedValue(undefined)
  const w = mount(AuthenticationView, options)
  await flushPromises()
  expect(w.text()).toContain('Privileged actions may require fresh verification')
  get.mockRejectedValueOnce(new ApiError(502, 'MFA status unavailable'))
  await w.get('button').trigger('click')
  await flushPromises()
  expect(w.get('[role="alert"]').text()).toContain('MFA status unavailable')
  expect(w.find('form').exists()).toBe(false)
  expect(w.text()).not.toContain('Recent verification is active')
  w.unmount()
})
it('clears the password on failure and never invokes an authenticator after a refused begin', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({ enabled: false, recent: false, factors: [] })
  const post = vi.spyOn(api, 'post').mockRejectedValue(new ApiError(429, 'Too many attempts'))
  const credential = vi.spyOn(web, 'webAuthn')
  const w = mount(AuthenticationView, options); await flushPromises()
  await w.findAll('input')[0]!.setValue('Primary'); await w.findAll('input')[1]!.setValue('synthetic-password')
  await w.get('form').trigger('submit'); await flushPromises()
  expect(post).toHaveBeenCalledExactlyOnceWith('/auth/mfa/register/begin', { name: 'Primary', password: 'synthetic-password' })
  expect(credential).not.toHaveBeenCalled(); expect(w.findAll('input')[1]!.element.value).toBe('')
  expect(w.get('[role="alert"]').text()).toContain('Too many attempts'); w.unmount()
})
it('submits one ceremony proof, refreshes the session and disables stale freshness', async () => {
  vi.useFakeTimers()
  vi.spyOn(api, 'get').mockResolvedValue({ enabled: true, recent: true, verified_until: new Date(Date.now() + 1000).toISOString(), factors: [] })
  const ceremony = { id: 'challenge', options: { publicKey: {} } }
  const post = vi.spyOn(api, 'post').mockResolvedValueOnce(ceremony).mockResolvedValueOnce({})
  vi.spyOn(web, 'webAuthn').mockResolvedValue({ proof: 'public-test-proof' })
  const resolve = vi.spyOn(useSession(), 'resolve').mockResolvedValue(undefined)
  const w = mount(AuthenticationView, options); await flushPromises()
  await w.get('button').trigger('click'); await flushPromises()
  expect(post).toHaveBeenNthCalledWith(2, '/auth/mfa/stepup/finish', { id: 'challenge', credential: { proof: 'public-test-proof' } })
  expect(resolve).toHaveBeenCalledTimes(1)
  expect(w.text()).toContain('Recent verification is active')
  await vi.advanceTimersByTimeAsync(1001)
  expect(w.text()).toContain('Privileged actions may require fresh verification')
  w.unmount(); expect(vi.getTimerCount()).toBe(0)
})
