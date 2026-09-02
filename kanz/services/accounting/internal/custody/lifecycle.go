package custody

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// BreakStatus is where a break has got to in the investigation. It mirrors
// accounting.v1.BreakStatus.
type BreakStatus int

const (
	// BreakStatusUnspecified is the zero value. A break is never legitimately in
	// it — it exists so a break that was never given a status is detectable
	// rather than reading as OPEN, which would be a working item nobody was ever
	// told about.
	BreakStatusUnspecified BreakStatus = iota
	// BreakOpen: detected, unassigned. Every break is born here.
	BreakOpen
	// BreakAssigned: an operator owns the investigation.
	BreakAssigned
	// BreakExplained: the cause is known and recorded, and the break is expected
	// to clear on its own — a trade that settles tomorrow, a fee the custodian
	// books on a different day.
	BreakExplained
	// BreakResolved: the discrepancy is gone. Terminal.
	BreakResolved
)

func (s BreakStatus) String() string {
	switch s {
	case BreakOpen:
		return "open"
	case BreakAssigned:
		return "assigned"
	case BreakExplained:
		return "explained"
	case BreakResolved:
		return "resolved"
	default:
		return "unspecified"
	}
}

// BreakStatuses returns every legitimate status, in wire order — the same
// derived-not-copied stance Outcomes() takes, and for the same reason: the open
// breaks gauge is pre-seeded from it so an unpopulated status is a zero rather
// than an absent series.
func BreakStatuses() []BreakStatus {
	return []BreakStatus{BreakOpen, BreakAssigned, BreakExplained, BreakResolved}
}

// Outstanding reports whether a break is still work. It is the predicate behind
// the open-breaks gauge and behind the aging alert.
//
// EXPLAINED IS OUTSTANDING, and that is the load-bearing decision in this file.
// An explanation is a claim about the future — "this clears when tomorrow's
// settlement lands" — not evidence that it has. A break still present a week
// after being explained is a WORSE signal than an unexplained one, because
// somebody looked at it and was wrong, and dropping it out of the gauge at
// EXPLAINED would let a wrong explanation silence the control indefinitely. Only
// RESOLVED, which requires the difference to actually be gone, stops counting.
func (s BreakStatus) Outstanding() bool {
	return s == BreakOpen || s == BreakAssigned || s == BreakExplained
}

// ErrIllegalTransition is returned when a break is moved between states the
// lifecycle does not allow. It is the same shape, and the same name, as
// posttrade.ErrIllegalTransition — this aggregate is modelled on that one rather
// than invented.
var ErrIllegalTransition = errors.New("custody: illegal break transition")

// Assign gives an OPEN or ASSIGNED break an owner.
//
// RE-ASSIGNMENT IS LEGAL, a handover between operators is ordinary, and refusing
// it would make the lifecycle something people work around rather than through.
// An EXPLAINED break may also be re-assigned: an explanation that turned out to
// be wrong needs a new owner, and forcing it to be resolved first would record a
// resolution that never happened.
func (b *Break) Assign(assignee string, now time.Time) error {
	if strings.TrimSpace(assignee) == "" {
		return errors.New("custody: assign needs an assignee")
	}
	switch b.Status {
	case BreakOpen, BreakAssigned, BreakExplained:
		b.Assignee = assignee
		return b.moveTo(BreakAssigned, now)
	default:
		return fmt.Errorf("%w: %s -> assigned", ErrIllegalTransition, b.Status)
	}
}

// Explain records the cause of a non-terminal break.
//
// AN EXPLANATION IS MANDATORY AND NON-EMPTY. "Explained" with no explanation is
// the same silence the whole control exists to abolish, one level in: it removes
// the break from an operator's attention while recording nothing anyone can
// audit or disagree with later.
func (b *Break) Explain(explanation string, now time.Time) error {
	if strings.TrimSpace(explanation) == "" {
		return errors.New("custody: explain needs an explanation")
	}
	switch b.Status {
	case BreakOpen, BreakAssigned, BreakExplained:
		b.Explanation = explanation
		return b.moveTo(BreakExplained, now)
	default:
		return fmt.Errorf("%w: %s -> explained", ErrIllegalTransition, b.Status)
	}
}

// THERE IS DELIBERATELY NO Resolve METHOD HERE, and its absence is the design.
//
// A BREAK IS RESOLVED WHEN THE BOOK AND THE CUSTODIAN AGREE — not when somebody
// is finished looking at it. The only thing that can establish agreement is
// running the comparison, so the RESOLVED transition belongs to the run and lives
// in Store.UpsertBreaks: a stored outstanding break that the latest run no longer
// detects is resolved, automatically, and that is the single path into the
// terminal state.
//
// AN OPERATOR-FACING Resolve WOULD BE A WAY TO SILENCE THE CONTROL. It would let
// a break be closed while the difference is still there, which leaves the number
// wrong and the queue looking clean — the failure that makes a reconciliation
// process decorative. It cannot be made safe by asking the store whether the
// break is "still detected" either: the store REMOVES a break from the
// outstanding set the moment a run stops finding it, so any break an operator
// could still see is by construction one the latest run DID find. The check would
// always refuse, and a route that always refuses is a dead route pretending to be
// a control.
//
// What an operator has instead is Explain — "I know why this is here and I expect
// it to clear" — which keeps the break ageing until it actually does. Closing one
// sooner than the next cycle needs an operator-triggered RE-RECONCILIATION rather
// than a status write, which is a real feature with a real question behind it
// (what happens when no statement exists for the date) and is tracked separately.

// moveTo applies a status change and stamps when it happened.
//
// THE TIMESTAMP MOVES ONLY WHEN THE STATUS DOES. A re-assignment to the same
// owner, or a second identical explanation, must not restamp — an operator
// reading "changed 2 minutes ago" needs it to mean something changed, or the
// field is noise and the queue cannot be triaged by it.
func (b *Break) moveTo(to BreakStatus, now time.Time) error {
	if b.Status != to {
		b.Status = to
		b.StatusChangedAt = now
	}
	return nil
}
