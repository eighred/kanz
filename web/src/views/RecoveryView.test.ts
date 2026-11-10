import { reactive, nextTick } from 'vue'
import { beforeEach, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { api, ApiError } from '../api/client'
import RecoveryView from './RecoveryView.vue'
import MailboxView from './MailboxView.vue'

const state = vi.hoisted(() => ({ route: { path: '/recover', hash: '' } }))
const replace = vi.hoisted(() => vi.fn())
vi.mock('vue-router', () => ({ useRoute: () => state.route, useRouter: () => ({ replace }) }))
const options = { global: { stubs: { RouterLink: true } } }
beforeEach(() => {
  vi.restoreAllMocks(); replace.mockReset(); state.route = reactive({ path: '/recover', hash: '' })
  setActivePinia(createPinia())
})
it('requests recovery by subject only and makes no account-existence claim', async () => {
  const post = vi.spyOn(api, 'post').mockResolvedValue({ status: 'accepted' })
  const w = mount(RecoveryView, options)
  await w.get('input').setValue('unknown-subject')
  await w.get('form').trigger('submit'); await flushPromises()
  expect(post).toHaveBeenCalledExactlyOnceWith('/auth/recovery', { subject: 'unknown-subject' })
  expect(w.get('[role="status"]').text()).toContain('If this account')
  expect(w.text()).toContain('does not confirm delivery')
})
it('strips a verification fragment and requires an explicit confirmation', async () => {
  state.route.path = '/verify-mailbox'; state.route.hash = '#token=synthetic-proof'
  const post = vi.spyOn(api, 'post').mockResolvedValue(undefined)
  const w = mount(RecoveryView, options)
  expect(replace).toHaveBeenCalledWith({ path: '/verify-mailbox', hash: '' })
  expect(post).not.toHaveBeenCalled()
  expect(w.html()).not.toContain('synthetic-proof')
  await w.get('form').trigger('submit'); await flushPromises()
  expect(post).toHaveBeenCalledExactlyOnceWith('/auth/mailbox/verify', { token: 'synthetic-proof' })
  expect(w.get('[role="status"]').text()).toContain('verified')
})
it('refuses mismatched resets, submits once, and clears password fields after refusal', async () => {
  state.route.hash = '#token=synthetic-proof'
  const post = vi.spyOn(api, 'post').mockRejectedValue(new ApiError(401, 'refused'))
  const w = mount(RecoveryView, options)
  const fields = w.findAll('input')
  await fields[0]!.setValue('synthetic-new-password'); await fields[1]!.setValue('different')
  await w.get('form').trigger('submit'); expect(post).not.toHaveBeenCalled()
  await fields[1]!.setValue('synthetic-new-password')
  await w.get('form').trigger('submit'); await flushPromises()
  expect(post).toHaveBeenCalledTimes(1)
  expect(fields.every(x => x.element.value === '')).toBe(true)
  expect(w.get('[role="alert"]').text()).toContain('invalid, expired, or already used')
})
it('enrollment requires a current credential and displays durable delivery status', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({ verified_address: '', pending_address: 'person@example.test', verification_delivery: 'failed' })
  const post = vi.spyOn(api, 'post').mockResolvedValue({ status: 'accepted' })
  const w = mount(MailboxView, options); await flushPromises()
  expect(w.text()).toContain('Verification delivery: failed')
  const fields = w.findAll('input')
  await fields[0]!.setValue('person@example.test'); await fields[1]!.setValue('synthetic-current-password')
  await w.get('form').trigger('submit'); await flushPromises()
  expect(post).toHaveBeenCalledExactlyOnceWith('/auth/mailbox', { address: 'person@example.test', credential: 'synthetic-current-password' })
  expect(fields[1]!.element.value).toBe('')
})

it('accepts a new recovery link when the current view is reused', async () => {
  const w = mount(RecoveryView, options)
  expect(w.findAll('input')).toHaveLength(1)
  state.route.hash = '#token=new-proof'
  await nextTick()
  expect(w.findAll('input')).toHaveLength(2)
  state.route.hash = ''
  await nextTick()
  expect(w.findAll('input')).toHaveLength(2)
  expect(w.text()).toContain('Reset password')
})

it('does not apply an old request result to a newly opened proof', async () => {
  let finish!: () => void
  vi.spyOn(api, 'post').mockReturnValue(new Promise<void>(resolve => { finish = resolve }))
  const w = mount(RecoveryView, options)
  await w.get('input').setValue('first-account')
  await w.get('form').trigger('submit')
  state.route.hash = '#token=new-proof'
  await nextTick()
  finish(); await flushPromises()
  expect(w.find('[role="status"]').exists()).toBe(false)
  expect(w.findAll('input')).toHaveLength(2)
  expect(w.get('button').attributes('disabled')).toBeUndefined()
})
