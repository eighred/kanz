import { api, ApiError } from './client'
import type { Decimal } from './decimal'

// The dual-control queue, as the browser sees it (#371, #539, #558).
//
// SNAKE_CASE WITH ZEROS PRESENT, like risk.ts and unlike control.ts. Both routes
// below are served by the api-gateway's protojson marshaler with UseProtoNames +
// EmitDefaultValues, so an unset string arrives as "" rather than being omitted.
// That matters for `state` in particular: see PendingApproval.state.
//
// # THE APPROVE CALL IS ASYNCHRONOUS AND ITS 202 IS NOT AN APPROVAL
//
// POST /v1/orders/{id}/approve publishes an ApproveOrder command to the bus and
// answers 202 immediately. The OMS decides afterwards, and internal/dualcontrol
// is the thing that can refuse — self-approval, a changed payload, an expired
// proposal. There is no synchronous channel to answer a refusal on, which
// internal/dualcontrol's own header records as the reason the OMS writes the
// refusal ONTO the proposal instead (#558).
//
// So a screen that renders the 202 as "approved" is claiming an outcome nothing
// has decided, and the one failure that produces is the one dual control exists
// to prevent: a person believes the order was released when their signature was
// refused. The only honest reading of a 202 is "the approval was submitted", and
// the only place the answer appears is this queue, re-read.

/** ApprovalState is internal/dualcontrol's shared vocabulary, OMS half. */
export type ApprovalState = 'pending' | 'refused'

/**
 * RefusalReason is the CLOSED SET services/oms/internal/order/service.go names.
 *
 * It is a code and not a sentence precisely so a client can branch on it: an
 * approver refused for self_approval needs a different next step (find somebody
 * else) from one refused for payload_changed (the terms moved; it must be
 * re-proposed). `unclassified` is not a fifth rule — it means the OMS refused
 * and could not name which rule did, which is a defect to report rather than
 * anything the approver can fix.
 */
export type RefusalReason =
  | 'self_approval'
  | 'expired'
  | 'payload_changed'
  | 'malformed'
  | 'lost_the_claim'
  | 'unclassified'

/**
 * ProposedCommand is order.v1.SubmitOrder — the command being signed for.
 *
 * NOT an OrderState, and there is no OrderState to have: the order was never
 * admitted. Prices and quantities here are common.v1.Decimal (SubmitOrder's own
 * type), NOT common.v1.Money as the order-history read surface uses — the two
 * messages differ and reading one as the other renders a price as nothing.
 */
export interface ProposedCommand {
  order_id?: string
  portfolio_id?: string
  instrument_id?: string
  side?: string
  quantity?: Decimal
  order_type?: string
  limit_price?: Decimal
  stop_price?: Decimal
  time_in_force?: string
  venue?: string
}

/** PendingApproval is one order waiting for its second signature. */
export interface PendingApproval {
  order_id: string
  command?: ProposedCommand
  /** The AUTHENTICATED subject that proposed it. The approver must be someone else. */
  proposer: string
  act?: string
  /**
   * digest is the value a signature COVERS, and the approve route refuses an
   * empty one with 400. It rides no other surface a person can read — the FACT
   * carrying it goes to NATS — so this queue is the only place it exists for a
   * human. A screen that drops it makes approving impossible.
   */
  digest: string
  proposed_at?: string
  expires_at?: string
  /**
   * state is "pending" or "refused" and the OMS ALWAYS sets it.
   *
   * Read it through classify() rather than comparing here: an entry whose state
   * this client does not recognise must not render as untouched work.
   */
  state?: string
  last_refusal_reason?: string
  last_refused_by?: string
  last_refused_at?: string
}

export interface PendingApprovalsResponse {
  pending?: PendingApproval[]
  owner_tenant?: string
}

/**
 * Reading is how this screen renders one entry's state.
 *
 * THREE VALUES FOR TWO SERVER STATES, and the third is the point. The OMS sets
 * `state` on every row so that absence is never ambiguous — but a client that
 * treats anything-not-"refused" as pending re-introduces exactly the ambiguity
 * the field removed: an empty state (an older server, a proxy that dropped it, a
 * field this client has not learned yet) would render as clean work awaiting a
 * signature. It is not; it is a queue whose state nobody stated.
 */
export type Reading = 'pending' | 'refused' | 'unstated'

/**
 * classify reads an entry's state, DENYING BY DEFAULT.
 *
 * Only the exact literal "pending" reads as untouched work. "refused" reads as
 * refused, and anything else — including the empty string — reads as unstated,
 * which the screen shows as loudly as a refusal. The direction matters: the cost
 * of showing a clean entry as unstated is a moment's confusion, and the cost of
 * showing a refused one as clean is an approver believing the order is merely
 * waiting when somebody's signature was already turned away.
 */
export function classify(p: PendingApproval): Reading {
  switch (p.state) {
    case 'pending':
      return 'pending'
    case 'refused':
      return 'refused'
    default:
      return 'unstated'
  }
}

/**
 * samePerson compares two subjects the way internal/dualcontrol does.
 *
 * CASE- AND SPACE-INSENSITIVE, because that is the comparison the server makes:
 * "Alice@kanz" approving "alice@kanz" is one person, and a case-sensitive
 * comparison would call it two. Matching the server's rule here is what makes
 * the disabled button agree with the refusal that would follow — a stricter
 * client comparison would offer a button that always fails, and a looser one
 * would hide a button that would have worked.
 *
 * IT IS NOT A CONTROL. The OMS refuses self-approval whatever this returns; the
 * only thing this buys is that the proposer is not invited to discover the rule
 * by pressing a button and waiting for an asynchronous refusal.
 */
export function samePerson(a: string | undefined, b: string | undefined): boolean {
  if (!a || !b) return false
  return a.trim().toLowerCase() === b.trim().toLowerCase()
}

export const approvals = {
  /**
   * pending lists everything awaiting a second signature in this tenant.
   *
   * NO PORTFOLIO FILTER IS SENT, deliberately: the gateway documents that an
   * empty portfolio_id means "everything still awaiting a signature", because
   * the whole-queue question is the one an approver actually asks. A queue that
   * can only be read one portfolio at a time is a queue nobody watches.
   *
   * AN EMPTY LIST IS AN ANSWER — nothing is held — and it must never be rendered
   * as a spinner or as an error. An ERROR is likewise never rendered as empty:
   * the gateway's own comment says so, because "nothing to approve" and "the OMS
   * is unreachable" look identical that way and only one of them is fine.
   */
  list: () => api.get<PendingApprovalsResponse>('/api/v1/orders/pending-approvals'),

  /**
   * approve gives the second signature. It returns when the command is PUBLISHED.
   *
   * The digest is required by the route (400 without it) and is passed straight
   * from the queue entry. The order id travels in the PATH and not the body: the
   * gateway overwrites a body-supplied id from the path anyway — "a
   * caller-supplied order id approves somebody else's order" — so sending one
   * would be a second spelling of the same identity for no gain.
   *
   * The approver's identity is NOT sent at all. The gateway binds it from the
   * authenticated principal; a self-asserted issuer is the forged-actor defect
   * #444 closed on the override path, and dual control over a forgeable identity
   * is theatre.
   */
  approve: (orderId: string, digest: string) =>
    api.post<{ order_id?: string; status?: string }>(
      `/api/v1/orders/${encodeURIComponent(orderId)}/approve`,
      { digest },
    ),
}

/**
 * describeApprovals turns a failure on the QUEUE READ into something an approver
 * can act on.
 *
 * 404 AND 403 ARE DIFFERENT DEPLOYMENTS AND MUST NOT BE COLLAPSED. #535 is the
 * rule: a route demanding a capability granted to nobody would refuse every
 * principal that exists while reading as a working control, so the gateway does
 * not register the queue at all when no approver role is named. The result is
 * that 404 means ABSENT — this deployment runs no dual control, or fronts no OMS
 * read surface — while 403 means FORBIDDEN: the control exists, and this account
 * is not one of its signatories. One of those is a message for an operator and
 * the other is a message for whoever grants roles, and "something went wrong"
 * sends both to the wrong person.
 *
 * The 404 also covers a reply stamped with another tenant, which the gateway's
 * writeOwned refuses rather than answering — so the wording does not promise
 * that nothing is held, only that nothing is visible here.
 */
export function describeApprovals(e: unknown): string {
  if (!(e instanceof ApiError)) {
    return e instanceof Error ? e.message : 'the request failed'
  }
  switch (e.status) {
    case 403:
      return 'Your account does not carry approval authority. This deployment DOES hold orders for a second signature — you are not one of its approvers. Ask whoever grants roles, not an operator.'
    case 404:
      return 'There is no approvals queue on this gateway. Dual control is not switched on here (no approver role is configured, or this gateway fronts no OMS read surface), or the OMS answering serves another tenant — this is ABSENT, not forbidden. It does not mean no order is being held somewhere.'
    case 502:
    case 503:
      return 'The OMS is unreachable, so what is awaiting a signature is unknown. This is NOT an empty queue — orders may be held right now.'
    default:
      return e.message
  }
}

/**
 * describeApproveFailure explains a failure to SUBMIT an approval.
 *
 * A separate function from describeApprovals because the same status means
 * something different on the write: this route is registered on the approver
 * role ALONE — deliberately, so that "this deployment takes no approvals" and
 * "you are not the approver" stay different answers — and it publishes to a bus
 * rather than reading from the OMS.
 *
 * IT DESCRIBES THE PUBLISH AND NOTHING MORE. Every one of these is a failure to
 * ASK; none of them is a refusal, because a refusal cannot arrive here at all.
 */
export function describeApproveFailure(e: unknown): string {
  if (!(e instanceof ApiError)) {
    return e instanceof Error ? e.message : 'the approval could not be submitted'
  }
  switch (e.status) {
    case 400:
      // The route refuses an approval carrying no digest. Reaching this means
      // the queue entry had none, which is a defect in what was served rather
      // than something a retry fixes.
      return 'The gateway refused the approval as unsigned: it carried no digest, so it would have covered nothing. The queue entry is missing the value to sign — report this rather than retrying.'
    case 403:
      return 'You may not issue this approval. Either your account does not carry approval authority, or your session carries no tenant — a token that does not say whose capital it may spend cannot release an order.'
    case 404:
      return 'This gateway registers no approve route, so no signature can be given here. Dual control is not configured on this deployment.'
    case 503:
      return 'Order writes are disabled on this gateway, so the approval was not submitted. The order is still held.'
    case 502:
      return 'The approval could not be published to the bus, so it never reached the OMS. The order is still held — nothing was signed.'
    default:
      return e.message
  }
}

/**
 * describeRefusal says what a refusal code means and what to do next.
 *
 * Each arm names a DIFFERENT person to go to, which is the whole reason the OMS
 * publishes a code rather than a sentence. Anything unrecognised is reported as
 * unrecognised rather than smoothed into the closest known reason — a new code
 * this client has not learned about is a message from a newer server, and
 * guessing at it would tell the approver something untrue.
 */
export function describeRefusal(reason: string | undefined): string {
  switch (reason) {
    case 'self_approval':
      return 'refused as a self-approval: the signature came from the person who proposed it. Dual control needs a second person — find another approver.'
    case 'expired':
      return 'refused because the proposal had expired before the signature arrived. It can no longer be released and must be proposed again.'
    case 'payload_changed':
      return 'refused because the order changed after it was proposed, so the signature would not have covered what would be sent. It must be re-proposed on the current terms.'
    case 'malformed':
      return 'refused because the proposal is not well-formed enough to decide on. It must be re-proposed.'
    case 'lost_the_claim':
      // THE ONE REFUSAL A RETRY FIXES, AND THE RETRY HAS A TRAP. The gateway
      // falls back to the ORDER ID as the command's idempotency key when no
      // Idempotency-Key header is sent, and the producer stamps that as
      // Nats-Msg-Id — which JetStream dedups on. An immediate second attempt on
      // the same order is therefore liable to be dropped by the broker while
      // this screen still shows a 202. Saying "try again" without saying "not
      // instantly" would send an approver round a loop that cannot end.
      return 'refused because another OMS replica was already handling this proposal. Nothing is wrong with the order — re-read the queue and approve again, but leave a gap: a second attempt sent straight away carries the same idempotency key and the broker may drop it.'
    case 'unclassified':
      return 'refused for a reason the OMS could not name. That is a defect in the platform rather than in this order — report it; re-approving will not help.'
    default:
      return `refused for "${reason}", which this screen does not recognise. Do not assume the order is fine — ask whoever runs the OMS.`
  }
}
