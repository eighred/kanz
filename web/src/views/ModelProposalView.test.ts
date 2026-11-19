import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { afterEach, expect, it, vi } from 'vitest'
import ModelProposalView from './ModelProposalView.vue'
import { useSession } from '../stores/session'
import { setUnauthenticatedHandler } from '../api/client'

afterEach(() => { setUnauthenticatedHandler(() => {}); vi.unstubAllGlobals() })

it.each(['edit', 'tenant', 'unmount'])('cancels a pending read on %s and discards a late refusal', async change => {
  let finish!: (response: Response) => void
  const fetch = vi.fn().mockReturnValue(new Promise<Response>(resolve => { finish = resolve }))
  vi.stubGlobal('fetch', fetch)
  const forgotten = vi.fn()
  setUnauthenticatedHandler(forgotten)
  const pinia = createPinia()
  setActivePinia(pinia)
  const session = useSession()
  session.identity = { tenant: 'tenant', subject: 'reader', expires_at: '2099-01-01T00:00:00Z' }
  const view = mount(ModelProposalView, { global: { plugins: [pinia] } })
  const inputs = view.findAll('input')
  for (const [i, value] of ['PF', 'USD', '1', '0', 'A', '0', '1', '0.1'].entries()) await inputs[i]!.setValue(value)
  await view.get('form').trigger('submit')
  await view.get('form').trigger('submit')
  expect(fetch).toHaveBeenCalledOnce()
  const signal = fetch.mock.calls[0]![1].signal
  if (change === 'edit') await inputs[2]!.setValue('2')
  else if (change === 'tenant') session.identity.tenant = 'second'
  else view.unmount()
  expect(signal.aborted).toBe(true)
  finish(Response.json({}, { status: 401 }))
  await flushPromises()
  expect(forgotten).not.toHaveBeenCalled()
  if (change !== 'unmount') {
    expect(view.find('section').exists()).toBe(false)
    expect(view.find('[role=alert]').exists()).toBe(false)
    if (change === 'tenant') expect(inputs[0]!.element.value).toBe('')
    view.unmount()
  }
})
