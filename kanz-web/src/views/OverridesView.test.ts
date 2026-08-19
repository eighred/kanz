import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { flushPromises, mount } from '@vue/test-utils'
import OverridesView from './OverridesView.vue'
import * as overridesApi from '../api/overrides'
import type { PendingOverride } from '../api/overrides'
import { ApiError } from '../api/client'
import { useSession } from '../stores/session'

// WHAT THIS SCREEN IS FOR, AND THEREFORE WHAT IS WORTH TESTING (#371 act one).
//
// Approving an override substitutes a human's price for a mark the platform does
// not trust, into an append-only compliance record. So the properties under test
// are the ones whose failure costs something real:
//
//   1. A LAPSED PROPOSAL DOES NOT READ AS UNTOUCHED WORK, AND CANNOT BE SIGNED.
//      #563 made it visible rather than absent so a proposer could tell "it died
//      unsigned" from "somebody is still considering it". Drawn as pending, that
//      repair is undone at the last step and the exception underneath goes on
//      looking like it has a resolution coming.
//   2. THE CHOSEN PRICE SURVIVES EXACTLY. It is half the payload datamaster
//      hashes and re-checks, so a dropped price makes the confirmation meaningless
//      and a coerced one has a second person sign a value nobody read. The wire
//      form is a RATIONAL string — "261/2" for 130.5 — on which Number() is NaN,
//      and above 2^53 it silently loses a digit.
//   3. AN UNRECOGNISED STATE IS A STATED ANOMALY, NOT WORK. Including empty.
//   4. 404 AND 403 ARE DIFFERENT DEPLOYMENTS. Absent is not forbidden (#535).
//   5. AN ERROR IS NEVER AN EMPTY QUEUE, and an empty queue is an answer that
//      also says what it cannot distinguish.
//   6. A REFUSAL IS THE CONTROL FIRING, not a transport failure — this route
//      decides synchronously, so a 403/409 must not read as "try again".
//
// Every assertion below names the exact rendered text or the exact call
// arguments. Asserting that a row merely EXISTS would pass with the state cell
// blank and the price gone, which is the whole failure being guarded against.

function entry(over: Partial<PendingOverride> = {}): PendingOverride {
  return {
    proposal_id: 'prop-1',
    exception_id: 'EX1',
    act: 'PRICING_OVERRIDE',
    proposer: 'user:bob',
    reason: 'vendor confirmed after corp action',
    chosen_price: '130',
    created_at: '2026-08-19T09:00:00Z',
    expires_at: '2099-01-01T00:00:00Z',
    state: 'pending',
    ...over,
  }
}

/** signedInAs seeds the session the way the router guard would have. */
function signedInAs(subject: string) {
  const s = useSession()
  s.identity = { subject, tenant: 'acme', expires_at: '2099-01-01T00:00:00Z' }
  s.resolved = true
}

async function open(resp: PendingOverride[] | Error) {
  if (resp instanceof Error) vi.spyOn(overridesApi.overrides, 'list').mockRejectedValue(resp)
  else vi.spyOn(overridesApi.overrides, 'list').mockResolvedValue(resp)
  const wrapper = mount(OverridesView)
  await flushPromises()
  return wrapper
}

type Wrapper = Awaited<ReturnType<typeof open>>

/** cellsOf indexes the primary rows — a detail row carries no state cell. */
function cellsOf(wrapper: Wrapper, rowIndex: number) {
  return wrapper.findAll('tbody tr')[rowIndex]!.findAll('td')
}

/** stateCell is column three: the one word an approver scans for. */
function stateCell(wrapper: Wrapper, rowIndex = 0) {
  return cellsOf(wrapper, rowIndex)[2]!
}

/** priceCell is column four: the figure being signed for. */
function priceCell(wrapper: Wrapper, rowIndex = 0) {
  return cellsOf(wrapper, rowIndex)[3]!
}

/** actionCell is column seven. */
function actionCell(wrapper: Wrapper, rowIndex = 0) {
  return cellsOf(wrapper, rowIndex)[6]!
}

function button(wrapper: Wrapper, label: string) {
  return wrapper.findAll('button').find((b) => b.text() === label)
}

beforeEach(() => {
  setActivePinia(createPinia())
  vi.restoreAllMocks()
  signedInAs('user:alice')
})

describe('a lapsed proposal does not read as untouched work', () => {
  it('renders LAPSED in the state cell, never "pending"', async () => {
    const wrapper = await open([entry({ state: 'lapsed', expires_at: '2026-08-18T09:00:00Z' })])

    // THE ASSERTION THE MUTATION MUST BREAK. Not "a row rendered" — the exact
    // word in the exact cell, because a state cell reading "pending" over a
    // proposal that died unsigned is the entire defect.
    const cell = stateCell(wrapper)
    expect(cell.text()).toContain('LAPSED')
    expect(cell.text()).not.toContain('pending')
    // And it is marked as the bad state rather than merely the waiting one, so
    // it cannot be scanned past.
    expect(cell.classes()).toContain('state-bad')
  })

  it('offers no approve action on a lapsed proposal', async () => {
    const wrapper = await open([entry({ state: 'lapsed' })])

    // datamaster answers 409 on a lapsed proposal, so a button here would be an
    // offer the server has already decided against.
    expect(button(wrapper, 'Approve')).toBeUndefined()
    const action = actionCell(wrapper).find('button')
    expect(action.text()).toBe('Lapsed — cannot be signed')
    expect(action.attributes('disabled')).toBeDefined()
  })

  it('says the override did not take effect and the exception is still unresolved', async () => {
    const wrapper = await open([entry({ state: 'lapsed' })])

    const detail = wrapper.findAll('tbody tr')[1]!.find('td')
    // A FINDING, NOT A CAPTION — a screen reader must be told, not left to
    // discover it by reading the row.
    expect(detail.attributes('role')).toBe('alert')
    expect(detail.text()).toContain('lapsed unsigned')
    expect(detail.text()).toContain('did NOT take effect')
    expect(detail.text()).toContain('proposed again')
  })

  it('counts it in the banner above the table rather than only in its row', async () => {
    const wrapper = await open([entry({ state: 'lapsed' }), entry({ proposal_id: 'prop-2' })])

    const banner = wrapper.findAll('[role="alert"]')[0]!
    expect(banner.text()).toContain('1 of 2 entries')
    expect(banner.text()).toContain('can no longer be signed')
  })
})

describe('the chosen price survives exactly', () => {
  // THE COERCION MUTATION'S TARGET. Number('261/2') is NaN and parseFloat is
  // 261 — the price a second person signs for would read as a different number
  // entirely, or as nothing.
  it('renders a rational price as its exact decimal', async () => {
    const wrapper = await open([entry({ chosen_price: '261/2' })])

    expect(priceCell(wrapper).text()).toContain('130.5')
    // And the wire form is kept beside it, because that rational is what the
    // audit trail and datamaster's digest are computed over.
    expect(priceCell(wrapper).text()).toContain('sent as 261/2')
  })

  // THE OTHER COERCION. 2^53 + 1 is the first integer a double cannot hold, so
  // anything routed through Number() renders ...992 for ...993 — silently, and
  // only for large values.
  it('renders a price beyond 2^53 digit for digit', async () => {
    const exact = '9007199254740993'
    const wrapper = await open([entry({ chosen_price: exact })])

    expect(priceCell(wrapper).text()).toContain(exact)
    expect(priceCell(wrapper).text()).not.toContain('9007199254740992')
    expect(Number(exact).toString()).not.toBe(exact) // the failure this avoids, demonstrated
  })

  // THE DROP MUTATION'S TARGET. A blank price cell is not a neutral omission:
  // the price IS the payload, and a confirmation with no number in it invites a
  // signature over nothing.
  it('shows the price on the row and in the confirmation before it is signed', async () => {
    const wrapper = await open([entry({ chosen_price: '261/2' })])
    expect(priceCell(wrapper).text()).toContain('130.5')

    await button(wrapper, 'Approve')!.trigger('click')
    const dialog = wrapper.find('[role="dialog"]')
    expect(dialog.exists()).toBe(true)
    expect(dialog.text()).toContain('130.5')
    expect(dialog.text()).toContain('vendor confirmed after corp action')
  })

  it('states a price it cannot render exactly rather than rounding it', async () => {
    // A denominator with a factor other than 2 or 5 has no exact decimal. The
    // server's fixed-scale assertion should stop this reaching a client at all,
    // so seeing it means something answered wrong — and 0.333… would be a lie.
    const wrapper = await open([entry({ chosen_price: '1/3' })])

    const cell = priceCell(wrapper)
    expect(cell.text()).toContain('PRICE NOT RENDERABLE')
    expect(cell.text()).toContain('1/3')
    expect(cell.text()).not.toContain('0.33')
    expect(cell.classes()).toContain('state-bad')
  })

  it('says so loudly when no price was sent at all, and never shows a zero', async () => {
    const wrapper = await open([entry({ chosen_price: '' })])

    const cell = priceCell(wrapper)
    expect(cell.text()).toContain('NO PRICE STATED')
    expect(cell.text()).not.toContain('0')
  })

  it('refuses to let the confirmation stand behind an unrenderable price', async () => {
    const wrapper = await open([entry({ chosen_price: '1/3' })])
    await button(wrapper, 'Approve')!.trigger('click')

    const dialog = wrapper.find('[role="dialog"]')
    expect(dialog.text()).toContain('Do not sign this')
    expect(dialog.text()).toContain('nobody has read')
  })
})

describe('an unrecognised state is an anomaly, not work', () => {
  for (const [name, state] of [
    ['an empty state', ''],
    ['an absent state', undefined],
    ['a state from a newer server', 'withdrawn'],
    // The OMS queue's own value. It cannot occur here — an override refusal
    // comes back on the same HTTP request — so reading it as anything but
    // unknown would be this client inventing a state for the wrong act.
    ['the other act’s state', 'refused'],
  ] as const) {
    it(`renders ${name} as STATE NOT STATED and withholds the action`, async () => {
      const wrapper = await open([entry({ state })])

      expect(stateCell(wrapper).text()).toContain('STATE NOT STATED')
      expect(stateCell(wrapper).text()).not.toContain('pending')
      expect(button(wrapper, 'Approve')).toBeUndefined()
      expect(actionCell(wrapper).find('button').text()).toBe('Not signable')
    })
  }

  it('names what the server actually said so it can be reported', async () => {
    const wrapper = await open([entry({ state: 'withdrawn' })])
    expect(wrapper.findAll('tbody tr')[1]!.text()).toContain('it said "withdrawn"')
  })
})

describe('the approve call names both ids and asserts nothing about the approver', () => {
  it('sends the exception id in the path and the proposal id in the body', async () => {
    const approve = vi.spyOn(overridesApi.overrides, 'approve').mockResolvedValue({})
    const wrapper = await open([entry({ exception_id: 'EX7', proposal_id: 'prop-9' })])

    await button(wrapper, 'Approve')!.trigger('click')
    await button(wrapper, 'Approve the override')!.trigger('click')
    await flushPromises()

    // The exception id is checked upstream against the proposal's own subject.
    // Dropping it would make the exception on screen decorative — an approver
    // could be shown one and sign for another.
    expect(approve).toHaveBeenCalledWith('EX7', 'prop-9')
  })

  it('reports the applied override as APPLIED, because this route decides now', async () => {
    vi.spyOn(overridesApi.overrides, 'approve').mockResolvedValue({})
    const wrapper = await open([entry()])

    await button(wrapper, 'Approve')!.trigger('click')
    await button(wrapper, 'Approve the override')!.trigger('click')
    await flushPromises()

    // NOT the order screen's "submitted": datamaster applies the override inside
    // the request. Understating it leaves an approver expecting a confirmation
    // that never comes, and the predictable repair for that is signing it twice.
    const notice = wrapper.find('[role="status"]')
    expect(notice.text()).toContain('APPLIED')
    expect(notice.text()).toContain('EX1')
    expect(notice.text()).toContain('user:bob')
  })

  it('withholds the button from the proposer, by the server’s own comparison', async () => {
    // Case and surrounding space are normalised upstream, so "User:Alice" and
    // "user:alice" are one person and a case-sensitive client would offer a
    // button that always 403s.
    const wrapper = await open([entry({ proposer: ' User:Alice ' })])

    expect(button(wrapper, 'Approve')).toBeUndefined()
    expect(actionCell(wrapper).find('button').text()).toBe('You proposed this')
  })
})

describe('a refusal is the control firing, not a failure to reach it', () => {
  it('reports a 403 as refused and names both possible rules', async () => {
    vi.spyOn(overridesApi.overrides, 'approve').mockRejectedValue(
      new ApiError(403, 'the approver must be a different person from the proposer'),
    )
    const wrapper = await open([entry()])

    await button(wrapper, 'Approve')!.trigger('click')
    await button(wrapper, 'Approve the override')!.trigger('click')
    await flushPromises()

    const dialog = wrapper.find('[role="dialog"]')
    expect(dialog.text()).toContain('REFUSED')
    expect(dialog.text()).toContain('the approver must be a different person from the proposer')
    // The panel stays open: the reason belongs beside what was attempted.
    expect(dialog.exists()).toBe(true)
  })

  it('reports a 409 as refused and NOT applied, rather than as a retryable error', async () => {
    vi.spyOn(overridesApi.overrides, 'approve').mockRejectedValue(
      new ApiError(409, 'this proposal expired before it was approved and must be proposed again'),
    )
    const wrapper = await open([entry()])

    await button(wrapper, 'Approve')!.trigger('click')
    await button(wrapper, 'Approve the override')!.trigger('click')
    await flushPromises()

    const text = wrapper.find('[role="dialog"]').text()
    expect(text).toContain('was NOT applied')
    expect(text).toContain('must be proposed again')
  })
})

describe('the queue read reports absent and forbidden as different deployments', () => {
  it('says 404 is ABSENT — no approver role, or another tenant', async () => {
    const wrapper = await open(new ApiError(404, 'not found'))

    const alert = wrapper.find('[role="alert"]')
    expect(alert.text()).toContain('ABSENT, not forbidden')
    expect(alert.text()).toContain('no approver role is configured')
    // And it must not claim the queue is empty.
    expect(alert.text()).toContain('does not mean no override is waiting')
  })

  it('says 403 is FORBIDDEN — the control runs and you are not a signatory', async () => {
    const wrapper = await open(new ApiError(403, 'forbidden'))

    const alert = wrapper.find('[role="alert"]')
    expect(alert.text()).toContain('does not carry approval authority')
    expect(alert.text()).toContain('DOES route the override queue')
  })

  it('never draws a failure as an empty queue', async () => {
    const wrapper = await open(new ApiError(503, 'upstream service unavailable'))

    expect(wrapper.find('[role="alert"]').text()).toContain('NOT an empty queue')
    expect(wrapper.text()).not.toContain('No override is awaiting a second signature')
  })

  it('treats a body that is not a list as unread, not as nothing held', async () => {
    // The module refuses to coerce it; the view must not undo that by rendering
    // the failure as an empty table.
    vi.spyOn(overridesApi.overrides, 'list').mockRejectedValue(
      new Error('the pending-override queue answered with something that is not a list'),
    )
    const wrapper = mount(OverridesView)
    await flushPromises()

    expect(wrapper.find('[role="alert"]').text()).toContain('is not a list')
  })
})

describe('an empty queue is an answer, and says what it cannot distinguish', () => {
  it('states that nothing is held AND that this is also an unarmed deployment', async () => {
    const wrapper = await open([])

    expect(wrapper.text()).toContain('No override is awaiting a second signature')
    // "Nothing configured" and "checked, and fine" must not look the same, and on
    // this surface the server cannot tell them apart — so the screen says so
    // rather than implying the control is running.
    expect(wrapper.text()).toContain('DATAMASTER_REQUIRE_DUAL_CONTROL')
    expect(wrapper.text()).toContain('not on its own evidence that the control')
  })
})

describe('a window that closes while the page is open is stated before the click', () => {
  it('marks a still-pending proposal whose expiry has passed', async () => {
    const wrapper = await open([entry({ expires_at: '2020-01-01T00:00:00Z' })])

    // datamaster decides lapsed at READ time; the list is a snapshot and the
    // deadline keeps moving. Saying so beats a 409 after the signature.
    expect(cellsOf(wrapper, 0)[5]!.text()).toContain('window closed')
  })
})
