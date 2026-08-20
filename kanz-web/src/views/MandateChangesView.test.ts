import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { flushPromises, mount } from '@vue/test-utils'
import MandateChangesView from './MandateChangesView.vue'
import * as mandatesApi from '../api/mandates'
import type { PendingMandateChange } from '../api/mandates'
import { ApiError } from '../api/client'
import { useSession } from '../stores/session'

// WHAT THIS SCREEN IS FOR, AND THEREFORE WHAT IS WORTH TESTING (#371 act two).
//
// Approving a mandate change publishes the constraint set every order in a
// portfolio is checked against. So the properties under test are the ones whose
// failure costs something real:
//
//   1. A LAPSED PROPOSAL DOES NOT READ AS UNTOUCHED WORK, AND CANNOT BE SIGNED.
//      #563 made it visible rather than absent. Drawn as pending, that repair is
//      undone at the last step: compliance answers 409 while the portfolio goes on
//      looking like its constraint is about to change when it is still the old one.
//   2. AN UNRECOGNISED STATE IS A STATED ANOMALY, NOT WORK. Including empty.
//   3. THE NUMBERS ARE NOT COERCED, IN EITHER DIRECTION. `rule_count` and
//      `version` ride the wire as JSON numbers and both have a falsy/lossy trap:
//      a legal 0 rule count means "constrains nothing" and disappears behind
//      `v-if`, and a uint64 version above 2^53 is already rounded by JSON.parse.
//      Neither may be printed as fact and neither may be Number()'d into one.
//   4. WHAT CANNOT BE SEEN IS SAID. This queue serves a digest, not a mandate,
//      and no route expands one — so a signatory must be told before they sign,
//      or the two names on the record belong to two people who never read the rule.
//   5. 404, 403 AND 401 ARE DIFFERENT DEPLOYMENTS. Absent is not forbidden (#535),
//      and authz.Mandate is not authz.Approve.
//   6. AN ERROR IS NEVER AN EMPTY QUEUE.
//   7. A REFUSAL IS THE CONTROL FIRING — this route decides synchronously, so a
//      403/409 must not read as "try again" — and a 502 is neither: the approval
//      was valid and the mandate is NOT in force.
//
// Every assertion below names the exact rendered text or the exact call
// arguments. Asserting that a row merely EXISTS would pass with the state cell
// blank and the rule count gone, which is the whole failure being guarded against.

function entry(over: Partial<PendingMandateChange> = {}): PendingMandateChange {
  return {
    proposal_id: 'prop-1',
    act: 'MANDATE_CHANGE',
    portfolio_id: 'PF1',
    mandate_id: 'MD1',
    version: 7,
    rule_count: 3,
    proposer: 'user:bob',
    reason: 'raise the tech concentration cap after the mandate committee ruling',
    digest: 'sha256:abcdef',
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

async function open(resp: PendingMandateChange[] | Error) {
  if (resp instanceof Error) vi.spyOn(mandatesApi.mandates, 'list').mockRejectedValue(resp)
  else vi.spyOn(mandatesApi.mandates, 'list').mockResolvedValue(resp)
  const wrapper = mount(MandateChangesView)
  await flushPromises()
  return wrapper
}

type Wrapper = Awaited<ReturnType<typeof open>>

/** cellsOf indexes the primary rows — a detail row carries no state cell. */
function cellsOf(wrapper: Wrapper, rowIndex: number) {
  return wrapper.findAll('tbody tr')[rowIndex]!.findAll('td')
}

/** stateCell is column three: the one word a signatory scans for. */
function stateCell(wrapper: Wrapper, rowIndex = 0) {
  return cellsOf(wrapper, rowIndex)[2]!
}

/** becomesCell is column four: the version and the rule count. */
function becomesCell(wrapper: Wrapper, rowIndex = 0) {
  return cellsOf(wrapper, rowIndex)[3]!
}

/** actionCell is column seven. */
function actionCell(wrapper: Wrapper, rowIndex = 0) {
  return cellsOf(wrapper, rowIndex)[6]!
}

function button(wrapper: Wrapper, label: string) {
  return wrapper.findAll('button').find((b) => b.text() === label)
}

/** confirm opens the signing panel on the first row and returns it. */
async function confirm(wrapper: Wrapper) {
  await button(wrapper, 'Approve')!.trigger('click')
  await flushPromises()
  return wrapper.find('[role="dialog"]')
}

beforeEach(() => {
  setActivePinia(createPinia())
  vi.restoreAllMocks()
  signedInAs('user:alice')
})

describe('a lapsed mandate change does not read as untouched work', () => {
  it('renders LAPSED in the state cell, never "pending"', async () => {
    const wrapper = await open([entry({ state: 'lapsed', expires_at: '2026-08-18T09:00:00Z' })])

    // THE ASSERTION THE MUTATION MUST BREAK. Not "a row rendered" — the exact
    // word in the exact cell, because a state cell reading "pending" over a
    // change that died unsigned is the entire defect.
    const cell = stateCell(wrapper)
    expect(cell.text()).toContain('LAPSED')
    expect(cell.text()).not.toContain('pending')
    expect(cell.classes()).toContain('state-bad')
  })

  it('offers no approve action on a lapsed proposal', async () => {
    const wrapper = await open([entry({ state: 'lapsed' })])

    // compliance answers 409 on a lapsed proposal, so a button here would be an
    // offer the server has already decided against.
    expect(button(wrapper, 'Approve')).toBeUndefined()
    const action = actionCell(wrapper).find('button')
    expect(action.attributes('disabled')).toBeDefined()
    expect(action.text()).toContain('Lapsed')
  })

  it('says the portfolio is still governed by its PREVIOUS mandate', async () => {
    const wrapper = await open([entry({ state: 'lapsed' })])

    // The consequence, not the status. A signatory who reads "lapsed" and stops
    // there does not learn that the constraint they thought had changed did not.
    const detail = wrapper.findAll('tbody tr')[1]!
    expect(detail.text()).toContain('lapsed unsigned')
    expect(detail.text()).toContain('still governed by its previous constraint set')
    expect(detail.text()).toContain('PF1')
  })

  it('counts it in the banner above the table rather than only in its row', async () => {
    const wrapper = await open([entry({ state: 'lapsed' }), entry({ proposal_id: 'p2' })])

    const banner = wrapper.findAll('[role="alert"]')[0]!
    expect(banner.text()).toContain('1 of 2 entries')
    expect(banner.text()).toContain('still governed by its PREVIOUS mandate')
  })
})

describe('an unrecognised state is an anomaly, not work', () => {
  // DENY BY DEFAULT. The server always sets `state`, so absence means something
  // other than compliance answered — and it must never read as clean work.
  for (const [label, state] of [
    ['empty', ''],
    ['absent', undefined],
    ['a value this client has not learned', 'withdrawn'],
  ] as const) {
    it(`renders ${label} as STATE NOT STATED and withholds the button`, async () => {
      const wrapper = await open([entry({ state })])

      const cell = stateCell(wrapper)
      expect(cell.text()).toContain('STATE NOT STATED')
      expect(cell.text()).not.toContain('pending')
      expect(cell.classes()).toContain('state-bad')
      expect(button(wrapper, 'Approve')).toBeUndefined()
    })
  }

  it('names what the server actually said so it can be reported', async () => {
    const wrapper = await open([entry({ state: 'withdrawn' })])
    expect(wrapper.findAll('tbody tr')[1]!.text()).toContain('it said "withdrawn"')
  })
})

describe('the numbers are not coerced, in either direction', () => {
  // THIS SURFACE CARRIES NO DECIMAL AT ALL — checked, not assumed. compliance's
  // proposalJSON is hand-built encoding/json and emits strings everywhere except
  // `version` (uint64) and `rule_count` (int), which are JSON NUMBERS. So the
  // coercion hazard #590 found in a rational price arrives here in the two
  // integers instead, and it is the same class of defect: a figure a second
  // person signs for that was quietly turned into a different figure.

  it('renders a rule count of ZERO as NO RULES, never as absent', async () => {
    const wrapper = await open([entry({ rule_count: 0 })])

    // THE FALSY-CHECK MUTATION MUST BREAK HERE. `v-if="p.rule_count"` and
    // `p.rule_count || 'unknown'` both read a legal, meaningful 0 as
    // nothing-stated — and 0 rules is a mandate that constrains NOTHING, which
    // is the most consequential thing this queue can be asked to approve.
    const cell = becomesCell(wrapper)
    expect(cell.text()).toContain('NO RULES')
    expect(cell.text()).toContain('constrains nothing')
    expect(cell.text()).not.toContain('RULE COUNT NOT STATED')
  })

  it('spells out what approving a no-rule mandate does, in the confirmation', async () => {
    const wrapper = await open([entry({ rule_count: 0 })])
    const dialog = await confirm(wrapper)

    expect(dialog.text()).toContain('NO RULES AT ALL')
    expect(dialog.text()).toContain('passes every mandate check')
  })

  it('refuses a non-numeric rule count rather than Number()-ing it', async () => {
    // An older or proxied server sending "3" — or "" — as a string. Number("")
    // is 0, which would print "constrains nothing" over a row whose count
    // nobody sent: absent turned into the most consequential legal value.
    const wrapper = await open([entry({ rule_count: '' as unknown as number })])

    const cell = becomesCell(wrapper)
    expect(cell.text()).toContain('RULE COUNT NOT STATED')
    expect(cell.text()).not.toContain('NO RULES')
    expect(cell.text()).not.toContain('constrains nothing')
    expect(cell.classes()).toContain('state-bad')
  })

  it('refuses a stringly-typed rule count even when it looks like a number', async () => {
    const wrapper = await open([entry({ rule_count: '3' as unknown as number })])

    const cell = becomesCell(wrapper)
    expect(cell.text()).toContain('RULE COUNT NOT STATED')
    // And it shows what was actually sent, so it can be reported rather than
    // guessed at.
    expect(cell.text()).toContain('3')
    expect(cell.text()).not.toContain('3 rules')
  })

  it('renders an ordinary rule count and version exactly', async () => {
    const wrapper = await open([entry({ version: 7, rule_count: 3 })])

    const cell = becomesCell(wrapper)
    expect(cell.text()).toContain('version 7')
    expect(cell.text()).toContain('3 rules')
    expect(cell.classes()).not.toContain('state-bad')
  })

  it('states a version that did not survive the wire rather than printing it', async () => {
    // uint64 max. JSON.parse has ALREADY rounded this to 18446744073709552000
    // before any of this code runs, and no client can recover the true value —
    // so the only honest options are to print the damaged one as fact or to say
    // it did not survive. On the field that pins which constraint an order is
    // audited against, printing it is the defect.
    const wrapper = await open([entry({ version: 18446744073709551615 })])

    const cell = becomesCell(wrapper)
    expect(cell.text()).toContain('VERSION NOT RENDERABLE')
    expect(cell.text()).not.toContain('version 18446744073709552000')
    expect(cell.classes()).toContain('state-bad')
  })

  it('says so when no version was stated at all, and never shows a zero', async () => {
    const wrapper = await open([entry({ version: undefined })])

    const cell = becomesCell(wrapper)
    expect(cell.text()).toContain('VERSION NOT RENDERABLE')
    expect(cell.text()).not.toContain('version 0')
  })
})

describe('what cannot be seen is said before it is signed', () => {
  // THE PROPERTY THIS ACT HAS THAT NEITHER SIBLING DOES. The queue serves the
  // digest and not the mandate, and no route expands one — propose, approve and
  // this queue are all three that exist. A signatory who believes this screen
  // showed them the change is exactly the failure two signatures prevent.

  it('tells the signatory the rules themselves are not on this surface', async () => {
    const wrapper = await open([entry()])
    const dialog = await confirm(wrapper)

    expect(dialog.text()).toContain('You cannot read this change here')
    expect(dialog.text()).toContain('WHICH rules, and which limits, are NOT on this surface')
    expect(dialog.text()).toContain('no diff to read')
  })

  it('shows the digest the signature covers, since it is the only handle there is', async () => {
    const wrapper = await open([entry({ digest: 'sha256:abcdef' })])
    const dialog = await confirm(wrapper)

    expect(dialog.text()).toContain('sha256:abcdef')
    expect(dialog.text()).toContain('digest your signature covers')
  })

  it('says to report it rather than sign when no digest was served', async () => {
    const wrapper = await open([entry({ digest: undefined })])
    const dialog = await confirm(wrapper)

    expect(dialog.text()).toContain('served no digest')
    expect(dialog.text()).toContain('Report this rather than signing')
  })

  it('shows the reason in full, since it is half the signed payload', async () => {
    const reason = 'raise the tech concentration cap after the mandate committee ruling'
    const wrapper = await open([entry({ reason })])

    // Not truncated on the row and not truncated in the confirmation: compliance
    // hashes the mandate AND the reason into the digest it re-checks.
    expect(cellsOf(wrapper, 0)[4]!.text()).toBe(reason)
    expect((await confirm(wrapper)).text()).toContain(reason)
  })
})

describe('the approve call names both ids and asserts nothing about the approver', () => {
  it('sends the portfolio id in the path and the proposal id in the body', async () => {
    const approve = vi.spyOn(mandatesApi.mandates, 'approve').mockResolvedValue(undefined)
    const wrapper = await open([entry({ portfolio_id: 'PF9', proposal_id: 'prop-42' })])
    await confirm(wrapper)
    await button(wrapper, 'Approve the mandate change')!.trigger('click')
    await flushPromises()

    // BOTH IDS, IN THAT ORDER. compliance matches the path's portfolio against
    // the proposal's stored subject, so approving on the proposal id alone would
    // make the portfolio on screen decorative.
    expect(approve).toHaveBeenCalledWith('PF9', 'prop-42')
    // No digest and no approver: compliance re-derives the first and takes the
    // second from the gateway-injected principal. A client-supplied actor is
    // #444's forged-signature defect.
    expect(approve.mock.calls[0]!.length).toBe(2)
  })

  it('reports the published mandate as in force, because this route decides now', async () => {
    vi.spyOn(mandatesApi.mandates, 'approve').mockResolvedValue(undefined)
    const wrapper = await open([entry()])
    await confirm(wrapper)
    await button(wrapper, 'Approve the mandate change')!.trigger('click')
    await flushPromises()

    // "submitted" would be the ORDER screen's wording and would understate a
    // completed act; a signatory who does not believe it landed signs it again.
    const notice = wrapper.find('[role="status"]')
    expect(notice.text()).toContain('PUBLISHED')
    expect(notice.text()).toContain('every replica arms with it')
    expect(notice.text()).not.toContain('submitted')
  })

  it('withholds the button from the proposer, by the server’s own comparison', async () => {
    // CASE- AND SPACE-INSENSITIVE, because dualcontrol.SameSubject folds that
    // way. A stricter client comparison offers a button that always 403s.
    signedInAs('  USER:Bob ')
    const wrapper = await open([entry({ proposer: 'user:bob' })])

    expect(button(wrapper, 'Approve')).toBeUndefined()
    const action = actionCell(wrapper).find('button')
    expect(action.attributes('disabled')).toBeDefined()
    expect(action.text()).toBe('You proposed this')
  })

  it('offers the button to a different person', async () => {
    signedInAs('user:carol')
    const wrapper = await open([entry({ proposer: 'user:bob' })])
    expect(button(wrapper, 'Approve')).toBeDefined()
  })
})

describe('a refusal is the control firing, not a failure to reach it', () => {
  async function refuse(status: number, message: string) {
    vi.spyOn(mandatesApi.mandates, 'approve').mockRejectedValue(new ApiError(status, message))
    const wrapper = await open([entry()])
    await confirm(wrapper)
    await button(wrapper, 'Approve the mandate change')!.trigger('click')
    await flushPromises()
    return wrapper
  }

  it('reports a 403 as refused, names both rules, and says the proposal survives', async () => {
    const wrapper = await refuse(403, 'the approver must be a different person from the proposer')

    const text = wrapper.find('[role="dialog"]').text()
    expect(text).toContain('REFUSED')
    expect(text).toContain('the control firing')
    expect(text).toContain('you are the person who proposed this change')
    // compliance checks the rule BEFORE it claims, precisely so a refused
    // self-approval cannot destroy a colleague's pending decision.
    expect(text).toContain('still PENDING')
  })

  it('reports a 409 as refused and NOT published, rather than as retryable', async () => {
    const wrapper = await refuse(409, 'this proposal expired before it was approved')

    const text = wrapper.find('[role="dialog"]').text()
    expect(text).toContain('was NOT published')
    expect(text).toContain('still governed by the mandate it had before')
    expect(text).not.toContain('try again')
  })

  it('reports a 502 as a VALID approval whose mandate is not in force', async () => {
    // THE ONE THAT IS NOT LIKE ITS SIBLINGS. compliance claims the proposal
    // before it publishes, so this leaves a valid approval, a consumed proposal
    // and no mandate. Nobody can retry — it must be proposed and signed again.
    const wrapper = await refuse(502, 'the approval was valid and the mandate was NOT published')

    const text = wrapper.find('[role="dialog"]').text()
    expect(text).toContain('YOUR APPROVAL WAS VALID AND THE MANDATE WAS NOT PUBLISHED')
    expect(text).toContain('must be PROPOSED AGAIN')
    expect(text).toContain('still governed by its previous mandate')
  })

  it('leaves the confirmation open so the reason sits beside what was attempted', async () => {
    const wrapper = await refuse(403, 'no')
    expect(wrapper.find('[role="dialog"]').exists()).toBe(true)
  })
})

describe('the queue read reports absent, forbidden and untenanted differently', () => {
  async function fail(status: number) {
    return open(new ApiError(status, 'x'))
  }

  it('says 404 is ABSENT — no mandate role, or no compliance service', async () => {
    const wrapper = await fail(404)
    const text = wrapper.find('[role="alert"]').text()
    expect(text).toContain('ABSENT, not forbidden')
    expect(text).toContain('no mandate role is configured')
    expect(text).toContain('does not mean no mandate change is waiting somewhere')
  })

  it('says 403 is FORBIDDEN, and that this is NOT the order-approval capability', async () => {
    const wrapper = await fail(403)
    const text = wrapper.find('[role="alert"]').text()
    expect(text).toContain('DOES hold mandate changes for a second signature')
    expect(text).toContain('SEPARATE capability from approving orders')
    // The reason the capabilities are split, so an operator does not "fix" it by
    // granting one role both.
    expect(text).toContain('sign away a limit')
  })

  it('says 401 is a session carrying no tenant, which is its own repair', async () => {
    const wrapper = await fail(401)
    expect(wrapper.find('[role="alert"]').text()).toContain('both a subject and a tenant')
  })

  it('never draws a failure as an empty queue', async () => {
    const wrapper = await fail(503)
    expect(wrapper.find('[role="alert"]').text()).toContain('NOT an empty queue')
    expect(wrapper.text()).not.toContain('No mandate change is awaiting a second signature')
  })

  it('treats a body that is not a list as unread, not as nothing held', async () => {
    // handlePendingChanges writes a bare array. Anything else is something other
    // than compliance answering, and coercing it would be the empty-queue
    // collapse arriving through the type system instead of through a status.
    const { api } = await import('../api/client')
    vi.spyOn(api, 'get').mockResolvedValue({ pending: [] })
    const wrapper = mount(MandateChangesView)
    await flushPromises()

    expect(wrapper.find('[role="alert"]').text()).toContain('treat it as unread')
    expect(wrapper.text()).not.toContain('No mandate change is awaiting a second signature')
  })

  it('clears a previously good list so a stale one cannot look current', async () => {
    const list = vi.spyOn(mandatesApi.mandates, 'list').mockResolvedValue([entry()])
    const wrapper = mount(MandateChangesView)
    await flushPromises()
    expect(wrapper.findAll('tbody tr').length).toBeGreaterThan(0)

    list.mockRejectedValue(new ApiError(503, 'x'))
    vi.spyOn(mandatesApi.mandates, 'approve').mockResolvedValue(undefined)
    await confirm(wrapper)
    await button(wrapper, 'Approve the mandate change')!.trigger('click')
    await flushPromises()

    expect(wrapper.find('table').exists()).toBe(false)
    expect(wrapper.find('[role="alert"]').text()).toContain('NOT an empty queue')
  })
})

describe('an empty queue is an answer', () => {
  it('states that nothing is held, and what that does not cover', async () => {
    const wrapper = await open([])

    const text = wrapper.text()
    expect(text).toContain('No mandate change is awaiting a second signature')
    expect(text).toContain('"nothing is held", not "nothing could be read"')
    // It also does not vouch for anything cmd/kanz-mandate published on one
    // person's authority before this surface existed.
    expect(text).toContain('single operator')
  })
})

describe('a window that closes while the page is open is stated before the click', () => {
  it('marks a still-pending proposal whose expiry has passed', async () => {
    const wrapper = await open([entry({ state: 'pending', expires_at: '2026-08-18T09:00:00Z' })])

    // compliance decides lapsed at READ time and the list is a snapshot, so an
    // entry that arrived pending can age out while somebody reads it. Saying so
    // before the click beats a 409 after it.
    const expires = cellsOf(wrapper, 0)[5]!
    expect(expires.text()).toContain('window closed')
    expect(expires.classes()).toContain('state-bad')

    expect((await confirm(wrapper)).text()).toContain('window has closed')
  })
})
