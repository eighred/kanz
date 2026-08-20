import { api, ApiError } from './client'
import { StateLapsed, StatePending } from './dualcontrol'

// THE MANDATE-CHANGE QUEUE, as the browser sees it (#371, act two of #410).
//
// A mandate is the control every order is checked against. PR #595 made changing
// one take two separately authenticated PEOPLE rather than two invocations of
// cmd/kanz-mandate — whose own doc admitted both invocations ran on one
// operator's machine under one SVID, so a unilateral change was detectable and
// never preventable. That mattered more here than for the other two acts:
// relax the constraint, then place the order it would have refused, and both
// acts read as correct in the trail.
//
// And then no client reached it. The routes exist behind authz.Mandate; an
// approver's options were curl or nothing. This module is the browser half.
//
// # THIS IS THE THIRD DUAL-CONTROL SURFACE AND IT INVENTS NOTHING
//
// The order queue (#580) and the pricing-override queue (#590) came first. The
// state vocabulary is imported from ./dualcontrol rather than respelled here, the
// person-comparison is theirs, and the deny-by-default classification below is
// the same shape as ../api/overrides. Three dual-control screens behaving
// differently is the divergence this repository keeps paying for.
//
// # HOW IT DIFFERS FROM THE OVERRIDE QUEUE, AND WHY EACH DIFFERENCE IS FORCED
//
//  1. THE APPROVE PATH NAMES THE PORTFOLIO, NOT AN EXCEPTION. compliance checks
//     the path's portfolio against the proposal's stored subject — which carries
//     BOTH the tenant and the portfolio — so one comparison is the tenant gate
//     and the "an approver must not be shown one portfolio while signing for
//     another" gate. Passing the proposal id alone would make the portfolio on
//     screen decorative.
//
//  2. `decision` IS "approve" OR "reject", AND THE ROUTE REFUSES AN EMPTY ONE.
//     Same stance as the override path: defaulting to approve publishes a mandate
//     nobody consented to, defaulting to reject discards a decision silently.
//     THIS MODULE SENDS ONLY "approve" — see approve() on why reject is absent.
//
//  3. THERE IS NO DIGEST TO CARRY, unlike the ORDER queue whose approve route
//     refuses an empty one with 400. compliance re-derives the digest from the
//     STORED mandate and reason and compares it itself, so a stored mandate that
//     changed under the proposal is refused rather than published. The digest is
//     still SERVED on the queue, and it is still the only thing binding a
//     signature to a payload — which is why this screen shows it.
//
//  4. THERE IS NO DECIMAL ON THIS SURFACE AT ALL, and that was checked rather
//     than assumed. #590 found datamaster sending big.Rat RatString()s, so
//     "130.5" arrived as "261/2"; nothing of the sort happens here. compliance's
//     proposalJSON is hand-built with encoding/json and every field it emits is a
//     string except two — `version` (uint64) and `rule_count` (int) — which ride
//     the wire as JSON NUMBERS. formatRational and formatDecimal are therefore
//     both inapplicable, and importing one "for consistency" would render every
//     field as null. The numbers have their own hazard; see below.
//
// # WHAT THIS SURFACE DOES NOT CARRY, WHICH IS THE THING WORTH SAYING LOUDEST
//
// IT DOES NOT CARRY THE MANDATE. proposalJSON's own comment says the digest
// travels "and not the mandate", because "the mandate is fetched by proposal id
// rather than splashed across a list" — but NO SUCH FETCH EXISTS. compliance
// registers exactly three routes and the gateway fronts exactly those three:
// propose, approve, and this queue. There is no GET for one proposal, so the
// rules an approver is about to put in force are unreachable by any client.
//
// What a signatory can actually see is: which portfolio, which mandate id, what
// version it becomes, HOW MANY rules it carries, the stated reason, and the
// digest. Not which rules, not which limits, and not what the portfolio is
// governed by today — `previous` is deliberately nil on publish too, so no diff
// exists anywhere on the platform.
//
// This module does not paper over that and it does not invent an endpoint to
// close it. It names the gap in describeVisibility() so the screen can put it in
// front of the person at the moment they sign. An approver signing a change they
// cannot read is the forgeable-actor problem in a new costume: the two names on
// the record are real, and neither of them read the rule.

/**
 * PendingMandateChange is one proposed mandate change awaiting a second signature.
 *
 * Field names are compliance's proposalJSON, which is a hand-built
 * map[string]any run through encoding/json — NOT protojson. So there is no
 * EmitDefaultValues guarantee the way ../api/approvals has one. The handler does
 * write every key on every row, which is exactly why an absent one means
 * something other than compliance answered.
 */
export interface PendingMandateChange {
  /** The unguessable id the approve call must name. Minted from crypto/rand. */
  proposal_id: string
  /** dualcontrol.Act — "MANDATE_CHANGE" on this queue. */
  act?: string
  /**
   * The portfolio this mandate governs. IT IS ALSO THE APPROVE PATH'S id, and
   * compliance refuses a proposal whose subject does not match it.
   */
  portfolio_id: string
  /** The mandate's stable identity, unchanged across versions. */
  mandate_id?: string
  /**
   * The version this change becomes — monotonic, uint64, and a JSON NUMBER.
   *
   * Read it through renderVersion(). Above 2^53 the value is already damaged by
   * JSON.parse before this module sees it, and printing the damaged one as fact
   * is worse than saying it did not survive.
   */
  version?: number
  /**
   * How many rules the proposed mandate carries — an int, and a JSON NUMBER.
   *
   * ZERO IS LEGAL AND IT MEANS SOMETHING. compliance's own comment: "an empty
   * ruleset is legal and means 'governed by a mandate that constrains nothing',
   * which is different from having no mandate — the approver has to be able to
   * see that is what they are signing." Read it through renderRuleCount(), which
   * is what keeps 0 distinct from absent.
   */
  rule_count?: number
  /** The AUTHENTICATED subject that proposed it. The approver must differ. */
  proposer: string
  /**
   * The justification, and HALF THE SIGNED PAYLOAD — comp.MandateDigest hashes
   * the mandate AND the reason, and compliance re-checks it at approval time.
   * The route refuses a proposal without one, because "an unexplained change to
   * what governs a portfolio is not auditable".
   *
   * On a surface that cannot show the rules, this is also very nearly the only
   * human-readable statement of what is changing. It must never be truncated.
   */
  reason?: string
  /**
   * digest is what the signature COVERS.
   *
   * It is NOT sent back on approval — compliance re-derives it from the stored
   * mandate — so unlike the order queue it is not a value this client must relay.
   * It is shown because it is the one field that ties what a person read to what
   * the published FACT will carry, and on a queue that cannot show the rules it
   * is the only handle an auditor has.
   */
  digest?: string
  created_at?: string
  expires_at?: string
  /**
   * state is "pending" or "lapsed" and compliance ALWAYS sets it.
   *
   * Read it through classify() rather than comparing here: an entry whose state
   * this client does not recognise must not render as untouched work.
   */
  state?: string
}

/**
 * Reading is how this screen renders one entry's state.
 *
 * THREE VALUES FOR TWO SERVER STATES, and the third is the point — the same
 * argument ../api/overrides makes, for the same reason. compliance sets `state`
 * on every row (its comment: adding it only to lapsed ones "would make a client
 * that ignores unknown keys read a lapsed proposal as work waiting for them"), so
 * absence is never ambiguous on the server's side. A client that treated
 * anything-not-"lapsed" as pending would put that ambiguity straight back.
 *
 * THERE IS DELIBERATELY NO "refused" HERE. A mandate-change refusal comes back on
 * the same HTTP request as a 403 or a 409; it never sits on this queue. That is
 * internal/dualcontrol's recorded difference between the three acts, not an
 * omission to repair. See ./dualcontrol.
 */
export type Reading = 'pending' | 'lapsed' | 'unstated'

/**
 * classify reads an entry's state, DENYING BY DEFAULT.
 *
 * Only the exact literal "pending" reads as signable work. Anything else —
 * including the empty string — reads as unstated, which the screen shows as
 * loudly as a lapse. The direction matters more here than on either sibling:
 * showing a live proposal as unstated costs a re-read, while showing a LAPSED
 * mandate change as clean sends a signatory to sign something compliance answers
 * 409 to — and leaves a portfolio looking like its constraint is about to change
 * when in fact it is still governed by the old one, unchanged and unwatched.
 */
export function classify(p: PendingMandateChange): Reading {
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
 * ONLY A CLEAN "pending" WITH BOTH IDS IS. compliance refuses a lapsed proposal
 * with 409, so offering the action would be a lie the server then corrects. An
 * unstated one is refused for the opposite reason — nobody knows whether it can
 * be signed, and inviting a signature on an unknown is exactly what the
 * deny-by-default classification exists to prevent.
 *
 * THE PORTFOLIO ID IS REQUIRED TOO, and not merely for the URL. It is what
 * compliance matches against the proposal's stored subject; without it there is
 * no route to call, and a client that guessed one would be asking to sign for a
 * portfolio nobody displayed.
 */
export function signable(p: PendingMandateChange): boolean {
  return classify(p) === 'pending' && !!p.proposal_id && !!p.portfolio_id
}

/**
 * renderRuleCount returns the rule count as text, or null when it was not stated.
 *
 * # NO COERCION, IN EITHER DIRECTION
 *
 * ZERO IS NOT ABSENT. `p.rule_count || 'unknown'` and `v-if="p.rule_count"` both
 * read a legal, meaningful 0 as nothing-stated — and 0 rules is a mandate that
 * constrains NOTHING, which is the single most consequential thing this queue can
 * be asked to approve. Hiding it behind a falsy check is how an unconstrained
 * mandate gets signed as though the field had simply not loaded.
 *
 * ABSENT IS NOT ZERO EITHER. `Number(p.rule_count)` turns undefined into NaN but
 * turns "" and null into 0 — which would print "constrains nothing" over a row
 * whose count nobody sent. So a non-number is refused rather than converted: a
 * string count is an older or proxied server answering, and guessing at it tells
 * a signatory something untrue about the control they are putting in force.
 *
 * This is the same contract formatDecimal takes in ./decimal — null is not zero,
 * and callers must not substitute one — arriving on a surface that has no
 * decimals at all.
 */
export function renderRuleCount(p: PendingMandateChange): string | null {
  const n = p.rule_count
  if (typeof n !== 'number' || !Number.isSafeInteger(n) || n < 0) return null
  return String(n)
}

/**
 * renderVersion returns the version as text, or null when it did not survive.
 *
 * THE DAMAGE HAPPENS BEFORE THIS FUNCTION AND CANNOT BE UNDONE HERE. `version` is
 * a uint64 that compliance emits as a JSON number, so a value above 2^53 is
 * already rounded by JSON.parse by the time any of this runs — 18446744073709551615
 * arrives as 18446744073709552000. This client cannot recover the true value; the
 * only honest options are to print the damaged one as fact or to say it did not
 * survive, and on the version that pins which constraint an order is audited
 * against, the second is the only defensible one.
 *
 * (The repair is server-side and is not this PR's: compliance would have to emit
 * the version as a string, the way protojson already does for every uint64 it
 * marshals. Until then this reports rather than hides.)
 */
export function renderVersion(p: PendingMandateChange): string | null {
  const v = p.version
  if (typeof v !== 'number' || !Number.isSafeInteger(v) || v < 0) return null
  return String(v)
}

export const mandates = {
  /**
   * pending lists every mandate change on this tenant: live AND lapsed.
   *
   * NO PORTFOLIO FILTER IS SENT, and none is available — compliance scopes the
   * read by the gateway-injected tenant alone. That is the question a signatory
   * actually asks, and a queue readable one portfolio at a time is a queue nobody
   * watches.
   *
   * AN EMPTY LIST IS AN ANSWER and must never be drawn as a spinner or an error.
   * AN ERROR IS LIKEWISE NEVER DRAWN AS EMPTY — "nothing is held" and "compliance
   * is unreachable" look identical that way, and only one of them is fine.
   *
   * A NON-ARRAY BODY IS AN ERROR, NOT AN EMPTY QUEUE. handlePendingChanges writes
   * a bare `[]map[string]any`, like datamaster's override queue and unlike the
   * OMS's enveloped one, so anything else is something other than compliance
   * answering. Coercing it would be the same collapse arriving through the type
   * system instead of through a status.
   */
  list: async (): Promise<PendingMandateChange[]> => {
    const body = await api.get<unknown>('/api/v1/mandates/pending-changes')
    if (!Array.isArray(body)) {
      throw new Error(
        'the pending mandate-change queue answered with something that is not a list, so what is ' +
          'awaiting a signature is unknown. This is NOT an empty queue — treat it as unread.',
      )
    }
    return body as PendingMandateChange[]
  },

  /**
   * approve gives the second signature, and it PUBLISHES the mandate on success.
   *
   * BOTH IDS TRAVEL AND NEITHER IS REDUNDANT. The portfolio id is in the path and
   * compliance refuses a proposal whose stored subject does not match it — that
   * one comparison is both the tenant gate and the guarantee that the portfolio
   * on screen is the portfolio being signed for. The proposal id is in the body
   * because it identifies the decision; it is minted from crypto/rand precisely
   * so it cannot be guessed by someone who was never shown this queue.
   *
   * `decision` IS SENT EXPLICITLY, because the route refuses an empty one.
   *
   * ONLY "approve" IS SENT FROM HERE. compliance accepts "reject", and a reject
   * consumes the proposal — the proposer must start again — so wiring the verb
   * without a screen that says what it destroys would be worse than its absence.
   * That is one slice further on; the same call is a one-line change when it has
   * a confirmation of its own.
   *
   * THE APPROVER'S IDENTITY IS NOT SENT AT ALL. compliance takes it from the
   * gateway-injected principal and has no field to name anyone else. A
   * self-asserted actor is #444's forged-signature defect, and dual control over a
   * forgeable identity is theatre.
   *
   * NO DIGEST IS SENT. compliance re-derives it from the stored mandate and
   * reason; a client-supplied one would let the caller state what their own
   * signature covers.
   */
  approve: (portfolioId: string, proposalId: string) =>
    api.post<unknown>(
      `/api/v1/portfolios/${encodeURIComponent(portfolioId)}/mandate/approve`,
      { proposal_id: proposalId, decision: 'approve' },
    ),
}

/**
 * describeMandateQueue turns a failure on the QUEUE READ into something a
 * signatory can act on.
 *
 * 404 AND 403 ARE DIFFERENT DEPLOYMENTS AND MUST NOT BE COLLAPSED (#535). The
 * gateway registers these three routes only when API_GATEWAY_MANDATE_ROLE names a
 * role, because authz.Mandate is carried by nobody otherwise and a route whose
 * capability nobody holds refuses every principal that exists while reading as a
 * working control. So 404 means ABSENT — no mandate signatory is configured here,
 * or this gateway fronts no compliance service — while 403 means FORBIDDEN: the
 * control is running and this account is not one of its signatories.
 *
 * AND authz.Mandate IS NOT authz.Approve, which is why the 403 text does not send
 * an order approver away thinking they already hold this. The capabilities are
 * deliberately distinct: under one, the second signature on the mandate change
 * and the second signature on the held order come from the same pool, so one
 * signatory could sign away the limit and then sign the trade the limit existed
 * to stop. Someone who can approve orders and cannot read this queue is looking
 * at the control working, not at a misconfiguration.
 *
 * 401 IS ITS OWN ANSWER HERE, unlike on either sibling. compliance answers 401
 * when the principal carries no TENANT — not merely no subject — because
 * neither gateway authenticator demands the tenant claim, so a perfectly valid
 * token can carry none, and a proposal filed under an empty key lists on no queue
 * and is approvable by nobody.
 */
export function describeMandateQueue(e: unknown): string {
  if (!(e instanceof ApiError)) {
    return e instanceof Error ? e.message : 'the request failed'
  }
  switch (e.status) {
    case 401:
      return 'Your session does not carry both a subject and a tenant, and compliance refuses to say whose mandates these would be. Sign in again; if it recurs, the token this deployment issues is missing its tenant claim and that is for whoever runs identity.'
    case 403:
      return 'Your account does not carry mandate-change authority. This deployment DOES hold mandate changes for a second signature — you are not one of their signatories. Note that this is a SEPARATE capability from approving orders, deliberately: one person holding both could sign away a limit and then sign the trade that limit existed to stop. Ask whoever grants roles.'
    case 404:
      return 'There is no mandate-change queue on this gateway. Dual control on mandate changes is not switched on here (no mandate role is configured), or this gateway fronts no compliance service — this is ABSENT, not forbidden. It does not mean no mandate change is waiting somewhere.'
    case 502:
    case 503:
      return 'compliance is unreachable, so what is awaiting a second signature is unknown. This is NOT an empty queue — a change to what governs a portfolio may be held right now, and it may lapse unsigned while this is unreadable.'
    case 500:
      return 'compliance could not read its proposal store, so the queue is unknown. This is NOT an empty queue.'
    default:
      return e.message
  }
}

/**
 * describeApproveFailure explains a failure to give the second signature.
 *
 * A SEPARATE FUNCTION because the same status means something different on the
 * write — and, as on the override path and unlike the order path, several of
 * these are REFUSALS rather than failures to ask. compliance decides inside the
 * request, so a 403 or a 409 here is the control firing and the mandate was NOT
 * published. Reporting either as a transport problem would invite a retry that
 * cannot succeed.
 *
 * THE 502 IS THE ONE THAT IS NOT LIKE ITS SIBLINGS, and it is the reason this
 * function exists rather than a shared one. compliance CLAIMS the proposal before
 * it publishes, so a publish failure leaves an approval that was VALID, a
 * proposal that is CONSUMED, and a mandate that is NOT in force. Nobody can
 * simply retry: it has to be proposed again by someone. A generic "could not
 * reach the server" would leave two people believing they had changed what
 * governs a portfolio when nothing changed at all.
 */
export function describeApproveFailure(e: unknown): string {
  if (!(e instanceof ApiError)) {
    return e instanceof Error ? e.message : 'the approval could not be submitted'
  }
  switch (e.status) {
    case 400:
      return `The approval was refused as malformed, so nothing was signed and the mandate was not published. compliance said: ${e.message}`
    case 401:
      return 'Your session does not carry both a subject and a tenant, so compliance could not tell who is signing. Nothing was signed and the mandate was not published.'
    case 403:
      return `REFUSED — this is the control firing, not a failure to reach it. Either your account does not carry mandate-change authority, or you are the person who proposed this change and it takes a second person. The proposal is still PENDING: a refused self-approval does not consume it, so whoever may legitimately sign it still can. compliance said: ${e.message}`
    case 404:
      return 'This proposal is not on this queue: it has already been decided, it names a different portfolio or tenant, or this gateway registers no approve route. Re-read the queue rather than retrying — nothing was signed. compliance answers the same 404 to all of those on purpose, so this route cannot be used to discover another tenant’s pending mandate changes.'
    case 409:
      return `REFUSED, and the mandate was NOT published. The proposal expired before you signed it, the stored mandate no longer matches what was proposed, or somebody else has already decided it. The portfolio is still governed by the mandate it had before. compliance said: ${e.message}`
    case 500:
      return 'compliance could not complete the approval. Re-read the queue before trying again, and do not assume either outcome.'
    case 502:
      return 'YOUR APPROVAL WAS VALID AND THE MANDATE WAS NOT PUBLISHED. compliance consumed the proposal and then failed to reach the broker, so this cannot be retried — the change must be PROPOSED AGAIN, and signed again by two people. Until then the portfolio is still governed by its previous mandate. Tell whoever proposed it.'
    case 503:
      return 'The mandate surface is disabled on this gateway, so the approval was not submitted. The change is still held.'
    default:
      return e.message
  }
}

/**
 * describeVisibility states what a signatory can and cannot see of the change.
 *
 * IT IS NOT A DISCLAIMER AND IT IS NOT DEFENSIVE PADDING. A mandate is the
 * control every order is checked against, so the one question this screen exists
 * to answer is "what is changing?" — and the queue does not carry the answer.
 * compliance serves the digest and not the mandate, on the stated reasoning that
 * "the mandate is fetched by proposal id rather than splashed across a list", and
 * no route to fetch one exists: propose, approve and this queue are all three.
 *
 * So what is visible is the shape of the change — which portfolio, which mandate,
 * what version it becomes, how many rules it will carry — plus the proposer's
 * stated reason. Not which rules, not which limits, and not what the portfolio is
 * governed by today. There is no diff on this platform to render: `previous` is
 * nil on publish too, deliberately, because the only registry compliance holds is
 * a replay-filled view that could disagree with the compacted stream.
 *
 * Saying so is the difference between a signatory who knows their signature
 * covers a digest they cannot expand, and one who believes the screen showed them
 * the change. The second is the forgeable-actor problem in a new costume: two
 * real names on the record, neither of whom read the rule.
 */
export function describeVisibility(p: PendingMandateChange): string {
  const rules = renderRuleCount(p)
  const shape =
    rules === null
      ? 'This queue did not even state how many rules the proposed mandate carries.'
      : rules === '0'
        ? 'The proposed mandate carries NO RULES: approving it leaves this portfolio governed by a mandate that constrains nothing. That is legal, and it is not the same as having no mandate — every order will pass every check.'
        : `The proposed mandate carries ${rules} rule${rules === '1' ? '' : 's'}.`
  return (
    `${shape} WHICH rules, and which limits, are NOT on this surface — compliance serves the ` +
    'digest rather than the mandate, and no route exists to fetch one proposal’s mandate. ' +
    'Neither is the mandate this portfolio is governed by today, so there is no diff to read. ' +
    'Your signature will cover the digest below; what it covers can only be confirmed against ' +
    'the proposal as it was submitted, from whoever proposed it.'
  )
}
