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

// ErrResolveWithoutAgreement is returned when a break is resolved while the book
// and the custodian still disagree.
var ErrResolveWithoutAgreement = errors.New("custody: cannot resolve a break the latest run still detects")

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

// Resolve moves a non-terminal break to the terminal RESOLVED.
//
// stillDetected is whether the MOST RECENT run still finds this discrepancy, and
// passing it is not a formality. A break is resolved when the book and the
// custodian agree — not when somebody is finished looking at it. Allowing a
// resolution while the difference is still there would let the control be
// silenced by hand, which is precisely the failure that makes a reconciliation
// process decorative: the number stays wrong and the queue looks clean.
//
// The legitimate path is that the underlying cause is corrected (the missing fill
// is folded, the custodian restates), the next run no longer detects it, and it
// is resolved then. Sweeper does exactly that automatically; this method is the
// operator's route to the same place.
func (b *Break) Resolve(stillDetected bool, now time.Time) error {
	if b.Status == BreakResolved {
		return fmt.Errorf("%w: %s -> resolved", ErrIllegalTransition, b.Status)
	}
	if stillDetected {
		return fmt.Errorf("%w: %s %s", ErrResolveWithoutAgreement, b.Kind, b.Key)
	}
	return b.moveTo(BreakResolved, now)
}

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
