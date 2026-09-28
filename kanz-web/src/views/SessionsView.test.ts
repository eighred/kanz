import { beforeEach, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import SessionsView from './SessionsView.vue'
import { api, ApiError } from '../api/client'
import { useSession } from '../stores/session'
const { replace } = vi.hoisted(() => ({ replace: vi.fn() }))
vi.mock('vue-router', () => ({ useRouter: () => ({ replace }) }))
const options = { global: { stubs: { RouterLink: true } } }
const current = { id: 'a'.repeat(64), created_at: '2026-09-01T00:00:00Z', expires_at: '2026-09-01T01:00:00Z', current: true }
const other = { ...current, id: 'b'.repeat(64), current: false }
beforeEach(() => { vi.restoreAllMocks(); replace.mockReset(); setActivePinia(createPinia()) })
it('revokes another session and reloads inventory without signing out this browser', async () => {
  vi.spyOn(api, 'get').mockResolvedValueOnce({ sessions: [current, other] }).mockResolvedValueOnce({ sessions: [current] })
  const post = vi.spyOn(api, 'post').mockResolvedValue({ revoked: true })
  const forget = vi.spyOn(useSession(), 'forget')
  const w = mount(SessionsView, options); await flushPromises()
  await w.findAll('tbody button')[1]!.trigger('click'); await flushPromises()
  expect(post).toHaveBeenCalledExactlyOnceWith(`/auth/sessions/${other.id}/revoke`, {})
  expect(w.findAll('tbody tr')).toHaveLength(1); expect(forget).not.toHaveBeenCalled(); w.unmount()
})
it('clears browser identity only after current-session revocation commits', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({ sessions: [current] })
  vi.spyOn(api, 'post').mockRejectedValueOnce(new ApiError(503, 'Session service unavailable')).mockResolvedValueOnce({ revoked: true })
  const forget = vi.spyOn(useSession(), 'forget')
  const w = mount(SessionsView, options); await flushPromises()
  await w.get('tbody button').trigger('click'); await flushPromises()
  expect(w.get('[role="alert"]').text()).toContain('unavailable'); expect(forget).not.toHaveBeenCalled(); expect(replace).not.toHaveBeenCalled()
  await w.get('tbody button').trigger('click'); await flushPromises()
  expect(forget).toHaveBeenCalledTimes(1); expect(replace).toHaveBeenCalledWith('/login'); w.unmount()
})
it('does not present a failed load as empty inventory', async () => {
  vi.spyOn(api, 'get').mockRejectedValue(new ApiError(503, 'Session service unavailable'))
  const w = mount(SessionsView, options); await flushPromises()
  expect(w.find('[role="alert"]').exists()).toBe(true); expect(w.text()).not.toContain('No active sessions.'); w.unmount()
})
