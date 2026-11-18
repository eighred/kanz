import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import CustodyBreaksView from './CustodyBreaksView.vue'
import { custody, type CustodyBreak, type CustodyEvidence } from '../api/custody'
import { ApiError } from '../api/client'

afterEach(() => vi.restoreAllMocks())
const row: CustodyBreak = { break_id: 'PF|CUST|quantity|BTC/USD', kind: 'quantity', key: 'BTC/USD', ibor: '1.000000000001', custodian: '1', difference: '0.000000000001', status: 'open', first_seen_at: '2026-01-01T00:00:00Z', last_seen_at: '2026-01-02T00:00:00Z', status_changed_at: '2026-01-01T00:00:00Z', age_seconds: 86400, revision: '1', values_state: 'exact', actions_enabled: true }
const receipt: CustodyEvidence = { request_id: 'test-request', break_id: row.break_id, actor: 'verified-human', action: 'claim', recorded_at: '2026-01-02T01:00:00Z', before: { status: 'open', assignee: '', explanation: '', revision: '1' }, after: { status: 'assigned', assignee: 'verified-human', explanation: '', revision: '2' }, values: { ibor: row.ibor, custodian: row.custodian, difference: row.difference } }

function button(wrapper: ReturnType<typeof mount>, text: string) {
  const result = wrapper.findAll('button').find((b) => b.text() === text)
  if (!result) throw new Error(`Missing button ${text}`)
  return result
}

describe('custody exact review', () => {
  it('requires review, preserves the retry identity, and reloads after evidence', async () => {
    const read = vi.spyOn(custody, 'breaks').mockResolvedValue([row])
    const act = vi.spyOn(custody, 'act').mockRejectedValueOnce(new Error('network lost')).mockResolvedValueOnce(receipt)
    const wrapper = mount(CustodyBreaksView, { attachTo: document.body })
    await flushPromises()
    await button(wrapper, 'Claim').trigger('click')
    expect(act).not.toHaveBeenCalled()
    await button(wrapper, 'Review action').trigger('click')
    expect(wrapper.find('[aria-label="Custody action review"]').text()).toContain('0.000000000001')
    expect(act).not.toHaveBeenCalled()
    await button(wrapper, 'Confirm reviewed action').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('Retry this same reviewed request')
    const first = act.mock.calls[0]?.[1]
    expect(document.activeElement).toBe(button(wrapper, 'Confirm reviewed action').element)
    expect(first?.review_contract).toBe('exact-v1')
    expect(first).not.toHaveProperty('actor')
    expect(first).not.toHaveProperty('assignee')
    await button(wrapper, 'Confirm reviewed action').trigger('click')
    await flushPromises()
    expect(act.mock.calls[1]?.[1]).toEqual(first)
    expect(read).toHaveBeenCalledTimes(2)
    expect(wrapper.find('[role="status"]').text()).toContain('verified-human')
    expect(document.activeElement).toBe(wrapper.get('[role="status"]').element)
    expect(wrapper.find('[aria-label="Custody action review"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('moves keyboard focus through both reviews and returns to the invoking row on cancel', async () => {
    vi.spyOn(custody, 'breaks').mockResolvedValue([row, { ...row, break_id: 'PF|CUST|quantity|ETH/USD', key: 'ETH/USD' }])
    const act = vi.spyOn(custody, 'act')
    const wrapper = mount(CustodyBreaksView, { attachTo: document.body })
    try {
      await flushPromises()
      for (const action of ['Claim', 'Explain']) {
        const trigger = wrapper.findAll('tbody button').filter(b => b.text() === action)[1]!
        trigger.element.focus()
        await trigger.trigger('click')
        expect(document.activeElement).toBe(action === 'Explain' ? wrapper.get('textarea').element : button(wrapper, 'Review action').element)
        if (action === 'Explain') await wrapper.get('textarea').setValue('Cash settlement in progress')
        await button(wrapper, 'Review action').trigger('click')
        expect(document.activeElement).toBe(button(wrapper, 'Confirm reviewed action').element)
        await button(wrapper, 'Cancel').trigger('click')
        expect(document.activeElement).toBe(trigger.element)
        expect(act).not.toHaveBeenCalled()
      }
    } finally { wrapper.unmount() }
  })

  it('requires a fresh review after a revision conflict', async () => {
    vi.spyOn(custody, 'breaks').mockResolvedValue([row])
    vi.spyOn(custody, 'act').mockRejectedValue(new ApiError(409, 'private database detail'))
    const wrapper = mount(CustodyBreaksView, { attachTo: document.body })
    await flushPromises()
    await button(wrapper, 'Explain').trigger('click')
    expect(button(wrapper, 'Review action').attributes('disabled')).toBeDefined()
    await wrapper.find('textarea').setValue('Settlement remains pending')
    await button(wrapper, 'Review action').trigger('click')
    await button(wrapper, 'Confirm reviewed action').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('review a fresh action')
    expect(document.activeElement).toBe(wrapper.get('[role="alert"]').element)
    expect(wrapper.text()).not.toContain('private database detail')
    expect(wrapper.find('[aria-label="Custody action review"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('does not offer actions or present legacy rounded numbers as exact', async () => {
    vi.spyOn(custody, 'breaks').mockResolvedValue([{ ...row, values_state: 'legacy_unverified', actions_enabled: false }])
    const wrapper = mount(CustodyBreaksView)
    await flushPromises()
    expect(wrapper.text()).toContain('source replay and reconciliation required')
    expect(wrapper.text()).not.toContain('1.000000000001')
    expect(wrapper.findAll('button').some((b) => b.text() === 'Claim')).toBe(false)
  })
})
