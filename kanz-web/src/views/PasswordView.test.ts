import { beforeEach, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import PasswordView from './PasswordView.vue'
import { api, ApiError } from '../api/client'
import { useSession } from '../stores/session'

beforeEach(() => { vi.restoreAllMocks(); setActivePinia(createPinia()) })
async function form() {
  const w = mount(PasswordView, { global: { stubs: { RouterLink: true } } })
  const fields = w.findAll('input')
  await fields[0]!.setValue('synthetic-current')
  await fields[1]!.setValue('synthetic-replacement')
  await fields[2]!.setValue('synthetic-replacement')
  return w
}
it('submits once, clears secrets and forgets the session after success', async () => {
  let finish!: () => void
  const post = vi.spyOn(api, 'post').mockReturnValue(new Promise<void>(resolve => { finish = resolve }))
  const forget = vi.spyOn(useSession(), 'forget')
  const w = await form()
  await w.get('form').trigger('submit'); await w.get('form').trigger('submit')
  expect(post).toHaveBeenCalledTimes(1)
  expect(post).toHaveBeenCalledWith('/auth/credential', { current_credential: 'synthetic-current', new_credential: 'synthetic-replacement' })
  finish(); await flushPromises()
  expect(forget).toHaveBeenCalledTimes(1)
  expect(w.text()).toContain('Password changed')
  expect(w.find('input').exists()).toBe(false)
})
it('retains no credentials after a throttled attempt and never retries', async () => {
  const post = vi.spyOn(api, 'post').mockRejectedValue(new ApiError(429, 'limit'))
  const w = await form(); await w.get('form').trigger('submit'); await flushPromises()
  expect(w.get('[role="alert"]').text()).toContain('Too many attempts')
  expect(w.findAll('input').every(x => x.element.value === '')).toBe(true)
  expect(post).toHaveBeenCalledTimes(1)
})
it('refuses mismatched confirmation without contacting the service', async () => {
  const post = vi.spyOn(api, 'post')
  const w = await form(); await w.findAll('input')[2]!.setValue('different')
  await w.get('form').trigger('submit')
  expect(post).not.toHaveBeenCalled()
  expect(w.text()).toContain('do not match')
})
