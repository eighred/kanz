import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { flushPromises, mount } from '@vue/test-utils'
import ApprovalsView from './ApprovalsView.vue'
import * as approvalsApi from '../api/approvals'
import type { PendingApproval, PendingApprovalsResponse } from '../api/approvals'
import { ApiError } from '../api/client'
import { useSession } from '../stores/session'

// WHAT THIS SCREEN IS FOR, AND THEREFORE WHAT IS WORTH TESTING (#371 slice).
//
// The queue is the ONLY place a held order appears — it is deliberately absent
// from the orders table — and the only place the digest an approval must carry
// exists for a human. So the properties under test are the ones whose failure
// costs something:
//
//   1. A REFUSED ENTRY DOES NOT READ AS UNTOUCHED WORK. "refused" still means
//      awaiting a signature, so it sits in the same list; drawn as "pending" it
//      hides that a rule already fired on this order.
//   2. THE DIGEST SURVIVES, whole, both on screen and into the approve call.
//      Dropped, approving is impossible (the route answers 400); truncated, an
//      approver cannot tell two proposals apart.
//   3. THE 202 IS NOT AN APPROVAL. The gateway publishes and the OMS decides
//      afterwards, so reporting release here would claim the one fact dual
//      control exists to establish.
//   4. SELF-APPROVAL IS SHOWN, NOT SWALLOWED — before the click (the proposer
//      gets no button) and after it (a refusal recorded on the queue is read
//      back and named).
//   5. 404 AND 403 ARE DIFFERENT DEPLOYMENTS. Absent is not forbidden (#535).
//
// Every assertion below names the exact rendered text or the exact call
// arguments. Asserting that a row merely EXISTS would pass with the state cell
// blank and the digest gone, which is the whole failure being guarded against.

const DIGEST = 'b1946ac92492d2347c6235b4d2611184b1946ac92492d2347c6235b4d2611184'
const OTHER_DIGEST = '9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08'

function entry(over: Partial<PendingApproval> = {}): PendingApproval {
  return {
    order_id: 'ord-1',
    proposer: 'user:bob',
    act: 'ORDER_SUBMISSION',
    digest: DIGEST,
    proposed_at: '2026-08-19T09:00:00Z',
    expires_at: '2099-01-01T00:00:00Z',
    state: 'pending',
    last_refusal_reason: '',
    last_refused_by: '',
    command: {
      order_id: 'ord-1',
      portfolio_id: 'pf-1',
      instrument_id: 'BTC-USD',
      side: 'SIDE_BUY',
      order_type: 'ORDER_TYPE_LIMIT',
      quantity: { coefficient: '250', exponent: -1 },
      limit_price: { coefficient: '6400000', exponent: -2 },
    },
    ...over,
  }
}

/** signedInAs seeds the session the way the router guard would have. */
function signedInAs(subject: string) {
  const s = useSession()
  s.identity = { subject, tenant: 'acme', expires_at: '2099-01-01T00:00:00Z' }
  s.resolved = true
}

async function open(resp: PendingApprovalsResponse | Error) {
  if (resp instanceof Error) vi.spyOn(approvalsApi.approvals, 'list').mockRejectedValue(resp)
  else vi.spyOn(approvalsApi.approvals, 'list').mockResolvedValue(resp)
  const wrapper = mount(ApprovalsView)
  await flushPromises()
  return wrapper
}

type Wrapper = Awaited<ReturnType<typeof open>>

/** dataRows are the primary rows — the detail rows carry no state cell. */
function cellsOf(wrapper: Wrapper, rowIndex: number) {
  return wrapper.findAll('tbody tr')[rowIndex]!.findAll('td')
}

/** stateCell is column three: the one word an approver scans for. */
function stateCell(wrapper: Wrapper, rowIndex = 0) {
  return cellsOf(wrapper, rowIndex)[2]!
}

/** digestCell is column five. */
function digestCell(wrapper: Wrapper, rowIndex = 0) {
  return cellsOf(wrapper, rowIndex)[4]!
}

function button(wrapper: Wrapper, label: string) {
  return wrapper.findAll('button').find((b) => b.text() === label)
}

beforeEach(() => {
  setActivePinia(createPinia())
  vi.restoreAllMocks()
  signedInAs('user:alice')
})

describe('a refused entry does not read as untouched work', () => {
  it('renders REFUSED in the state cell, never "pending"', async () => {
    const wrapper = await open({
      pending: [
        entry({
          state: 'refused',
          last_refusal_reason: 'self_approval',
          last_refused_by: 'user:bob',
          last_refused_at: '2026-08-19T10:30:00Z',
        }),
      ],
      owner_tenant: 'acme',
    })

    // THE ASSERTION THE MUTATION MUST BREAK. Not "a row rendered" — the exact
    // word in the exact cell, because a state cell that says "pending" over a
    // refused proposal is the entire defect.
    expect(stateCell(wrapper).text()).toBe('REFUSED')
    expect(stateCell(wrapper).text()).not.toContain('pending')
    // Colour is reserved for meaning on this surface; a refusal is not neutral.
    expect(stateCell(wrapper).classes()).toContain('state-bad')
  })

  it('names who was refused, why, and when — not merely that something happened', async () => {
    const wrapper = await open({
      pending: [
        entry({
          state: 'refused',
          last_refusal_reason: 'payload_changed',
          last_refused_by: 'user:carol',
          last_refused_at: '2026-08-19T10:30:00Z',
        }),
      ],
    })

    const alerts = wrapper.findAll('[role="alert"]').map((a) => a.text()).join(' ')
    expect(alerts).toContain('user:carol')
    expect(alerts).toContain('2026-08-19 10:30:00Z')
    expect(alerts).toMatch(/changed after it was proposed/i)
    // "refused" NEVER means finished — a decided proposal is off the queue.
    expect(alerts).toMatch(/still held and still needs a second signature/i)
  })

  it('says my signature was refused when it was mine, and not when it was not', async () => {
    const mine = await open({
      pending: [
        entry({ state: 'refused', last_refusal_reason: 'self_approval', last_refused_by: 'User:Alice ' }),
      ],
    })
    // Case- and space-insensitive, matching the comparison the server makes —
    // otherwise "User:Alice " and "user:alice" read as two people here and one
    // person there.
    expect(mine.text()).toContain('Your signature was refused')

    const theirs = await open({
      pending: [entry({ state: 'refused', last_refusal_reason: 'self_approval', last_refused_by: 'user:dave' })],
    })
    expect(theirs.text()).toContain('A signature was refused')
    expect(theirs.text()).not.toContain('Your signature was refused')
  })

  it('counts refused entries in a banner above the table', async () => {
    const wrapper = await open({
      pending: [
        entry({ order_id: 'ord-1' }),
        entry({ order_id: 'ord-2', state: 'refused', last_refusal_reason: 'expired', last_refused_by: 'user:bob' }),
      ],
    })
    expect(wrapper.text()).toContain('1 of 2 entries')
    expect(wrapper.text()).toMatch(/do not read them as untouched work/i)
  })

  it('treats a state it cannot read as unknown rather than as clean', async () => {
    // The OMS always sets `state`. An empty or unrecognised one therefore means
    // something is wrong with what answered — never that the entry is fine.
    const wrapper = await open({ pending: [entry({ state: '' })] })
    expect(stateCell(wrapper).text()).toBe('STATE NOT STATED')
    expect(stateCell(wrapper).classes()).toContain('state-bad')
    expect(wrapper.text()).toMatch(/Treat it as unknown rather than as untouched work/i)
  })

  it('renders a genuinely pending entry as pending', async () => {
    // The counterweight: if everything read as refused the guard above would
    // pass while the screen was useless.
    const wrapper = await open({ pending: [entry()] })
    expect(stateCell(wrapper).text()).toBe('pending')
    expect(wrapper.findAll('[role="alert"]')).toHaveLength(0)
  })
})

describe('the digest survives, whole', () => {
  it('shows the digest in full, because the approve call is impossible without it', async () => {
    const wrapper = await open({ pending: [entry()] })
    // The FULL 64 characters in the digest cell. Truncating it is how an
    // approver signs the proposal they were not reading about.
    expect(digestCell(wrapper).text()).toBe(DIGEST)
  })

  it('passes the entry’s own digest to the approve route, not another entry’s', async () => {
    const approve = vi
      .spyOn(approvalsApi.approvals, 'approve')
      .mockResolvedValue({ order_id: 'ord-2', status: 'approval_submitted' })
    const wrapper = await open({
      pending: [entry({ order_id: 'ord-1' }), entry({ order_id: 'ord-2', digest: OTHER_DIGEST })],
    })

    // Second row's Approve button — rows 0 and 1 are both primary rows here
    // because neither entry is refused.
    await cellsOf(wrapper, 1)[5]!.find('button').trigger('click')
    await button(wrapper, 'Submit approval')!.trigger('click')
    await flushPromises()

    expect(approve).toHaveBeenCalledExactlyOnceWith('ord-2', OTHER_DIGEST)
  })

  it('offers no approve affordance for an entry carrying no digest', async () => {
    const approve = vi.spyOn(approvalsApi.approvals, 'approve').mockResolvedValue({})
    const wrapper = await open({ pending: [entry({ digest: '' })] })

    expect(button(wrapper, 'Approve')).toBeUndefined()
    expect(button(wrapper, 'No digest')!.attributes('disabled')).toBeDefined()
    expect(digestCell(wrapper).text()).toContain('cannot be approved')
    expect(approve).not.toHaveBeenCalled()
  })

  it('shows the digest again in the confirmation, beside the terms it covers', async () => {
    const wrapper = await open({ pending: [entry()] })
    await button(wrapper, 'Approve')!.trigger('click')

    const dialog = wrapper.find('[role="dialog"]')
    expect(dialog.text()).toContain(DIGEST)
    expect(dialog.text()).toContain('BTC-USD')
    expect(dialog.text()).toContain('25') // quantity 250e-1
    expect(dialog.text()).toContain('64000.00') // limit 6400000e-2, trailing zeros kept
    expect(dialog.text()).toContain('user:bob')
  })
})

describe('the 202 is not an approval', () => {
  it('reports the approval as submitted and says it is not yet approved', async () => {
    vi.spyOn(approvalsApi.approvals, 'approve').mockResolvedValue({
      order_id: 'ord-1',
      status: 'approval_submitted',
    })
    const wrapper = await open({ pending: [entry()] })

    await button(wrapper, 'Approve')!.trigger('click')
    await button(wrapper, 'Submit approval')!.trigger('click')
    await flushPromises()

    const status = wrapper.find('[role="status"]').text()
    // BOTH halves. A mutation to "Approved." fails the first; one that dropped
    // the caveat and kept the verb fails the second.
    expect(status).toMatch(/SUBMITTED/)
    expect(status).toMatch(/not approved yet/i)
    expect(status).toContain('ord-1')
  })

  it('re-reads the queue afterwards, because that is where the answer appears', async () => {
    const list = vi.spyOn(approvalsApi.approvals, 'list').mockResolvedValue({ pending: [entry()] })
    vi.spyOn(approvalsApi.approvals, 'approve').mockResolvedValue({})
    const wrapper = mount(ApprovalsView)
    await flushPromises()

    await button(wrapper, 'Approve')!.trigger('click')
    await button(wrapper, 'Submit approval')!.trigger('click')
    await flushPromises()

    expect(list).toHaveBeenCalledTimes(2)
  })

  it('surfaces a refusal that arrives on the re-read rather than leaving "submitted" standing alone', async () => {
    // THE END-TO-END SHAPE OF AN ASYNCHRONOUS REFUSAL. The approve call
    // succeeded — it published — and the OMS refused afterwards. The screen must
    // show the refusal, not only the submission.
    const list = vi.spyOn(approvalsApi.approvals, 'list')
    list.mockResolvedValueOnce({ pending: [entry()] })
    list.mockResolvedValueOnce({
      pending: [
        entry({
          state: 'refused',
          last_refusal_reason: 'self_approval',
          last_refused_by: 'user:alice',
          last_refused_at: '2026-08-19T11:00:00Z',
        }),
      ],
    })
    vi.spyOn(approvalsApi.approvals, 'approve').mockResolvedValue({})
    const wrapper = mount(ApprovalsView)
    await flushPromises()

    await button(wrapper, 'Approve')!.trigger('click')
    await button(wrapper, 'Submit approval')!.trigger('click')
    await flushPromises()

    expect(stateCell(wrapper).text()).toBe('REFUSED')
    expect(wrapper.text()).toContain('Your signature was refused')
    expect(wrapper.text()).toMatch(/find another approver/i)
  })

  it('reports a failed publish as nothing having been signed, and keeps the panel open', async () => {
    vi.spyOn(approvalsApi.approvals, 'approve').mockRejectedValue(new ApiError(503, 'order writes are disabled'))
    const wrapper = await open({ pending: [entry()] })

    await button(wrapper, 'Approve')!.trigger('click')
    await button(wrapper, 'Submit approval')!.trigger('click')
    await flushPromises()

    const dialog = wrapper.find('[role="dialog"]')
    expect(dialog.exists()).toBe(true)
    expect(dialog.text()).toMatch(/writes are disabled/i)
    expect(dialog.text()).toMatch(/still held/i)
    expect(wrapper.find('[role="status"]').exists()).toBe(false)
  })
})

describe('self-approval is shown, not swallowed', () => {
  it('withholds the approve affordance from the proposer and says why', async () => {
    const approve = vi.spyOn(approvalsApi.approvals, 'approve').mockResolvedValue({})
    const wrapper = await open({ pending: [entry({ proposer: 'user:alice' })] })

    expect(button(wrapper, 'Approve')).toBeUndefined()
    expect(button(wrapper, 'You proposed this')!.attributes('disabled')).toBeDefined()
    expect(wrapper.text()).toContain('that is you')
    expect(approve).not.toHaveBeenCalled()
  })

  it('matches the proposer the way the server does — case and space insensitive', async () => {
    // "Alice@kanz" approving "alice@kanz" is ONE person to internal/dualcontrol.
    // A stricter comparison here would offer a button that always fails.
    const wrapper = await open({ pending: [entry({ proposer: '  User:Alice ' })] })
    expect(button(wrapper, 'Approve')).toBeUndefined()
    expect(button(wrapper, 'You proposed this')).toBeDefined()
  })

  it('still offers the button for somebody else’s proposal', async () => {
    const wrapper = await open({ pending: [entry({ proposer: 'user:bob' })] })
    expect(button(wrapper, 'Approve')).toBeDefined()
    expect(wrapper.text()).not.toContain('that is you')
  })

  it('says so when the browser has no subject to compare against', async () => {
    const s = useSession()
    s.identity = null
    const wrapper = await open({ pending: [entry()] })
    expect(wrapper.text()).toMatch(/cannot tell which of these you proposed/i)
  })
})

describe('the deployment postures are different sentences', () => {
  it('reads 404 as ABSENT — no approver configured — never as forbidden', async () => {
    const wrapper = await open(new ApiError(404, 'not found'))
    const alert = wrapper.find('[role="alert"]').text()
    expect(alert).toMatch(/no approvals queue on this gateway/i)
    expect(alert).toMatch(/ABSENT, not forbidden/)
    // It must not claim nothing is held anywhere — the route simply is not here.
    expect(alert).toMatch(/does not mean no order is being held/i)
    expect(alert).not.toMatch(/does not carry approval authority/i)
  })

  it('reads 403 as FORBIDDEN — the control exists and you are not a signatory', async () => {
    const wrapper = await open(new ApiError(403, 'forbidden'))
    const alert = wrapper.find('[role="alert"]').text()
    expect(alert).toMatch(/does not carry approval authority/i)
    expect(alert).toMatch(/DOES hold orders for a second signature/)
    expect(alert).not.toMatch(/not switched on here/i)
  })

  it('never renders a failure as an empty queue', async () => {
    const wrapper = await open(new ApiError(503, 'unavailable'))
    expect(wrapper.find('table').exists()).toBe(false)
    expect(wrapper.text()).not.toMatch(/Nothing is awaiting a second signature/i)
    expect(wrapper.text()).toMatch(/NOT an empty queue/)
  })

  it('renders a successful empty read as an answer, not as a spinner', async () => {
    const wrapper = await open({ pending: [], owner_tenant: 'acme' })
    expect(wrapper.text()).toMatch(/Nothing is awaiting a second signature/i)
    expect(wrapper.text()).toMatch(/not "nothing could be read"/i)
    expect(wrapper.text()).not.toContain('Loading')
  })
})

describe('the expiry window is part of the answer', () => {
  it('marks a proposal whose window has closed, and warns before the signature', async () => {
    const wrapper = await open({ pending: [entry({ expires_at: '2020-01-01T00:00:00Z' })] })
    expect(cellsOf(wrapper, 0)[3]!.text()).toContain('window closed')
    expect(cellsOf(wrapper, 0)[3]!.classes()).toContain('state-bad')

    await button(wrapper, 'Approve')!.trigger('click')
    expect(wrapper.find('[role="dialog"]').text()).toMatch(/refuse the signature as expired/i)
  })

  it('does not mark a live proposal as closed', async () => {
    const wrapper = await open({ pending: [entry()] })
    expect(cellsOf(wrapper, 0)[3]!.text()).not.toContain('window closed')
  })
})
