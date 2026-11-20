import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import CustodyBreaksView from './CustodyBreaksView.vue'
import { custody, type CustodyBreak, type CustodyDecision, type CustodyEvidence } from '../api/custody'
import { api, ApiError } from '../api/client'

afterEach(() => vi.restoreAllMocks())
const row: CustodyBreak = { break_id: 'PF|CUST|quantity|BTC/USD', kind: 'quantity', key: 'BTC/USD', ibor: '1.000000000001', custodian: '1', difference: '0.000000000001', status: 'open', first_seen_at: '2026-01-01T00:00:00Z', last_seen_at: '2026-01-02T00:00:00Z', status_changed_at: '2026-01-01T00:00:00Z', age_seconds: 86400, revision: '1', values_state: 'exact', actions_enabled: true }
const receipt: CustodyEvidence = { request_id: 'test-request', break_id: row.break_id, actor: 'verified-human', action: 'claim', recorded_at: '2026-01-02T01:00:00Z', before: { status: 'open', assignee: '', explanation: '', revision: '1' }, after: { status: 'assigned', assignee: 'verified-human', explanation: '', revision: '2' }, values: { ibor: row.ibor, custodian: row.custodian, difference: row.difference } }

function button(wrapper: ReturnType<typeof mount>, text: string) {
  const result = wrapper.findAll('button').find((b) => b.text() === text)
  if (!result) throw new Error(`Missing button ${text}`)
  return result
}

describe('custody exact review', () => {
  it('recovers a failed comparison with the same reviewed identity and shows the committed receipt', async () => {
    vi.spyOn(custody, 'queue').mockResolvedValue({ breaks: [], rerunActor: 'operator' })
    const compare = vi.spyOn(custody, 'rerun').mockRejectedValueOnce(new ApiError(503, 'pending')).mockImplementationOnce(async req => ({
      ...req, actor: 'operator', run_id: 'retained-run', completed_at: '2020-02-05T10:00:00Z', outcome: 'no_statement',
    }))
    const wrapper = mount(CustodyBreaksView)
    try {
      await flushPromises()
      const form = wrapper.get('form')
      for (const [i, value] of ['PF1', 'CUST-A', '2020-02-04'].entries()) await form.findAll('input')[i]!.setValue(value)
      await form.trigger('submit')
      expect(compare).not.toHaveBeenCalled()
      await button(wrapper, 'Run comparison').trigger('click')
      await flushPromises()
      await button(wrapper, 'Retry same request').trigger('click')
      await flushPromises()
      expect(compare.mock.calls[0]).toEqual(compare.mock.calls[1])
      expect(wrapper.get('[aria-label="Recorded custody comparison"]').text()).toContain('no_statement')
      expect(wrapper.text()).toContain('retained-run')
    } finally { wrapper.unmount() }
  })

  it('reads detection again on reload and preserves unknown historical fields', async () => {
    const detection = { run_id: 'recorded-run', statement_id: 'signed-statement-r1', business_date: '2020-02-04', journal_position: '9007199254740993' }
    const get = vi.spyOn(custody, 'queue').mockResolvedValueOnce({ breaks: [{ ...row, portfolio_id: 'PF1', custodian_id: 'CUST-A', detection }] }).mockResolvedValueOnce({ breaks: [row] })
    const wrapper = mount(CustodyBreaksView)
    try {
      await flushPromises()
      const source = wrapper.get('details').text()
      for (const value of ['PF1', 'CUST-A', ...Object.values(detection)]) expect(source).toContain(value)
      expect(source).toContain('Account scopeUnavailable')
      await button(wrapper, 'Reload queue').trigger('click')
      await flushPromises()
      expect(get).toHaveBeenCalledTimes(2)
      expect(wrapper.text()).not.toContain(detection.statement_id)
      expect(wrapper.text()).toContain('Detection source unavailable.')
    } finally { wrapper.unmount() }
  })

  it('identifies the selected break from the queue through confirmation and the recorded receipt', async () => {
    const other = { ...row, break_id: 'PF2|CUST|quantity|BTC/USD' }
    vi.spyOn(api, 'get').mockResolvedValue({ breaks: [row, other], count: 2, actions_enabled: true })
    const post = vi.spyOn(api, 'post').mockImplementation(async (_path, body) => ({
      ...receipt, break_id: other.break_id, request_id: (body as CustodyDecision).request_id,
    }))
    const wrapper = mount(CustodyBreaksView)
    try {
      await flushPromises()
      const rows = wrapper.findAll('tbody tr')
      expect(rows.map(entry => entry.get('th[scope="row"]').text())).toEqual([row.break_id, other.break_id])
      await rows[1]!.get('button').trigger('click')
      const review = wrapper.get('[aria-label="Custody action review"]')
      expect(review.text()).toContain(other.break_id)
      await button(wrapper, 'Review action').trigger('click')
      expect(review.text()).toContain(other.break_id)
      expect(post).not.toHaveBeenCalled()
      await button(wrapper, 'Confirm reviewed action').trigger('click')
      await flushPromises()
      expect(post).toHaveBeenCalledTimes(1)
      const [path, decision] = post.mock.calls[0]!
      expect(path).toBe('/api/v1/custody/breaks/PF2%7CCUST%7Cquantity%7CBTC%2FUSD/actions')
      const recorded = wrapper.get('[role="status"]').text()
      expect(recorded).toContain(other.break_id)
      expect(recorded).toContain((decision as CustodyDecision).request_id)
      expect(recorded).not.toContain(row.break_id)
    } finally { wrapper.unmount() }
  })

  it('requires review, preserves the retry identity, and reloads after evidence', async () => {
    const read = vi.spyOn(custody, 'queue').mockResolvedValue({ breaks: [row] })
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
    vi.spyOn(custody, 'queue').mockResolvedValue({ breaks: [row, { ...row, break_id: 'PF|CUST|quantity|ETH/USD', key: 'ETH/USD' }] })
    const act = vi.spyOn(custody, 'act')
    const wrapper = mount(CustodyBreaksView, { attachTo: document.body })
    try {
      await flushPromises()
      for (const action of ['Claim', 'Explain']) {
        const trigger = wrapper.findAll('tbody button').filter(b => b.text() === action)[1]!
        const triggerElement = trigger.element as HTMLButtonElement
        triggerElement.focus()
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
    vi.spyOn(custody, 'queue').mockResolvedValue({ breaks: [row] })
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
    vi.spyOn(custody, 'queue').mockResolvedValue({ breaks: [{ ...row, values_state: 'legacy_unverified', actions_enabled: false }] })
    const wrapper = mount(CustodyBreaksView)
    await flushPromises()
    expect(wrapper.text()).toContain('source replay and reconciliation required')
    expect(wrapper.text()).not.toContain('1.000000000001')
    expect(wrapper.findAll('button').some((b) => b.text() === 'Claim')).toBe(false)
  })
})
