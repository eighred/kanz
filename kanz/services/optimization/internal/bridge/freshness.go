package bridge

import (
	"errors"
	"fmt"
	"time"

	"github.com/eighred/kanz/internal/optimization"
)

// THE PROPOSAL'S INPUTS HAVE AN AGE, AND UNTIL #970 NOTHING READ IT.
//
// RebalanceProposal.AsOf is "the knowledge horizon the inputs (covariance,
// prices, holdings) were read at". Materialize mapped, gated and published
// without ever looking at it, so a proposal computed an hour ago became live
// orders exactly like one computed a second ago.
//
// # Why that is capital risk and not tidiness
//
// `Trades` is the MINIMAL TRADE LIST — a DELTA between the holdings read at AsOf
// and the optimizer's targets. Against a book that has since moved, the delta is
// simply the wrong trade: the position may already be where the proposal wanted
// to move it, and the order then moves it past. Three properties compound it:
//
//   - Every emitted child is ORDER_TYPE_MARKET / TIME_IN_FORCE_DAY (ToOrders), so
//     no limit price stands between a stale quantity and the book.
//   - Materialize's compliance re-check mixes instants: the price comes from the
//     caller's map, supplied NOW, and the quantity comes from the holdings at
//     AsOf. The gate passes, and it is answering a question nobody asked.
//   - AsOf is caller-supplied (server.ordersRequest.Proposal) and was unbounded
//     in BOTH directions — a timestamp in the future defeats any age bound.
//
// # Why a clock and not a version, and what replaces it
//
// A timestamp cannot tell "nothing happened for an hour" from "three fills landed
// in the last second", and only the first is safe. The correct check compares the
// STATE the proposal was computed against with the state at execution — the shape
// the OMS already uses for orders (orders.version plus a CAS predicate).
//
// IT CANNOT LAND HERE YET, AND THE REASON IS STRUCTURAL. This service is
// stateless by construction: its own package header says the market inputs
// "arrive in the request, so the service needs no broker or database", and
// /v1/orders receives the entire proposal in the request body. There is nothing
// to compare a version against, because this service never read the holdings.
// Giving it a holdings source is the same wiring #646 names as the retirement
// path for the mandate, and the reader belongs with that work rather than ahead
// of it.
//
// So this is the bound that IS available, and it is the capital fix: it stops a
// proposal whose inputs are old enough that the book has plausibly moved. When
// the version check arrives it becomes defence in depth rather than the primary
// control — see #970 increment 2.

var (
	// ErrProposalStale: the inputs are older than the configured bound.
	//
	// A DISTINCT ERROR FROM THE MANDATE REFUSALS, deliberately. The operator
	// responses differ — a stale proposal is re-run through the optimizer, an
	// infeasible one is a mandate to investigate — and #646's whole lesson is
	// that "refused" and "refused for a different reason" must not arrive at the
	// caller identically.
	ErrProposalStale = errors.New("this proposal's inputs are older than the freshness bound")

	// ErrProposalFuture: AsOf is after now by more than the clock-skew allowance.
	//
	// A HORIZON THAT HAS NOT HAPPENED YET IS NOT A HORIZON. Left unchecked it
	// defeats the age bound entirely: any caller wanting an unbounded proposal
	// need only date it forward. This is the same class of input as the mandate
	// verdict the server already refuses from the body (#409/#646) — an assertion
	// the server cannot verify and must therefore constrain.
	ErrProposalFuture = errors.New("this proposal's inputs are dated in the future")

	// ErrProposalUndated: AsOf is the zero time.
	//
	// The caller stated no horizon at all, which is not the same as stating a
	// recent one. It is refused for the reason MandateUnchecked is: an absent
	// verdict must not read as a clean one.
	ErrProposalUndated = errors.New("this proposal states no knowledge horizon")

	// ErrFreshnessUnbounded: no bound was configured.
	//
	// AN UNSET BOUND IS UNKNOWN, NOT UNLIMITED, and UNKNOWN refuses. This is the
	// platform's "nothing configured and checked-and-fine must never look the
	// same" rule on the path where a proposal becomes capital: a deployment that
	// forgot OPTIMIZATION_PROPOSAL_MAX_AGE must not silently materialize
	// everything, which is exactly the pre-#970 behaviour it would restore.
	ErrFreshnessUnbounded = errors.New("no proposal freshness bound is configured")
)

// ClockSkewAllowance is how far into the future a proposal's AsOf may sit before
// it is refused.
//
// IT IS NOT ZERO, AND THAT IS NOT LAXITY. The optimizer that stamps AsOf and the
// service that reads it are different processes on different hosts; NTP-disciplined
// clocks in a cluster disagree by milliseconds and, during a step correction, by
// more. A zero allowance would refuse perfectly good proposals for a reason no
// operator could act on, and an alert nobody can clear is one everybody learns to
// scroll past.
//
// IT IS SMALL ENOUGH NOT TO BE A BOUND. Five seconds cannot hide a book moving:
// the failure this guards against is a proposal dated hours or days forward to
// escape the age check, not one dated 200ms forward by clock drift.
const ClockSkewAllowance = 5 * time.Second

// Freshness is the bound on how old a proposal's inputs may be when it becomes
// orders.
//
// IT IS A PARAMETER RATHER THAN A PACKAGE-LEVEL DEFAULT so every caller must
// answer the question. A default here would be a number this package invented for
// a deployment it cannot see, and the zero value is the refusal rather than a
// silent "unlimited" — see ErrFreshnessUnbounded.
type Freshness struct {
	// MaxAge is how old AsOf may be. Zero or negative ⇒ unconfigured ⇒ refuse.
	MaxAge time.Duration
	// Now is the injectable clock. Nil ⇒ time.Now.
	Now func() time.Time
}

// now resolves the clock.
func (f Freshness) now() time.Time {
	if f.Now == nil {
		return time.Now()
	}
	return f.Now()
}

// Check reports whether a proposal's inputs are fresh enough to become orders.
//
// It returns the AGE alongside the error so the caller can report how stale the
// refusal was — "refused, and it was four hours old" is an operator's answer;
// "refused" alone sends them back to the logs.
func (f Freshness) Check(p optimization.RebalanceProposal) (age time.Duration, err error) {
	if f.MaxAge <= 0 {
		return 0, fmt.Errorf("%w (set OPTIMIZATION_PROPOSAL_MAX_AGE)", ErrFreshnessUnbounded)
	}
	if p.AsOf.IsZero() {
		return 0, ErrProposalUndated
	}
	now := f.now()
	age = now.Sub(p.AsOf)
	if age < -ClockSkewAllowance {
		return age, fmt.Errorf("%w: as_of %s is %s ahead of now, beyond the %s clock-skew allowance",
			ErrProposalFuture, p.AsOf.UTC().Format(time.RFC3339), (-age).Truncate(time.Millisecond), ClockSkewAllowance)
	}
	if age > f.MaxAge {
		return age, fmt.Errorf("%w: inputs are %s old, bound is %s",
			ErrProposalStale, age.Truncate(time.Second), f.MaxAge)
	}
	return age, nil
}

// RefusalCode renders a freshness refusal as a stable, closed-vocabulary code, or
// "" for an error that is not one.
//
// FOUR ERRORS, THREE CODES, AND THE SPLIT IS DELIBERATE. Stale and future-dated
// are both "this proposal does not describe the current book" and an operator
// answers them the same way: re-run the optimizer. An UNCONFIGURED BOUND is a
// different problem with a different owner — nothing is wrong with the proposal,
// the deployment is missing a setting — and reporting it as STALE_PROPOSAL would
// send somebody to re-run an optimizer that will be refused again forever.
//
// A CODE RATHER THAN THE SENTENCE, for the reason pkg/auth's deny.code exists:
// "how often did staleness refuse" is a question about a control's health, and
// answering it by grepping English out of an error message is how a reworded
// sentence silently empties a dashboard.
func RefusalCode(err error) string {
	switch {
	case errors.Is(err, ErrProposalStale), errors.Is(err, ErrProposalFuture):
		return "STALE_PROPOSAL"
	case errors.Is(err, ErrProposalUndated):
		return "UNDATED_PROPOSAL"
	case errors.Is(err, ErrFreshnessUnbounded):
		return "FRESHNESS_UNCONFIGURED"
	default:
		return ""
	}
}

// IsFreshnessRefusal reports whether err is one of this file's refusals.
//
// It exists so a caller can render the four as one HTTP status and one reason
// class without matching on strings — the refusal CLASS is what an operator
// dashboards on, and answering "how often did staleness refuse" by grepping
// English out of an error message is how a reworded sentence silently empties a
// panel (the argument pkg/auth's deny.code makes).
func IsFreshnessRefusal(err error) bool { return RefusalCode(err) != "" }
