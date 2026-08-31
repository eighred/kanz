package algo

import (
	"errors"
	"fmt"
	"math/big"
)

// POV — percent of volume (#869).
//
// # What it is, and what it bounds
//
// "Never be more than n% of prints." Without it there is no way to bound
// self-signalling on a thin instrument, where a fixed-size slice IS the print. On
// a mid-cap or a thin crypto pair that is the difference between working an order
// and advertising it — and it is a bound MaxSlice cannot express, because a
// perfectly small child is still the whole tape when the tape is small.
//
// So POV works the parent at a CONSTANT participation in every interval, and
// refuses outright when the parent cannot be worked inside the cap. It never
// trims and it never spills: a schedule that quietly exceeded the cap for one
// slice is the only outcome a participation control must not have.
//
// # THE CAP IS REQUIRED, AND THAT IS WHAT MAKES POV POV
//
// A POV plan with no Plan.MaxParticipation is REFUSED (ErrNoParticipationCap)
// rather than worked uncapped. An uncapped POV is not a conservative POV — it is
// VWAP with a different label, and this platform has already ruled on what a
// mislabelled execution costs (#868: the fills arrive and every downstream reader
// is looking at the name of an algorithm that never ran).
//
// # WHAT POV AND VWAP SHARE, STATED RATHER THAN HIDDEN
//
// When the cap is not the binding constraint, POV's allocation IS VWAP's, slice
// for slice. That is a theorem and not a coincidence: participating at a constant
// rate ρ in every interval means working ρ·vᵢ in interval i, which sums to ρ·V, so
// conserving the parent forces ρ = Total/V — which is proportional allocation.
// The two algorithms differ in what they REQUIRE and REFUSE, not in the shape they
// produce against a forecast:
//
//	VWAP  needs a curve, has no cap, and participates at whatever rate falls out.
//	POV   needs a curve AND a cap, and refuses the whole order when the rate that
//	      would complete it exceeds that cap.
//
// Writing that down is the point. Two algorithms with the same arithmetic and
// different names would be a mislabelling risk if the label claimed a different
// EXECUTION; here it claims a different ADMISSION, and the label is exactly true.
//
// # WHAT THIS POV IS NOT, AND WHY IT CANNOT BE HERE
//
// A desk's POV follows the TAPE: it looks at what has actually printed since the
// last child and sizes the next one against that, so a quiet hour slows it down
// and a busy one speeds it up. THAT ALGORITHM CANNOT BE WRITTEN IN THIS PACKAGE,
// and the reason is structural rather than a matter of effort.
//
// A schedule here is DERIVED, never held: two pods holding the same order must
// derive the same children forever, which is what makes a parent survive a restart
// with no cursor to lose (#435, and test/arch/schedule_is_derived_test.go — whose
// own doc names POV as the algorithm most tempted toward state). A tape-following
// POV would re-size slice i on every tick as prints arrive, so the quantity of a
// child ALREADY SENT would change between two derivations — and
// services/oms/internal/order.authorizeChild admits a child only if its quantity
// is EXACTLY what the parent's schedule derives. The reactive half is a SEND-TIME
// control over a live view, and it belongs beside the driver, not inside a pure
// function of the order.
//
// So what ships is the half that is decidable in advance: the cap binds against
// the FORECAST. It is a real control — an order the forecast says cannot be worked
// inside the cap is refused before it exists — and it is not the whole one: if the
// realised tape comes in thinner than the profile, the children were sized for
// volume that did not arrive. Nothing here can see that, and nothing here pretends
// to.
const NamePOV Name = "POV"

var (
	// ErrNoParticipationCap refuses a POV plan that carries no cap.
	//
	// NOT DEFAULTED TO UNCAPPED, and not defaulted to a number invented here.
	// "Never more than n% of volume" is a mandate-level decision about a fund, and
	// a value chosen in this file would either refuse orders a desk permits or
	// permit ones it does not — while looking, from every screen, like a control
	// that was configured.
	ErrNoParticipationCap = errors.New("algo: POV requires a participation cap and will not work an order without one")

	// ErrParticipationCapExceeded refuses a parent that cannot be worked inside
	// its cap.
	//
	// THE REFUSAL IS THE CONTROL. The alternatives are to work part of the parent
	// (leaving a remainder no child will ever carry, so the order never completes)
	// or to exceed the cap on some slices (which is the one thing a participation
	// limit exists to prevent). Both look like success from every signal this
	// platform has, which is why neither is available here.
	ErrParticipationCapExceeded = errors.New("algo: the parent cannot be worked inside its participation cap")
)

// povAlgo is POV's registration.
type povAlgo struct{}

// Name is NamePOV.
func (povAlgo) Name() Name { return NamePOV }

// Schedule derives the children. ParentState is not read, for the reason VWAP's
// Schedule gives — and, for POV, for the stronger one in this file's doc: an
// algorithm that sized the next child from what the last one did would not be
// derivable from the order at all.
func (povAlgo) Schedule(p Plan, _ ParentState, mkt MarketView) ([]Slice, error) {
	return pov(p, mkt)
}

// pov is the arithmetic, unexported for the reason vwap is.
//
// THE CAP IS CHECKED BEFORE THE MARKET IS ASKED ANYTHING. A plan with no cap, or
// with one outside (0, 1], is the operator's own error and they can fix it from
// the command; telling them instead that no volume profile exists would send them
// to chase a market-data problem they do not have. It is the ordering
// TestAlgoRegistry_TheNameIsCheckedBeforeThePlan already argues for one layer up.
func pov(p Plan, mkt MarketView) ([]Slice, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	// A LOCAL, NOT A PACKAGE-LEVEL var. test/arch/schedule_is_derived_test.go
	// refuses package-level mutable state in this package outright, and a *big.Rat
	// bound at package scope is exactly that: any caller could Set it, and every
	// schedule derived afterwards would be capped against a different number.
	oneWholeTape := new(big.Rat).SetInt64(1)

	capRate := p.MaxParticipation
	if capRate == nil {
		return nil, fmt.Errorf("%w: set a participation cap, or work this order as %s, which "+
			"does not claim one", ErrNoParticipationCap, NameVWAP)
	}
	if capRate.Sign() <= 0 || capRate.Cmp(oneWholeTape) > 0 {
		return nil, fmt.Errorf("%w: a participation cap of %s is not a cap — it must be greater "+
			"than 0 and at most 1, or it permits either nothing or more than everything that "+
			"trades; work this order as %s if no cap is wanted",
			ErrNoParticipationCap, capRate.RatString(), NameVWAP)
	}

	vols, total, err := expectedVolumes(p, mkt)
	if err != nil {
		return nil, err
	}

	// THE FEASIBILITY TEST, STATED AS A QUANTITY RATHER THAN AS A RATE. Working
	// the parent at all means participating at Total/V across the window, so the
	// cap is honoured if and only if Total <= cap·V. Comparing quantities keeps it
	// one exact rational comparison with no division, and lets the refusal name
	// the largest parent this window and this cap could have worked — which is
	// what the desk actually has to decide about.
	workable := new(big.Rat).Mul(capRate, total)
	if p.Total.Cmp(workable) > 0 {
		rate := new(big.Rat).Quo(p.Total, total)
		return nil, fmt.Errorf("%w: working %s over this window would be %s of the %s expected to "+
			"trade in it, against a cap of %s; at that cap this window can work at most %s — "+
			"lengthen the window, reduce the parent, or raise the cap deliberately",
			ErrParticipationCapExceeded, p.Total.FloatString(8), rate.FloatString(6),
			total.FloatString(8), capRate.FloatString(6), workable.FloatString(8))
	}

	out, err := allocateProportional(p, vols, total)
	if err != nil {
		return nil, err
	}

	// THE POSTCONDITION, CHECKED EVEN THOUGH THE TEST ABOVE MAKES IT UNREACHABLE.
	// Proportional allocation participates uniformly, so Total <= cap·V already
	// implies qᵢ <= cap·vᵢ for every i — today. It is checked anyway for the reason
	// volprofile checks a session's total before dividing by it: the alternative,
	// if the allocation ever stops being proportional, is a participation control
	// that is silently exceeded on one slice while every other signal stays green.
	// This is the assertion that fires, and it fires here rather than in a test.
	for _, s := range out {
		limit := new(big.Rat).Mul(capRate, vols[s.Index])
		if s.Quantity.Cmp(limit) > 0 {
			return nil, fmt.Errorf("%w: slice %d is %s against %s expected to trade in its "+
				"interval, which is more than the cap of %s permits (%s)",
				ErrParticipationCapExceeded, s.Index, s.Quantity.FloatString(8),
				vols[s.Index].FloatString(8), capRate.FloatString(6), limit.FloatString(8))
		}
	}
	return out, nil
}
