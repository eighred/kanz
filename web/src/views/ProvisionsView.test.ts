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

async function onboardingForm() {
  const w = mount(ProvisionsView, options)
  await flushPromises()
  await w.get('button').trigger('click')
  for (const [name, value] of Object.entries({
    hostname: ' worker-1 ', ip: '192.0.2.7', user: 'fixture', hostKey: 'fixture-public-key',
  })) await w.get(`[name="${name}"]`).setValue(value)
  const files = new DataTransfer()
  files.items.add(new File(['synthetic-private-value'], 'fixture.pem', { type: 'text/plain' }))
  w.get<HTMLInputElement>('input[type=file]').element.files = files.files
  return w
}

it('submits once through the authenticated browser client and displays the confirmed run', async () => {
  let finish!: (response: Response) => void
  const fetch = vi.spyOn(globalThis, 'fetch')
    .mockResolvedValueOnce(body('PROVISION_STATUS_PENDING'))
    .mockReturnValueOnce(new Promise<Response>(resolve => { finish = resolve }))
    .mockResolvedValueOnce(body('PROVISION_STATUS_JOINED'))
  const w = await onboardingForm()
  await w.get('form').trigger('submit')
  await w.get('form').trigger('submit')
  await flushPromises()
  expect(fetch).toHaveBeenCalledTimes(2)
  const [url, request] = fetch.mock.calls[1]!
  expect(url).toBe('/api/v1/control/nodes')
  expect(request).toMatchObject({ method: 'POST', credentials: 'same-origin' })
  expect(JSON.parse(String(request?.body))).toEqual({
    hostname: 'worker-1', ip: '192.0.2.7', sshUser: 'fixture', sshHostKey: 'fixture-public-key',
    sshPrivateKey: btoa('synthetic-private-value'),
  })
  finish(Response.json({ provisionId: 'new-run' }))
  await flushPromises()
  expect(w.get('[role=status]').text()).toContain('new-run')
  expect(w.html()).not.toContain('synthetic-private-value')
  expect(w.find('form').exists()).toBe(false)
  expect(w.get('tbody').text()).toContain('joined')
  w.unmount()
})

it.each([
  [Response.json({}, { status: 200 }), 'Check the runs'],
  [Response.json({ error: 'synthetic-private-value' }, { status: 403 }), 'operator authority'],
  [Response.json({ code: 'mfa_required' }, { status: 403 }), 'Verify your security key'],
  [Response.json({ error: 'synthetic-private-value' }, { status: 503 }), 'Check the runs'],
])('requires a receipt and drops the selected key after failure', async (response, message) => {
  const fetch = vi.spyOn(globalThis, 'fetch')
    .mockResolvedValueOnce(body('PROVISION_STATUS_PENDING')).mockResolvedValueOnce(response)
  const w = await onboardingForm()
  await w.get('form').trigger('submit')
  await flushPromises()
  expect(w.get('[role=alert]').text()).toContain(message)
  expect(w.find('[role=status]').exists()).toBe(false)
  expect(w.get<HTMLInputElement>('input[type=file]').element.files?.length).toBe(0)
  expect(w.html()).not.toContain('synthetic-private-value')
  expect(fetch).toHaveBeenCalledTimes(2)
  w.unmount()
})

it('drops a late submission response after leaving the session', async () => {
  let finish!: (response: Response) => void
  const forget = vi.fn()
  setUnauthenticatedHandler(forget)
  const fetch = vi.spyOn(globalThis, 'fetch')
    .mockResolvedValueOnce(body('PROVISION_STATUS_PENDING'))
    .mockReturnValueOnce(new Promise<Response>(resolve => { finish = resolve }))
  const w = await onboardingForm()
  await w.get('form').trigger('submit')
  await flushPromises()
  const signal = fetch.mock.calls[1]![1]!.signal!
  w.unmount()
  expect(signal.aborted).toBe(true)
  finish(new Response(null, { status: 401 }))
  await flushPromises()
  expect(forget).not.toHaveBeenCalled()
  expect(fetch).toHaveBeenCalledTimes(2)
})
