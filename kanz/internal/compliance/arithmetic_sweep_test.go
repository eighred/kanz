package compliance

import (
	"context"
	"math"
	"testing"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	compliancepb "github.com/kanz-eng/kanz-schemas-go/compliance/v1"
)

// sweepBook is a portfolio holding $100,000 of AAPL against a $100,000 NAV,
// so a correctly-valued order in the same instrument is large relative to it
// and any value-based cap has something to bite on. NAV is set deliberately
// (the first draft of this sweep left it nil): LeverageRule fails closed on a
// nil/non-positive NAV (rules.go), which would make gross_leverage refuse
// EVERY case regardless of the arithmetic under test and give a false sense
// that the rule had been exercised.
func sweepBook() *Book {
	return &Book{
		PortfolioID:  "p1",
		BaseCurrency: "USD",
		NAV:          &commonpb.Money{Amount: dec(100000, 0), CurrencyCode: "USD"},
		Positions: []Position{{
			InstrumentID: "AAPL",
			Quantity:     dec(1000, 0),
			MarketValue:  &commonpb.Money{Amount: dec(100000, 0), CurrencyCode: "USD"},
		}},
	}
}

// sweepClassifier resolves AAPL to an issuer of the same name. Only
// IssuerExclusionRule needs a classifier at all (rules.go: bucketKey resolves
// DIMENSION_INSTRUMENT straight off Position.InstrumentID, and CurrencyRule
// reads Position.MarketValue's currency code directly — neither touches the
// classifier), but issuer identity is resolved exclusively through
// classify(c, pos).Issuer, so a nil classifier would make issuer_exclusion
// permanently unbreachable — a gap in the test harness having nothing to do
// with the arithmetic under test — rather than exercising the rule at all.
var sweepClassifier = StaticClassifier{"AAPL": {Issuer: "AAPL"}}

// EVERY rule type the engine implements. Restriction, issuer-exclusion and
// currency key off IDENTITY rather than magnitude, so a wrong quantity should
// not be able to move them — but "should not" is the assumption this test
// exists to stop relying on, which is why all five are swept and not just the
// two value-based ones.
func sweepRules() map[string]*compliancepb.Rule {
	return map[string]*compliancepb.Rule{
		"concentration": {
			RuleId: "c1", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
			Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
				Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT,
				MaxWeight: dec(10, -2), // 10%
			}},
		},
		"gross_leverage": {
			RuleId: "l1", Type: compliancepb.RuleType_RULE_TYPE_GROSS_LEVERAGE,
			Params: &compliancepb.Rule_LeverageCap{LeverageCap: &compliancepb.LeverageCap{
				// 0.5x, well BELOW the book's own baseline (existing $100,000
				// position / $100,000 NAV = 1.0x). The baseline alone already
				// breaches, so every swept order — genuinely huge (moves gross
				// far past 0.5x) or genuinely negligible at a deeply negative
				// exponent (leaves gross at ~1.0x, still past 0.5x) — must
				// still breach if correctly computed. A cap that only breaches
				// for the large-delta cases would let the negligible-delta
				// cases pass on their own merits and make an admission there
				// look like a false positive instead of the sign it is.
				MaxGrossLeverage: dec(5, -1),
			}},
		},
		"restriction": {
			RuleId: "r1", Type: compliancepb.RuleType_RULE_TYPE_RESTRICTION,
			Params: &compliancepb.Rule_Restriction{Restriction: &compliancepb.RestrictionList{
				Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT,
				Values:    []string{"AAPL"},
				Mode:      compliancepb.RestrictionMode_RESTRICTION_MODE_DENY,
			}},
		},
		"issuer_exclusion": {
			RuleId: "e1", Type: compliancepb.RuleType_RULE_TYPE_ISSUER_EXCLUSION,
			Params: &compliancepb.Rule_IssuerExclusion{IssuerExclusion: &compliancepb.IssuerExclusion{
				IssuerIds: []string{"AAPL"},
			}},
		},
		"currency": {
			RuleId: "cc1", Type: compliancepb.RuleType_RULE_TYPE_CURRENCY,
			Params: &compliancepb.Rule_CurrencyRestriction{CurrencyRestriction: &compliancepb.CurrencyRestriction{
				AllowedCurrencies: []string{"JPY"}, // USD is NOT allowed
			}},
		},
	}
}

// TestArithmeticSweep_NoWrapAdmitsAnOrder drives every rule type against a
// wide range of quantity exponents and coefficients, and asserts that NOTHING
// is admitted.
//
// The board recorded this hole as "not demonstrated exploitable via
// CONCENTRATION" on the strength of a 738-case sweep against that one rule --
// whose numerator and denominator move together, which is exactly why it
// masked the defect. That was one negative result about one of five rules.
// This is the whole rule set, and it is a permanent test rather than a
// one-off script, so the claim stops being an argument.
//
// Run against the pre-fix addDecimal (commit 0450d62's single-return-value,
// unclamped pow10 version), this sweep DOES fail, and not narrowly:
// concentration, restriction, issuer_exclusion and currency each admit 2 of
// 132 cases (the engineered exact-zero-collision coefficients below), and
// gross_leverage admits 88 of 132 — the majority of the matrix, because most
// swept exponent/coefficient pairs land on a wrapped notional under its cap
// even without being engineered to. The board's finding was correct that
// CONCENTRATION specifically resists the naive sweep, and correct to flag
// that as narrow; it understated the hole, which was exploitable across the
// whole rule set once the harness gives each rule real caps and a real NAV to
// evaluate against, instead of parameters that reject unconditionally
// regardless of the arithmetic.
func TestArithmeticSweep_NoWrapAdmitsAnOrder(t *testing.T) {
	ctx := context.Background()
	// Exponents straddle the gap of 19 where pow10's int64 used to overflow,
	// and -25 where the alignment used to go negative.
	exponents := []int32{0, -1, -2, -8, -17, -18, -19, -20, -25, -30, -38, -40}
	coefficients := []int64{
		1, 5, 1000,
		1 << 62, -(1 << 62),
		math.MaxInt64, -math.MaxInt64,
		1844674407, 9223372036,
		// These two are not arbitrary: against the book's existing 1000@exp0
		// AAPL quantity, the pre-fix addDecimal computes
		// 1000*oldPow10(exponentGap) for the aligned existing side, and
		// oldPow10 wraps at a gap of 19 and again at 25. These coefficients
		// are exactly -(that wrapped value), so paired with exponent -19 (resp.
		// -25) the wrapped sum lands on EXACTLY ZERO — not a small residual,
		// zero. That matters because heldPositions (rules.go) drops any
		// position whose market value is exactly zero, so a client sending
		// this coefficient at this exponent — a completely ordinary
		// *commonpb.Decimal, nothing the wire format forbids — makes an
		// existing $100,000 AAPL position VANISH from every rule's view for
		// one instruction cycle, not because it was sold, but because the
		// alignment arithmetic cancelled it by accident of overflow.
		-1864712049423024128, // pairs with exponent -19
		-4477988020393345024, // pairs with exponent -25
	}

	for name, rule := range sweepRules() {
		t.Run(name, func(t *testing.T) {
			reg := NewMandateRegistry()
			reg.Put(mandate(rule))
			g := NewPreTradeGate(nil, MapBookSource{"p1": sweepBook()}, reg, sweepClassifier, nil, nil)

			admitted := 0
			for _, exp := range exponents {
				for _, coeff := range coefficients {
					d := OrderDelta{
						PortfolioID:    "p1",
						InstrumentID:   "AAPL",
						SignedQuantity: &commonpb.Decimal{Coefficient: coeff, Exponent: exp},
						Price:          dec(100, 0),
						Currency:       "USD",
						OrderID:        "o1",
						AsOf:           t0,
					}
					verdict, err := g.Evaluate(ctx, d)
					if err != nil {
						t.Fatalf("Evaluate(coeff=%d exp=%d): %v", coeff, exp, err)
					}
					if verdict.Allowed {
						admitted++
						t.Errorf("ADMITTED with coeff=%d exp=%d — an order of this size "+
							"against a $100,000 book must be refused by %s or refused as "+
							"unvaluable, never allowed", coeff, exp, name)
					}
				}
			}
			if admitted > 0 {
				t.Fatalf("%s admitted %d of %d cases", name, admitted, len(exponents)*len(coefficients))
			}
		})
	}
}
