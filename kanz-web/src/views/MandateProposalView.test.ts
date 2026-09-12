import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import MandateProposalView from './MandateProposalView.vue'
import * as mandatesApi from '../api/mandates'

const document = {
  mandate_id: 'PF1-TESTNET-INITIAL-001',
  tenant_id: '__system__',
  portfolio_id: 'PF1',
  version: '1',
  effective_at: '2026-09-13T12:00:00Z',
  rules: [
    {
      rule_id: 'btc-usdt-allow-only',
      type: 'RULE_TYPE_RESTRICTION',
      on_violation: 'COMPLIANCE_STATUS_BREACH',
      restriction: {
        dimension: 'DIMENSION_INSTRUMENT',
        values: ['BTC-USDT'],
        mode: 'RESTRICTION_MODE_ALLOW_ONLY',
      },
    },
  ],
}

beforeEach(() => vi.restoreAllMocks())

describe('mandate proposal', () => {
  it('shows the exact payload and sends no actor, digest, or approval', async () => {
    const propose = vi.spyOn(mandatesApi.mandates, 'propose').mockResolvedValue({
      proposal_id: 'proposal-1',
      status: 'PENDING_APPROVAL',
      portfolio_id: 'PF1',
      mandate_id: document.mandate_id,
      version: '1',
      rule_count: 1,
      proposer: 'user:alice',
      digest: 'sha256:abc',
      expires_at: '2026-09-16T12:00:00Z',
    })
    const wrapper = mount(MandateProposalView, {
      global: { stubs: { RouterLink: { template: '<a><slot /></a>' } } },
    })

    await wrapper.find('textarea[placeholder*="complete approved"]').setValue(JSON.stringify(document))
    await wrapper.findAll('textarea')[1]!.setValue('approved initial restriction')
    await wrapper.find('input[type="checkbox"]').setValue(true)
    await wrapper.find('form').trigger('submit.prevent')
    await flushPromises()

    expect(wrapper.find('.mandate-preview pre').text()).toContain('BTC-USDT')
    expect(propose).toHaveBeenCalledWith(document, 'approved initial restriction')
    expect(propose.mock.calls[0]!.length).toBe(2)
    expect(wrapper.find('[role="status"]').text()).toContain('PENDING')
    expect(wrapper.find('[role="status"]').text()).toContain('not published')
  })

  it('refuses a numeric uint64 version before it can be rounded by the browser', async () => {
    const propose = vi.spyOn(mandatesApi.mandates, 'propose')
    const wrapper = mount(MandateProposalView)
    await wrapper
      .find('textarea[placeholder*="complete approved"]')
      .setValue(JSON.stringify({ ...document, version: 1 }))

    expect(wrapper.find('[role="alert"]').text()).toContain('exact string version')
    expect(wrapper.find('button[type="submit"]').attributes('disabled')).toBeDefined()
    expect(propose).not.toHaveBeenCalled()
  })

  it('requires an explicit check after every mandate or rationale edit', async () => {
    const wrapper = mount(MandateProposalView)
    const source = wrapper.find('textarea[placeholder*="complete approved"]')
    const rationale = wrapper.findAll('textarea')[1]!
    const checked = wrapper.find('input[type="checkbox"]')

    await source.setValue(JSON.stringify(document))
    await rationale.setValue('approved initial restriction')
    await checked.setValue(true)
    expect(wrapper.find('button[type="submit"]').attributes('disabled')).toBeUndefined()

    await rationale.setValue('changed rationale')
    expect((checked.element as HTMLInputElement).checked).toBe(false)
    expect(wrapper.find('button[type="submit"]').attributes('disabled')).toBeDefined()
  })
})
