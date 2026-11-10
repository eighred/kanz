import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import PreflightView from './PreflightView.vue'
import * as preflightApi from '../api/preflight'
import type { PreflightStatus } from '../api/preflight'

function evidence(status: 'PASS' | 'FAIL' | 'UNKNOWN'): PreflightStatus {
  return {
    status,
    reason: status === 'UNKNOWN' ? 'preflight evidence is stale' : undefined,
    observed_at: '2026-09-13T12:00:00Z',
    max_age_seconds: 900,
    deployed_commit: '2222222222222222222222222222222222222222',
    verifier_commit: '1111111111111111111111111111111111111111',
    command_id: 'command-1',
    checks: [
      { name: 'mandate_inventory', status, observed: { portfolios: 1, compacted_mandates: status === 'PASS' ? 1 : 0 } },
    ],
    workload_images: { oms: 'registry/oms@sha256:abc' },
  }
}

beforeEach(() => vi.restoreAllMocks())

describe('capital admission evidence', () => {
  for (const state of ['PASS', 'FAIL', 'UNKNOWN'] as const) {
    it(`renders ${state} without offering a resume action`, async () => {
      vi.spyOn(preflightApi.preflight, 'status').mockResolvedValue(evidence(state))
      const wrapper = mount(PreflightView)
      await flushPromises()

      expect(wrapper.find('.preflight-verdict').text()).toBe(state)
      expect(wrapper.text()).toContain('mandate inventory')
      expect(wrapper.text()).toContain('2222222222222222222222222222222222222222')
      expect(wrapper.findAll('button')).toHaveLength(0)
    })
  }

  it('renders stale evidence as UNKNOWN with the server reason', async () => {
    vi.spyOn(preflightApi.preflight, 'status').mockResolvedValue(evidence('UNKNOWN'))
    const wrapper = mount(PreflightView)
    await flushPromises()

    expect(wrapper.find('.preflight-summary').text()).toContain('UNKNOWN')
    expect(wrapper.find('.preflight-summary').text()).toContain('stale')
    expect(wrapper.find('.preflight-summary').classes()).toContain('state-warn')
  })

  it('maps transport failure to UNKNOWN rather than an empty or passing screen', async () => {
    vi.spyOn(preflightApi.preflight, 'status').mockRejectedValue(new Error('network detail'))
    const wrapper = mount(PreflightView)
    await flushPromises()

    expect(wrapper.find('.preflight-verdict').text()).toBe('UNKNOWN')
    expect(wrapper.text()).toContain('evidence is unreachable')
    expect(wrapper.text()).not.toContain('network detail')
  })

  it('refuses a PASS response whose controls do not all pass', async () => {
    const inconsistent = evidence('PASS')
    inconsistent.checks[0]!.status = 'FAIL'
    vi.spyOn(preflightApi.preflight, 'status').mockResolvedValue(inconsistent)
    const wrapper = mount(PreflightView)
    await flushPromises()

    expect(wrapper.find('.preflight-verdict').text()).toBe('UNKNOWN')
    expect(wrapper.text()).toContain('verdict disagrees')
  })
})
