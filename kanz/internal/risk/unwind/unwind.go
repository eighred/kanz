// Package unwind decides what a breached portfolio WOULD have to shed. It never
// places an order, and it is built so that it cannot (M4, #74).
//
// # Decides, does not execute
//
// That is the whole scope, and the constraint is structural rather than a
// promise. This package imports no venue adapter, no order schema and no
// producer; test/arch asserts it, because "we did not call the exchange" is the
// kind of property that holds until somebody adds one convenient call.
//
// Auto-deleveraging that ACTS is long-term work. What is missing before then is
// not the arithmetic below — it is an operator's judgement about whether firing
// orders off the back of a market-move breach is safe at all. A breach is often
// a price moving, and a book that liquidates into a falling market is how a risk
// control becomes the accelerant. So this half is built first and the trigger is
// left to a human.
//
// # The reduction is derived, not guessed
//
// A ComplianceBreach carries the engine's own evidence — for concentration
// {dimension, bucket, observed, limit} and for leverage {observed, limit}, where
// observed and limit are exact decimal strings. Both reductions fall out of those
// two numbers alone, with no access to the book:
//
//	concentration   observed w = bucketGross/totalGross, cap m
//	                shedding x from the bucket moves BOTH numerator and
//	                denominator, so w is not simply scaled:
//	                  (g−x)/(total−x) = m  ⇒  f = (w−m) / (w·(1−m))
//	                where f = x/bucketGross, the fraction of the BUCKET to shed.
//
//	leverage        observed L = gross/nav, cap m; nav is unchanged by shedding
//	                gross, so  f = (L−m)/L,  the fraction of the BOOK to shed.
//
// The concentration form is the one worth stating: reducing a bucket shrinks the
// total too, so the naive f = (w−m)/w sheds MORE than necessary — it solves for a
// denominator that will not exist once the trade is done. Selling more than a
// mandate requires is not a conservative error; it is an unrequested trade.
//
// # A violation it cannot solve says so
//
// Anything without both numbers, or whose numbers do not describe a breach, is
// returned as Undecidable with the reason. It is NOT silently dropped and NOT
// given a default reduction: a proposal that quietly omits a rule reads as "this
// rule needs nothing", which is the opposite of true.
package unwind

import (
	"fmt"
	"math/big"
	"sort"
	"time"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
)

// Evidence keys the compliance engine writes (internal/compliance/rules.go).
// Read as constants so a rename there fails here rather than silently producing
// Undecidable for every violation.
const (
	evidenceObserved  = "observed"
	evidenceLimit     = "limit"
	evidenceBucket    = "bucket"
	evidenceDimension = "dimension"
)

// Reduction is how much must come off, as a FRACTION rather than a quantity.
//
// A fraction because this package does not read the book: it is derived from the
// engine's ratios, and turning it into instrument quantities needs positions,
// marks and lot sizes — every one of which is a place to be wrong about a number
// that would become an order. Whoever executes owns that conversion, and owns
// being right about it.
type Reduction struct {
	// Scope is what the fraction applies to: the named bucket, or the whole book
	// when Bucket is empty.
	Dimension string
	Bucket    string
	// Fraction of that scope's GROSS exposure to shed, in (0, 1].
	Fraction *big.Rat
	// RuleID is the violated rule this reduction answers.
	RuleID string
	// Why is the arithmetic, in words, for the operator reading the proposal.
	Why string
}

// Undecidable is a violation this package will not propose a reduction for.
type Undecidable struct {
	RuleID string
	Reason string
}

// Proposal is what WOULD be done. It is not an order and carries nothing that
// could become one without a human deciding to.
type Proposal struct {
	PortfolioID    string
	MandateID      string
	MandateVersion uint64
	Reductions     []Reduction
	Undecidable    []Undecidable
	DecidedAt      time.Time
}

// Actionable reports whether anything was derivable. False with a non-empty
// Undecidable is a real answer — "a breach nobody can automatically unwind" — and
// the caller must be able to tell it from "no breach".
func (p Proposal) Actionable() bool { return len(p.Reductions) > 0 }

// Decide reads a breach and returns what would clear it.
//
// It has no error return on purpose: a violation it cannot solve is DATA in the
// proposal (Undecidable), not a failure of the call. An error would let a caller
// drop the whole proposal on one unparseable rule and lose the others with it.
func Decide(b *compliancepb.ComplianceBreach, now time.Time) Proposal {
	p := Proposal{
		PortfolioID:    b.GetPortfolioId(),
		MandateID:      b.GetMandateId(),
		MandateVersion: b.GetMandateVersion(),
		DecidedAt:      now,
	}

	for _, v := range b.GetResult().GetViolations() {
		// WARN is not unwound. A mandate that warns has said this is worth seeing,
		// not worth trading on, and shedding a position over a warning would act
		// past what the mandate asked for.
		if v.GetSeverity() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
			continue
		}
		r, u := reductionFor(v)
		if u != nil {
			p.Undecidable = append(p.Undecidable, *u)
			continue
		}
		p.Reductions = append(p.Reductions, *r)
	}

	// Deterministic order so two runs over the same breach produce the same
	// proposal — an operator comparing them should see no diff that is not a real
	// change, and a test should not depend on map iteration.
	sort.Slice(p.Reductions, func(i, j int) bool { return p.Reductions[i].RuleID < p.Reductions[j].RuleID })
	sort.Slice(p.Undecidable, func(i, j int) bool { return p.Undecidable[i].RuleID < p.Undecidable[j].RuleID })
	return p
}

// reductionFor solves one violation. Exactly one of the two returns is non-nil.
func reductionFor(v *compliancepb.Violation) (*Reduction, *Undecidable) {
	ev := v.GetEvidence()
	observed, ok := ratFrom(ev[evidenceObserved])
	if !ok {
		return nil, &Undecidable{v.GetRuleId(),
			fmt.Sprintf("no usable %q in the rule's evidence, so there is no measured value to reduce from", evidenceObserved)}
	}
	limit, ok := ratFrom(ev[evidenceLimit])
	if !ok {
		return nil, &Undecidable{v.GetRuleId(),
			fmt.Sprintf("no usable %q in the rule's evidence, so there is no target to reduce to", evidenceLimit)}
	}
	if observed.Cmp(limit) <= 0 {
		// The engine said BREACH but its own numbers do not show one. Refused
		// rather than reconciled: proposing a trade to fix a breach that the
		// evidence does not describe is acting on a disagreement inside the input.
		return nil, &Undecidable{v.GetRuleId(),
			fmt.Sprintf("evidence does not describe a breach (observed %s is within limit %s) — "+
				"nothing to unwind, and the disagreement with the BREACH verdict is worth a look",
				ratStr(observed), ratStr(limit))}
	}
	if limit.Sign() < 0 {
		return nil, &Undecidable{v.GetRuleId(), "limit is negative, which no reduction can satisfy"}
	}

	bucket, dimension := ev[evidenceBucket], ev[evidenceDimension]
	if bucket == "" {
		// Whole-book ratio (leverage): the denominator (NAV) does not move when
		// gross is shed, so the fraction is linear in the observed ratio.
		f := new(big.Rat).Sub(observed, limit)
		f.Quo(f, observed)
		return &Reduction{
			Fraction: f, RuleID: v.GetRuleId(),
			Why: fmt.Sprintf("observed %s against a limit of %s; the denominator does not move when "+
				"gross is shed, so shedding %s of the book's gross reaches the limit",
				ratStr(observed), ratStr(limit), ratStr(f)),
		}, nil
	}

	// Bucket ratio (concentration): shedding from the bucket shrinks the TOTAL
	// too, so f = (w−m)/(w·(1−m)) rather than (w−m)/w. See the package doc — the
	// naive form oversheds, and overshedding is an unrequested trade.
	oneMinusLimit := new(big.Rat).Sub(big.NewRat(1, 1), limit)
	if oneMinusLimit.Sign() <= 0 {
		return nil, &Undecidable{v.GetRuleId(),
			fmt.Sprintf("limit %s is at or above 100%%, so no bucket reduction can breach it",
				ratStr(limit))}
	}
	f := new(big.Rat).Sub(observed, limit)
	f.Quo(f, new(big.Rat).Mul(observed, oneMinusLimit))
	if f.Cmp(big.NewRat(1, 1)) > 0 {
		// Shedding the entire bucket still would not clear the limit. Capped at
		// the whole bucket and reported as such rather than proposing >100%, which
		// is not a trade anyone can place.
		f = big.NewRat(1, 1)
	}
	return &Reduction{
		Dimension: dimension, Bucket: bucket, Fraction: f, RuleID: v.GetRuleId(),
		Why: fmt.Sprintf("bucket %q weighs %s against a limit of %s; shedding it shrinks the total "+
			"as well, so %s of the bucket's gross reaches the limit",
			bucket, ratStr(observed), ratStr(limit), ratStr(f)),
	}, nil
}

// ratFrom parses one evidence value. The engine writes exact decimal strings, so
// this is exact — a float here would put rounding inside a number that decides
// how much of a book to sell.
func ratFrom(s string) (*big.Rat, bool) {
	if s == "" {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, false
	}
	return r, true
}

// ratStr renders a rational for an operator, trimmed.
func ratStr(r *big.Rat) string {
	s := r.FloatString(6)
	for len(s) > 0 && s[len(s)-1] == '0' {
		s = s[:len(s)-1]
	}
	if len(s) > 0 && s[len(s)-1] == '.' {
		s = s[:len(s)-1]
	}
	if s == "" {
		s = "0"
	}
	return s
}
