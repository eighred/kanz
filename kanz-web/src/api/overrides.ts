import { api, ApiError } from './client'
import { StateLapsed, StatePending } from './dualcontrol'

// THE PRICING-OVERRIDE QUEUE, as the browser sees it (#371 act one, #410).
//
// datamaster has carried a complete maker-checker workflow since #495/#498 and
// #539 opened the gateway doors to it, but no client ever reached them: an
// approver worked from curl or not at all. This is the sibling of the ORDER
// approvals module, and the differences below are NOT style — every one of them
// is forced by the server, and pretending otherwise is how the two screens start
// telling operators different things about the same control.
//
// # THREE WAYS THIS SURFACE DIFFERS FROM THE ORDER QUEUE, AND WHY
//
//  1. THE BODY IS A BARE JSON ARRAY, not an object with a `pending` field and an
//     owner tenant. handlePendingOverrides writes `[]map[string]any` directly.
//     There is no envelope to read a tenant out of.
//
//  2. APPROVING IS SYNCHRONOUS AND ITS 200 IS A REAL OUTCOME. The order path
//     publishes to the bus and answers 202 — nothing has decided yet — which is
//     why that screen may only ever say "submitted". Here the override is
//     APPLIED inside the request and the reply is the updated exception, and a
//     refusal comes back on the same request as a 403 or a 409 naming the rule.
//     internal/dualcontrol's own header records this as the reason the order path
//     needed a refusal recorded on its queue and this one does not. So this
//     screen may say "applied", and understating it would be its own dishonesty.
//
//  3. THERE IS NO DIGEST TO CARRY. The order approve route refuses an empty one
//     with 400 because the client supplies it; here datamaster recomputes it from
//     the STORED proposal (store.PayloadDigest over exception, reason and price)
//     and checks it itself. Nothing to pass — but it means the reason and the
//     price rendered on this screen ARE the payload the signature covers, which
//     is why they are shown in full and never abbreviated or rounded.
//
// The state vocabulary is internal/dualcontrol's, imported from ./dualcontrol so
// a third act cannot invent a third spelling (#558, #575).

/**
 * PendingOverride is one proposed price override awaiting a second signature.
 *
 * Field names are proposalJSON's, which is hand-built rather than protojson —
 * so unlike the order queue there is no EmitDefaultValues guarantee here. The
 * handler does write every key on every row, which is exactly why an absent one
 * means something other than datamaster answered.
 */
export interface PendingOverride {
  /** The unguessable id the approve call must name. Without it nothing is signable. */
  proposal_id: string
  /** The pricing-oversight exception being overridden; it is also the path id. */
  exception_id: string
  /** dualcontrol.Act — "PRICING_OVERRIDE" on this queue. */
  act?: string
  /** The AUTHENTICATED subject that proposed it (#444). The approver must differ. */
  proposer: string
  /**
   * The justification, and HALF THE SIGNED PAYLOAD. It is hashed into the digest
   * datamaster re-checks at approval time, so a screen that truncates it shows
   * an approver less than they are signing for.
   */
  reason?: string
  /**
   * The chosen price as `big.Rat.RatString()` — "130", or "261/2" for 130.5.
   *
   * A STRING, AND IT MUST STAY ONE. It is the other half of the signed payload,
   * and it is not a common.v1.Decimal: see formatRational in ./decimal for the
   * wire form and for why Number() is unavailable in both directions.
   */
  chosen_price?: string
  created_at?: string
  expires_at?: string
  /**
   * state is "pending" or "lapsed" and datamaster ALWAYS sets it.
   *
   * Read it through classify() rather than comparing here: an entry whose state
   * this client does not recognise must not render as untouched work.
   */
  state?: string
}

/**
 * Reading is how this screen renders one entry's state.
 *
 * THREE VALUES FOR TWO SERVER STATES, and the third is the point. proposalJSON
 * sets `state` on every row — its own comment says adding it only to lapsed ones
 * would make a client that ignores unknown keys read a lapsed proposal as work
 * waiting for it — so absence is never ambiguous on the server's side. A client
 * that treated anything-not-"lapsed" as pending would put that ambiguity back:
 * an empty state (an older datamaster, a proxy that dropped it, a value this
 * client has not learned) would render as a live proposal inviting a signature.
 *
 * THERE IS DELIBERATELY NO "refused" HERE. An override refusal comes back on the
 * same HTTP request; it never sits on this queue. See ./dualcontrol.
 */
export type Reading = 'pending' | 'lapsed' | 'unstated'

/**
 * classify reads an entry's state, DENYING BY DEFAULT.
 *
 * Only the exact literal "pending" reads as signable work. "lapsed" reads as
 * lapsed, and anything else — including the empty string — reads as unstated,
 * which the screen shows as loudly as a lapse. The direction matters: showing a
 * live proposal as unstated costs a moment's confusion and a re-read, while
 * showing a lapsed one as clean sends an approver to sign something the server
 * will refuse with 409 — and, worse, leaves an exception looking like it has a
 * resolution pending when in fact it died unsigned (#563).
 */
export function classify(p: PendingOverride): Reading {
  switch (p.state) {
    case StatePending:
      return 'pending'
    case StateLapsed:
      return 'lapsed'
    default:
      return 'unstated'
  }
}

/**
 * signable reports whether a second signature can still be given on this entry.
 *
 * ONLY A CLEAN "pending" IS. A lapsed proposal cannot be signed — dualcontrol
 * refuses it with ErrExpired and datamaster answers 409 — so offering the action
 * would be a lie the server then has to correct. An unstated one is refused for
 * the opposite reason: nobody knows whether it can be, and inviting a signature
 * on an unknown is exactly the deny-by-default this classification exists for.
 */
export function signable(p: PendingOverride): boolean {
  return classify(p) === 'pending' && !!p.proposal_id
}

export const overrides = {
  /**
   * pending lists every override proposal on this datamaster: live AND lapsed.
   *
   * AN EMPTY LIST IS AN ANSWER and must never be drawn as a spinner or an error.
   * An ERROR IS LIKEWISE NEVER DRAWN AS EMPTY — "nothing is held" and "datamaster
   * is unreachable" look identical that way, and only one of them is fine.
   *
   * BUT THE EMPTY LIST IS AMBIGUOUS, AND THE SCREEN SAYS SO. datamaster answers
   * `[]` both when nothing is pending and when it holds no proposal store at all
   * — i.e. when DATAMASTER_REQUIRE_DUAL_CONTROL is off, in which case overrides
   * apply on ONE signature and nothing will ever appear here. Its handler calls
   * that "a real answer", and for the server it is; for an approver watching an
   * empty queue it is the estate's own rule about "nothing configured" and
   * "checked, and fine" looking the same. The client cannot resolve it, so it
   * states it rather than implying the control is running.
   *
   * A NON-ARRAY BODY IS AN ERROR, NOT AN EMPTY QUEUE. Coercing it would be the
   * same collapse arriving through the type system instead of through a status.
   */
  list: async (): Promise<PendingOverride[]> => {
    const body = await api.get<unknown>('/api/v1/exceptions/pending-overrides')
    if (!Array.isArray(body)) {
      throw new Error(
        'the pending-override queue answered with something that is not a list, so what is ' +
          'awaiting a signature is unknown. This is NOT an empty queue — treat it as unread.',
      )
    }
    return body as PendingOverride[]
  },

  /**
   * approve gives the second signature, and it APPLIES the override on success.
   *
   * BOTH IDS TRAVEL, and neither is redundant. The exception id is in the path
   * and datamaster refuses a proposal whose subject does not match it — without
   * that check the path id would be decorative and an approver could be shown one
   * exception while signing for another. The proposal id is in the body because
   * it is what identifies the decision; it is minted from crypto/rand precisely
   * so it cannot be guessed by someone who was never shown the queue.
   *
   * `decision` IS SENT EXPLICITLY. The route refuses an empty one rather than
   * assuming, because both assumptions are wrong: defaulting to approve applies
   * an override nobody consented to, and defaulting to reject discards a decision
   * silently. This module only ever sends "approve" — withdrawing a proposal is
   * not part of this slice, and a "reject" wired without a screen that explains
   * what it destroys would be worse than its absence.
   *
   * THE APPROVER'S IDENTITY IS NOT SENT AT ALL. datamaster takes it from the
   * gateway-injected principal and refuses a body naming anyone else; a
   * self-asserted actor is the forged-signature defect #444 closed on this very
   * surface, and dual control over a forgeable identity is theatre.
   */
  approve: (exceptionId: string, proposalId: string) =>
    api.post<unknown>(
      `/api/v1/exceptions/${encodeURIComponent(exceptionId)}/override/approve`,
      { proposal_id: proposalId, decision: 'approve' },
    ),
}

/**
 * describeOverrideQueue turns a failure on the QUEUE READ into something an
 * approver can act on.
 *
 * 404 AND 403 ARE DIFFERENT DEPLOYMENTS AND MUST NOT BE COLLAPSED (#535). The
 * gateway does not register these routes at all unless API_GATEWAY_APPROVE_ROLE
 * names a role, because a route demanding a capability nobody holds would refuse
 * every principal that exists while reading as a working control. So 404 means
 * ABSENT — no approver role is configured here, or this datamaster serves another
 * tenant and refused to answer — while 403 means FORBIDDEN: the control is
 * running and this account is not one of its signatories. One of those is a
 * message for an operator and the other for whoever grants roles, and "something
 * went wrong" sends both to the wrong person.
 */
export function describeOverrideQueue(e: unknown): string {
  if (!(e instanceof ApiError)) {
    return e instanceof Error ? e.message : 'the request failed'
  }
  switch (e.status) {
    case 403:
      return 'Your account does not carry approval authority. This deployment DOES route the override queue — you are not one of its approvers. Ask whoever grants roles, not an operator.'
    case 404:
      return 'There is no override queue on this gateway. Dual control on pricing overrides is not switched on here (no approver role is configured), or the datamaster answering serves another tenant — this is ABSENT, not forbidden. It does not mean no override is waiting somewhere.'
    case 502:
    case 503:
      return 'datamaster is unreachable, so what is awaiting a second signature is unknown. This is NOT an empty queue — an override may be held right now, and an exception may be sitting unresolved because of it.'
    case 500:
      return 'datamaster could not read its proposal store, so the queue is unknown. This is NOT an empty queue.'
    default:
      return e.message
  }
}

/**
 * describeApproveFailure explains a failure to give the second signature.
 *
 * A SEPARATE FUNCTION because the same status means something different on the
 * write — and unlike the order path, some of these are REFUSALS rather than
 * failures to ask. datamaster decides inside the request, so a 403 or a 409 here
 * is the control firing and the override was NOT applied. Reporting either as a
 * transport problem would invite a retry that cannot succeed.
 *
 * The server's own sentence is appended where it names WHICH rule fired: 403 is
 * either "you are not an approver" or "you are the proposer", and 409 is one of
 * expired, payload-changed, or already-decided. Those are three different next
 * steps and the client must not flatten them into one guess.
 */
export function describeApproveFailure(e: unknown): string {
  if (!(e instanceof ApiError)) {
    return e instanceof Error ? e.message : 'the approval could not be submitted'
  }
  switch (e.status) {
    case 400:
      return `The approval was refused as malformed, so nothing was signed and the override was not applied. datamaster said: ${e.message}`
    case 403:
      return `REFUSED — this is the control firing, not a failure to reach it. Either your account does not carry approval authority, or you are the person who proposed this override and dual control needs a second person. datamaster said: ${e.message}`
    case 404:
      return 'This proposal is not on this queue: it has already been decided, it names a different exception, or this gateway registers no approve route. Re-read the queue rather than retrying — nothing was signed.'
    case 409:
      return `REFUSED, and the override was NOT applied. The proposal expired before you signed it, its stored values no longer match what was proposed, or somebody else has already decided it. datamaster said: ${e.message}`
    case 500:
      return 'datamaster could not complete the approval. The override may or may not have been applied — re-read the queue and the exception before trying again, because a second approval on an applied override would append a second entry to an append-only trail.'
    case 502:
      return 'The approval never reached datamaster, so nothing was signed and the override was not applied.'
    case 503:
      return 'The datamaster surface is disabled on this gateway, so the approval was not submitted. The override is still held.'
    default:
      return e.message
  }
}
