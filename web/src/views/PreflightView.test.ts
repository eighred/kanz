import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import PreflightView from './PreflightView.vue'
import { api } from '../api/client'
import served from '../../../kanz/services/web_bff/internal/server/testdata/preflight_pass.json'

function evidence(status: 'PASS' | 'FAIL' | 'UNKNOWN') {
  const wire = structuredClone(served)
  if (status === 'FAIL') wire.checks[0]!.status = 'FAIL'
  return {
    ...wire, status,
    ...(status === 'UNKNOWN' && { reason: 'preflight evidence is stale', age_seconds: 901 }),
  }
}

beforeEach(() => vi.restoreAllMocks())

describe('capital admission evidence', () => {
  for (const state of ['PASS', 'FAIL', 'UNKNOWN'] as const) {
    it(`renders ${state} without offering a resume action`, async () => {
      vi.spyOn(api, 'get').mockResolvedValue(evidence(state))
      const wrapper = mount(PreflightView)
      await flushPromises()

      expect(wrapper.find('.preflight-verdict').text()).toBe(state)
      expect(wrapper.text()).toContain('mandate inventory')
      expect(wrapper.text()).toContain('2222222222222222222222222222222222222222')
      expect(wrapper.findAll('button')).toHaveLength(0)
    })
  }

  it('renders stale evidence as UNKNOWN with the server reason', async () => {
    vi.spyOn(api, 'get').mockResolvedValue(evidence('UNKNOWN'))
    const wrapper = mount(PreflightView)
    await flushPromises()

    expect(wrapper.find('.preflight-summary').text()).toContain('UNKNOWN')
    expect(wrapper.find('.preflight-summary').text()).toContain('stale')
    expect(wrapper.find('.preflight-summary').classes()).toContain('state-warn')
  })

  it('maps transport failure to UNKNOWN rather than an empty or passing screen', async () => {
    vi.spyOn(api, 'get').mockRejectedValue(new Error('network detail'))
    const wrapper = mount(PreflightView)
    await flushPromises()

    expect(wrapper.find('.preflight-verdict').text()).toBe('UNKNOWN')
    expect(wrapper.text()).toContain('evidence is unreachable')
    expect(wrapper.text()).not.toContain('network detail')
  })

  it('refuses a PASS response whose controls do not all pass', async () => {
    const inconsistent = evidence('PASS')
    inconsistent.checks[0]!.status = 'FAIL'
    vi.spyOn(api, 'get').mockResolvedValue(inconsistent)
    const wrapper = mount(PreflightView)
    await flushPromises()

    expect(wrapper.find('.preflight-verdict').text()).toBe('UNKNOWN')
    expect(wrapper.text()).toContain('verdict disagrees')
  })

  it('refuses a lone passing control presented as a completed preflight', async () => {
    vi.spyOn(api, 'get').mockResolvedValue({ ...served, checks: [served.checks[0]] })
    const wrapper = mount(PreflightView)
    await flushPromises()
    expect(wrapper.find('.preflight-verdict').text()).toBe('UNKNOWN')
    expect(wrapper.text()).toContain('control set is incomplete')
    wrapper.unmount()
  })
})

it('renders a reported PASS without any control evidence as UNKNOWN', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({ ...evidence('PASS'), checks: [] })
  const wrapper = mount(PreflightView)
  await flushPromises()
  expect(wrapper.find('.preflight-verdict').text()).toBe('UNKNOWN')
  expect(wrapper.text()).toContain('no controls')
  wrapper.unmount()
})

it('keeps a newer unavailable verdict when an older passing poll finishes', async () => {
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
  let finish!: (result: unknown) => void
  vi.spyOn(api, 'get')
    .mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
    .mockRejectedValueOnce(new Error('offline'))
  const wrapper = mount(PreflightView)
  try {
    await vi.advanceTimersByTimeAsync(30_000)
    await flushPromises()
    expect(wrapper.find('.preflight-verdict').text()).toBe('UNKNOWN')
    finish(evidence('PASS'))
    await flushPromises()
    expect(wrapper.find('.preflight-verdict').text()).toBe('UNKNOWN')
    expect(wrapper.text()).toContain('unreachable')
  } finally {
    wrapper.unmount()
    vi.useRealTimers()
  }
})
