package unwind_test

import (
	"math/big"
	"strings"
	"testing"
	"time"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"

	"github.com/eighred/kanz/internal/risk/unwind"
)

var now = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

func breach(vs ...*compliancepb.Violation) *compliancepb.ComplianceBreach {
	return &compliancepb.ComplianceBreach{
		PortfolioId: "fund-alpha", MandateId: "m-1", MandateVersion: 3,
		Result: &compliancepb.ComplianceResult{
			Status: compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH, Violations: vs,
		},
	}
}

func violation(id string, ev map[string]string) *compliancepb.Violation {
	return &compliancepb.Violation{
		RuleId: id, Severity: compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH, Evidence: ev,
	}
}

// THE CONCENTRATION FORM IS THE ONE WORTH PINNING.
//
// Shedding from a bucket shrinks the TOTAL as well, so the weight does not scale
// linearly: the naive f = (w−m)/w solves for a denominator that will not exist
// once the trade is done, and sheds MORE than the mandate requires. Selling more
// than asked is not a conservative error — it is an unrequested trade.
//
// Worked: w = 0.20, m = 0.10.
//
//	correct  f = (0.20−0.10) / (0.20 × 0.90) = 0.5555…
//	naive    f = (0.20−0.10) /  0.20         = 0.5
//
// The correct answer is LARGER here, which is the point: the naive form
// undersheds for concentration and would leave the book still in breach. The
// test asserts the exact rational, so either error fails.
func TestConcentrationReductionAccountsForTheShrinkingTotal(t *testing.T) {
	p := unwind.Decide(breach(violation("conc-1", map[string]string{
		"dimension": "SECTOR", "bucket": "TECH", "observed": "0.20", "limit": "0.10",
	})), now)

	if len(p.Reductions) != 1 {
		t.Fatalf("got %d reductions, want 1 (undecidable: %+v)", len(p.Reductions), p.Undecidable)
	}
	r := p.Reductions[0]
	want := new(big.Rat).Quo(big.NewRat(1, 10), new(big.Rat).Mul(big.NewRat(2, 10), big.NewRat(9, 10)))
	if r.Fraction.Cmp(want) != 0 {
		t.Errorf("fraction = %s, want %s — shedding a bucket shrinks the total too, so the "+
			"naive (w−m)/w is wrong", r.Fraction.RatString(), want.RatString())
	}
	// And it is NOT the naive answer.
	if r.Fraction.Cmp(big.NewRat(1, 2)) == 0 {
		t.Error("fraction is exactly (w−m)/w — the shrinking denominator was ignored")
	}
	if r.Bucket != "TECH" || r.Dimension != "SECTOR" {
		t.Errorf("scope = %s/%s, want SECTOR/TECH", r.Dimension, r.Bucket)
	}
}

// Applying the proposed fraction must actually clear the limit. This is the
// property the formula exists for, checked by simulation rather than by
// restating the algebra: shed f of the bucket and the recomputed weight must land
// on the limit, not near it.
func TestApplyingTheReductionLandsExactlyOnTheLimit(t *testing.T) {
	for _, tc := range []struct{ observed, limit string }{
		{"0.20", "0.10"}, {"0.35", "0.25"}, {"0.99", "0.50"}, {"0.11", "0.10"},
	} {
		t.Run(tc.observed+"->"+tc.limit, func(t *testing.T) {
			p := unwind.Decide(breach(violation("c", map[string]string{
				"bucket": "TECH", "observed": tc.observed, "limit": tc.limit,
			})), now)
			if len(p.Reductions) != 1 {
				t.Fatalf("no reduction: %+v", p.Undecidable)
			}
			f := p.Reductions[0].Fraction

			// Simulate on a unit book: total = 1, bucket gross = observed.
			w, _ := new(big.Rat).SetString(tc.observed)
			limit, _ := new(big.Rat).SetString(tc.limit)
			shed := new(big.Rat).Mul(w, f)                     // x = f·g
			g2 := new(big.Rat).Sub(w, shed)                    // g − x
			total2 := new(big.Rat).Sub(big.NewRat(1, 1), shed) // total − x
			got := new(big.Rat).Quo(g2, total2)

			if got.Cmp(limit) != 0 {
				t.Errorf("after shedding %s of the bucket the weight is %s, want exactly the "+
					"limit %s", f.RatString(), got.RatString(), limit.RatString())
			}
		})
	}
}

// Leverage has a fixed denominator (NAV does not move when gross is shed), so the
// fraction IS linear — a different formula, and using the concentration one here
// would overshed.
func TestLeverageReductionIsLinearInTheObservedRatio(t *testing.T) {
	p := unwind.Decide(breach(violation("lev-1", map[string]string{
		"observed": "4.0", "limit": "3.0", // 4x gross leverage against a 3x cap
	})), now)

	if len(p.Reductions) != 1 {
		t.Fatalf("got %d reductions, want 1 (undecidable: %+v)", len(p.Reductions), p.Undecidable)
	}
	r := p.Reductions[0]
	if r.Fraction.Cmp(big.NewRat(1, 4)) != 0 {
		t.Errorf("fraction = %s, want 1/4 — (L−m)/L with NAV fixed", r.Fraction.RatString())
	}
	if r.Bucket != "" {
		t.Errorf("bucket = %q, want empty: leverage applies to the whole book", r.Bucket)
	}
	// The concentration formula would give (4−3)/(4×(1−3)) — negative — so a
	// single shared formula would be visibly wrong here.
	if r.Fraction.Sign() <= 0 {
		t.Error("fraction is not positive; the bucket formula was applied to a whole-book ratio")
	}
}

// A VIOLATION IT CANNOT SOLVE IS REPORTED, NOT DROPPED.
//
// A proposal that quietly omits a rule reads as "this rule needs nothing", which
// is the opposite of true. Each case names why, because the reasons are different
// problems: missing evidence is a schema/rule gap, while evidence that contradicts
// the verdict is a disagreement inside the input worth looking at.
func TestUnsolvableViolationsAreReportedWithAReason(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ev      map[string]string
		wantSub string
	}{
		{"no evidence at all", map[string]string{}, "observed"},
		{"no limit", map[string]string{"observed": "0.2"}, "limit"},
		{"unparseable observed", map[string]string{"observed": "n/a", "limit": "0.1"}, "observed"},
		{"observed within limit", map[string]string{"observed": "0.05", "limit": "0.10"}, "does not describe a breach"},
		{"limit at 100%", map[string]string{"bucket": "T", "observed": "1.2", "limit": "1.0"}, "at or above 100%"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := unwind.Decide(breach(violation("r-1", tc.ev)), now)
			if len(p.Reductions) != 0 {
				t.Fatalf("proposed a reduction from unusable evidence: %+v", p.Reductions)
			}
			if len(p.Undecidable) != 1 {
				t.Fatalf("got %d undecidable, want 1 — a rule it cannot solve must not vanish", len(p.Undecidable))
			}
			if !strings.Contains(p.Undecidable[0].Reason, tc.wantSub) {
				t.Errorf("reason %q does not mention %q", p.Undecidable[0].Reason, tc.wantSub)
			}
			if p.Actionable() {
				t.Error("Actionable() is true with no reductions")
			}
		})
	}
}

// A WARN IS NOT UNWOUND. A mandate that warns has said this is worth seeing, not
// worth trading on; shedding a position over a warning acts past what the mandate
// asked for.
func TestWarningsProduceNoReduction(t *testing.T) {
	warn := &compliancepb.Violation{
		RuleId: "w-1", Severity: compliancepb.ComplianceStatus_COMPLIANCE_STATUS_WARN,
		Evidence: map[string]string{"bucket": "TECH", "observed": "0.20", "limit": "0.10"},
	}
	p := unwind.Decide(breach(warn), now)
	if len(p.Reductions) != 0 || len(p.Undecidable) != 0 {
		t.Errorf("a WARN produced %d reductions and %d undecidable, want none of either — "+
			"it is not a breach and is not this package's business",
			len(p.Reductions), len(p.Undecidable))
	}

	// NON-VACUITY: the same evidence at BREACH severity does produce one, so the
	// test above is filtering on severity rather than on unusable evidence.
	p = unwind.Decide(breach(violation("b-1", warn.GetEvidence())), now)
	if len(p.Reductions) != 1 {
		t.Fatalf("the same evidence at BREACH produced %d reductions, want 1", len(p.Reductions))
	}
}

// Shedding the whole bucket may still not clear the limit; the proposal is capped
// at 100% rather than asking for a trade nobody can place.
func TestAnImpossibleReductionIsCappedAtTheWholeBucket(t *testing.T) {
	// A bucket at 90% against a 5% cap: even selling all of it leaves the rest of
	// the book too concentrated relative to what remains.
	p := unwind.Decide(breach(violation("c", map[string]string{
		"bucket": "TECH", "observed": "0.99", "limit": "0.005",
	})), now)
	if len(p.Reductions) != 1 {
		t.Fatalf("no reduction: %+v", p.Undecidable)
	}
	if f := p.Reductions[0].Fraction; f.Cmp(big.NewRat(1, 1)) > 0 {
		t.Errorf("fraction = %s, want it capped at 1 — a reduction above 100%% is not a trade",
			f.RatString())
	}
}

// Several violations in one breach each get their own reduction, in a stable
// order so two runs over the same breach produce the same proposal.
func TestMultipleViolationsAreEachAnsweredInAStableOrder(t *testing.T) {
	b := breach(
		violation("z-rule", map[string]string{"bucket": "FIN", "observed": "0.30", "limit": "0.20"}),
		violation("a-rule", map[string]string{"observed": "4.0", "limit": "2.0"}),
		violation("m-rule", map[string]string{"observed": "bad"}),
	)
	p := unwind.Decide(b, now)
	if len(p.Reductions) != 2 || len(p.Undecidable) != 1 {
		t.Fatalf("got %d reductions / %d undecidable, want 2 / 1", len(p.Reductions), len(p.Undecidable))
	}
	if p.Reductions[0].RuleID != "a-rule" || p.Reductions[1].RuleID != "z-rule" {
		t.Errorf("reductions are %s,%s — want rule-id order so the proposal is reproducible",
			p.Reductions[0].RuleID, p.Reductions[1].RuleID)
	}
	if !p.Actionable() {
		t.Error("Actionable() is false with two reductions")
	}
	if p.PortfolioID != "fund-alpha" || p.MandateID != "m-1" || p.MandateVersion != 3 {
		t.Error("the proposal does not carry the portfolio and the mandate version it answers")
	}
	if !p.DecidedAt.Equal(now) {
		t.Error("DecidedAt is not the time it was told")
	}
}

// Every reduction explains itself in words. The proposal is read by a human who
// then decides whether to act, and a bare fraction is not a reason.
func TestEveryReductionCarriesItsReasoning(t *testing.T) {
	p := unwind.Decide(breach(
		violation("c", map[string]string{"bucket": "TECH", "observed": "0.20", "limit": "0.10"}),
		violation("l", map[string]string{"observed": "4.0", "limit": "3.0"}),
	), now)
	for _, r := range p.Reductions {
		if r.Why == "" {
			t.Errorf("reduction for %s carries no explanation", r.RuleID)
		}
		if !strings.Contains(r.Why, "limit") {
			t.Errorf("reduction for %s does not name the limit it is reaching: %q", r.RuleID, r.Why)
		}
	}
}
