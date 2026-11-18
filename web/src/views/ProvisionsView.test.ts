import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ProvisionsView from './ProvisionsView.vue'
import { setUnauthenticatedHandler } from '../api/client'

const options = { global: { stubs: { RouterLink: true } } }
const body = (status: string) => Response.json({ provisions: [{ id: 'job-1', hostname: 'worker-1', status }] })
beforeEach(() => vi.useFakeTimers())
afterEach(() => {
  setUnauthenticatedHandler(() => {})
  vi.restoreAllMocks()
  vi.useRealTimers()
})

it('retains the newer terminal state when an earlier poll finishes later', async () => {
  let finish!: (response: Response) => void
  const fetch = vi.spyOn(globalThis, 'fetch')
    .mockReturnValueOnce(new Promise<Response>(resolve => { finish = resolve }))
    .mockResolvedValueOnce(body('PROVISION_STATUS_JOINED'))
  const w = mount(ProvisionsView, options)
  await vi.advanceTimersByTimeAsync(30_000)
  await flushPromises()
  expect(fetch).toHaveBeenCalledTimes(2)
  expect(w.get('tbody').text()).toContain('joined')
  finish(body('PROVISION_STATUS_PENDING'))
  await flushPromises()
  expect(w.get('tbody').text()).toContain('joined')
  expect(w.text()).not.toContain('pending')
  w.unmount()
})

it('aborts the wire read on exit before a late 401 can forget the current session', async () => {
  let finish!: (response: Response) => void
  const fetch = vi.spyOn(globalThis, 'fetch').mockReturnValueOnce(new Promise<Response>(resolve => { finish = resolve }))
  const forget = vi.fn()
  setUnauthenticatedHandler(forget)
  const w = mount(ProvisionsView, options)
  const signal = fetch.mock.calls[0]![1]!.signal!
  expect(signal.aborted).toBe(false)
  w.unmount()
  expect(signal.aborted).toBe(true)
  finish(new Response(null, { status: 401 }))
  await flushPromises()
  await vi.advanceTimersByTimeAsync(60_000)
  expect(forget).not.toHaveBeenCalled()
  expect(fetch).toHaveBeenCalledOnce()
})

it('reports a current upstream failure and recovers on the next poll', async () => {
  vi.spyOn(globalThis, 'fetch')
    .mockResolvedValueOnce(Response.json({ error: 'upstream unavailable' }, { status: 503 }))
    .mockResolvedValueOnce(body('PROVISION_STATUS_FAILED'))
  const w = mount(ProvisionsView, options)
  await flushPromises()
  expect(w.get('[role=alert]').text()).toContain('control plane is unreachable')
  await vi.advanceTimersByTimeAsync(30_000)
  await flushPromises()
  expect(w.find('[role=alert]').exists()).toBe(false)
  expect(w.get('tbody').text()).toContain('failed')
  w.unmount()
})
