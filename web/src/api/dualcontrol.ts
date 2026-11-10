// The browser half of internal/dualcontrol's shared vocabulary (#558, #575).
//
// Two dual-control queues now exist in this app — the ORDER approvals screen and
// the PRICING OVERRIDE screen — and on the server the same pair diverged on
// casing the day they both existed: one said "pending", the other "PENDING". #575
// moved the spelling into internal/dualcontrol beside the rule so a third act
// could not invent a third. This file is that decision, one layer out: the
// browser is now the second place two acts meet, and a second copy of the
// comparison is how the two screens start disagreeing about who a person is.
//
// # WHAT IS SHARED HERE IS THE SPELLING AND THE PERSON-COMPARISON, NOT THE SET
//
// Do NOT add a classify() here that accepts all three states. internal/dualcontrol
// is explicit that the sets differ per act and that the difference is a property
// of how each act reports rather than an omission to repair:
//
//   - the OMS queue has NO "lapsed" — it excludes expired work outright, and #547
//     answers expiry with a terminal ORDER_REJECTED FACT.
//   - datamaster's override queue has NO "refused" — an override is approved over
//     HTTP, so a refusal comes back on the same request as a 403/409 naming the
//     rule and never sits on a queue to be discovered.
//
// A shared classify() would therefore hand each screen a state its own act can
// never reach, and the screen would carry rendering for a condition that cannot
// occur while looking like it covered everything. Each act declares its own
// closed reading over the subset it can reach, denying by default.

/** StatePending: awaiting a second signature, and nobody has been turned away. */
export const StatePending = 'pending'

/**
 * StateRefused: STILL awaiting a second signature; the last person who tried was
 * refused. It NEVER means finished. OMS only — see the note above.
 */
export const StateRefused = 'refused'

/**
 * StateLapsed: nobody signed it before it expired, so it is no longer actionable
 * and is listed rather than erased (#563). datamaster only.
 */
export const StateLapsed = 'lapsed'

/**
 * samePerson compares two subjects the way internal/dualcontrol does.
 *
 * CASE- AND SPACE-INSENSITIVE, because that is the comparison the server makes:
 * "Alice@kanz" approving "alice@kanz" is one person, and a case-sensitive
 * comparison would call it two. Matching the server's rule here is what makes a
 * withheld button agree with the refusal that would otherwise follow — a stricter
 * client comparison would offer a button that always fails, and a looser one
 * would hide a button that would have worked.
 *
 * IT IS NOT A CONTROL, on either surface. The OMS and datamaster each refuse
 * self-approval whatever this returns; the only thing it buys is that the
 * proposer is not invited to discover the rule by pressing a button.
 */
export function samePerson(a: string | undefined, b: string | undefined): boolean {
  if (!a || !b) return false
  return a.trim().toLowerCase() === b.trim().toLowerCase()
}
