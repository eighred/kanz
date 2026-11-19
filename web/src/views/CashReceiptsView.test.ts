import { mount, flushPromises } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { afterEach, expect, it, vi } from 'vitest'
import { setUnauthenticatedHandler } from '../api/client'
import { useSession } from '../stores/session'
import CashReceiptsView from './CashReceiptsView.vue'

afterEach(() => {
  setUnauthenticatedHandler(() => {})
  vi.unstubAllGlobals()
})

it.each(['portfolio', 'movement', 'session', 'tenant', 'unmount'])('discards a pending lookup after changing %s', async change => {
  let finish!: (response: Response) => void
  const fetch = vi.fn().mockReturnValue(new Promise<Response>(resolve => { finish = resolve }))
  vi.stubGlobal('fetch', fetch)
  const forgotten = vi.fn()
  setUnauthenticatedHandler(forgotten)
  const pinia = createPinia()
  setActivePinia(pinia)
  const session = useSession()
  session.identity = { subject: 'fund-reader', tenant: 'first', expires_at: '2099-01-01T00:00:00Z' }
  const view = mount(CashReceiptsView, { global: { plugins: [pinia] } })
  const [pf, movement] = view.findAll('input')
  await pf!.setValue('PF')
  await movement!.setValue('M1')
  await view.get('form').trigger('submit')
  expect(fetch).toHaveBeenCalledOnce()
  const signal = fetch.mock.calls[0]![1].signal
  if (change === 'session') session.identity = { ...session.identity, subject: 'second-reader' }
  else if (change === 'tenant') session.identity.tenant = 'second'
  else if (change === 'unmount') view.unmount()
  else await (change === 'portfolio' ? pf! : movement!).setValue('new')
  expect(signal.aborted).toBe(true)
  finish(Response.json({ error: 'stale request' }, { status: 401 }))
  await flushPromises()
  expect(forgotten).not.toHaveBeenCalled()
  if (change !== 'unmount') {
    expect(view.find('[role="alert"]').exists()).toBe(false)
    expect(view.find('section').exists()).toBe(false)
    if (change === 'session' || change === 'tenant') expect([pf!.element.value, movement!.element.value]).toEqual(['', ''])
    view.unmount()
  }
})
