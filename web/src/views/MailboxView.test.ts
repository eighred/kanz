import { flushPromises, mount } from '@vue/test-utils'
import { expect, it, vi } from 'vitest'
import { api, ApiError } from '../api/client'
import MailboxView from './MailboxView.vue'

function mailbox(address: string) {
  return { verified_address: address, pending_address: '', verification_delivery: 'verified' }
}

it('clears a failed status read after retry and preserves separate command feedback', async () => {
  vi.spyOn(api, 'get').mockRejectedValueOnce(new Error('offline'))
    .mockResolvedValue(mailbox('verified@example.test'))
  vi.spyOn(api, 'post').mockRejectedValue(new ApiError(429, 'rate limited'))
  const wrapper = mount(MailboxView)
  await flushPromises()
  expect(wrapper.text()).toContain('Mailbox status could not be loaded')

  await wrapper.find('input[type=email]').setValue('new@example.test')
  await wrapper.find('input[type=password]').setValue('fixture-only')
  await wrapper.find('form').trigger('submit')
  await flushPromises()
  await wrapper.get('button[type=button]').trigger('click')
  await flushPromises()
  expect(wrapper.text()).toContain('verified@example.test')
  expect(wrapper.text()).not.toContain('could not be loaded')
  expect(wrapper.get('[role=alert]').text()).toContain('Too many attempts')
})

it('keeps the latest verified address when an earlier read completes last', async () => {
  let finish!: (value: ReturnType<typeof mailbox>) => void
  vi.spyOn(api, 'get').mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
    .mockResolvedValueOnce(mailbox('current@example.test'))
  const wrapper = mount(MailboxView)
  await wrapper.get('button[type=button]').trigger('click')
  await flushPromises()
  finish(mailbox('old@example.test'))
  await flushPromises()
  expect(wrapper.text()).toContain('current@example.test')
  expect(wrapper.text()).not.toContain('old@example.test')
})
