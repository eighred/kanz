package compliance

import (
	"context"
	"math"
	"math/big"
	"testing"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	compliancepb "github.com/kanz-eng/kanz-schemas-go/compliance/v1"
)

// sweepPrice is the order price every swept case is marked at. project()
// re-marks the affected position at this price, so the projected AAPL market
// value is exactly |existing quantity + swept quantity| × sweepPrice — which is
// what the expectation model below computes in *big.Rat.
const sweepPrice = 10

// sweepBook is the fixture the whole sweep hangs on, and its defining property
// is that IT IS COMPLIANT ON ITS OWN under the value-based rules below.
//
// The first version of this file was not. It held $100,000 of AAPL against a
// $100,000 NAV and set every cap below the book's own baseline, so the baseline
// breached every rule unaided and all five arms refused all 132 cases no matter
// what the arithmetic returned — including for an order in an unrelated
// instrument with a negligible quantity. Every result was predetermined and no
// assertion depended on the projected quantity being correct. The only
// regression it could still catch was total position ERASURE.
//
// So: AAPL is 1000 units marked at sweepPrice ($10,000), GOVT is an untouched
// $90,000, NAV is $100,000. Gross is $100,000 = 1.0x NAV and AAPL's weight is
// 10%, both comfortably inside the caps in sweepRules. Only the swept order can
// push the book over, which is what makes an admission or a refusal informative.
// NAV is set deliberately: LeverageRule fails closed on a nil/non-positive NAV
// (rules.go), which would refuse every case regardless of the arithmetic.
func sweepBook() *Book {
	return &Book{
		PortfolioID:  "p1",
		BaseCurrency: "USD",
		NAV:          &commonpb.Money{Amount: dec(100000, 0), CurrencyCode: "USD"},
		Positions: []Position{
			{
				InstrumentID: "AAPL",
				Quantity:     dec(1000, 0),
				MarketValue:  &commonpb.Money{Amount: dec(10000, 0), CurrencyCode: "USD"},
			},
			{
				InstrumentID: "GOVT",
				Quantity:     dec(900, 0),
				MarketValue:  &commonpb.Money{Amount: dec(90000, 0), CurrencyCode: "USD"},
			},
		},
	}
}

// sweepClassifier resolves both fixture instruments to issuers of the same
// name. Only IssuerExclusionRule needs a classifier at all (rules.go: bucketKey
// resolves DIMENSION_INSTRUMENT straight off Position.InstrumentID, and
// CurrencyRule reads Position.MarketValue's currency code directly — neither
// touches the classifier), but issuer identity is resolved exclusively through
// classify(c, pos).Issuer, so a nil classifier would make issuer_exclusion
// permanently unbreachable — a gap in the harness having nothing to do with the
// arithmetic under test — rather than exercising the rule at all.
var sweepClassifier = StaticClassifier{
	"AAPL": {Issuer: "AAPL"},
	"GOVT": {Issuer: "GOVT"},
}

// sweepArm is one rule under test, plus the model of what that rule SHOULD say
// about a projected AAPL market value computed correctly.
//
//   - deltaSensitive arms (concentration, gross_leverage) key off MAGNITUDE:
//     their verdict is a function of the projected value, so getting the
//     quantity wrong by any material amount flips them. These are the arms that
//     make this file a real arithmetic regression guard.
//
//   - erasure-only arms (restriction, issuer_exclusion, currency) key off
//     IDENTITY — instrument, issuer, currency — and by design cannot depend on
//     magnitude. The one arithmetic failure they can still see is the one that
//     removes the position from the book entirely: heldPositions (rules.go)
//     drops a zero-valued position as flat, so a projected value of exactly
//     zero makes AAPL invisible and the rule silently passes. That is COMP-M1's
//     failure mode and worth guarding, but it is ALL these three prove. They are
//     not evidence that the arithmetic is correct, only that it is non-zero.
//
// THE RESOLUTION OF THE TWO DELTA-SENSITIVE ARMS, stated so nobody reads more
// into a green run than it earns. The caps sit at 10%→20% weight and 1.0x→2.0x
// leverage, so a PROPORTIONAL valuation error only flips a verdict at roughly
// ×1.2 over-valuation or ×0.5 under-valuation. A projected value 20% too SMALL
// passes this sweep silently — and under-valuation is the dangerous direction,
// because it is the one that admits an order the rules should have refused.
//
// This is an ORDER-OF-MAGNITUDE guard, not a proportional one. It is left that
// way deliberately: no plausible defect in exact math/big code lands in the
// 1-ULP-to-2× band — the arithmetic either rounds by an ulp (caught by the unit
// tests in mul_test.go, which do have ulp resolution) or collapses entirely
// (caught here). Tightening the caps to chase the middle would be inventing a
// failure mode nobody has produced. If a future change makes proportional drift
// plausible, this is the comment that says the sweep will not see it.
type sweepArm struct {
	name string
	rule *compliancepb.Rule
	// orderCurrency is the currency the swept order is denominated in. Only
	// the currency arm varies it (see its comment below).
	orderCurrency string
	// deltaSensitive marks the arms whose expectation below actually reads mv.
	deltaSensitive bool
	// breaches reports whether the rule should breach given the projected AAPL
	// market value, as an exact non-negative Rat. mv.Sign()==0 means AAPL was
	// dropped by heldPositions.
	breaches func(mv *big.Rat) bool
}

// The fixture's untouched side, as exact Rats: GOVT's $90,000 and the $100,000
// NAV. Every expectation below is expressed against these.
func sweepOtherGross() *big.Rat { return big.NewRat(90000, 1) }
func sweepNAV() *big.Rat        { return big.NewRat(100000, 1) }

// EVERY rule type the engine implements.
func sweepArms() []sweepArm {
	return []sweepArm{
		{
			name: "concentration",
			rule: &compliancepb.Rule{
				RuleId: "c1", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
				Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
					Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT,
					// Bucketed on AAPL specifically so GOVT's 90% baseline
					// weight does not breach the cap on its own — without the
					// bucket this rule would have to be loosened past 90% and
					// the delta would need to be absurd before it bit.
					Bucket:    "AAPL",
					MaxWeight: dec(20, -2), // 20%; baseline AAPL weight is 10%
				}},
			},
			orderCurrency:  "USD",
			deltaSensitive: true,
			// weight = mv / (mv + 90000) > 0.20  ⇔  mv > 22500.
			breaches: func(mv *big.Rat) bool {
				if mv.Sign() == 0 {
					return false // dropped by heldPositions: zero weight
				}
				total := new(big.Rat).Add(mv, sweepOtherGross())
				weight := new(big.Rat).Quo(mv, total)
				return weight.Cmp(big.NewRat(20, 100)) > 0
			},
		},
		{
			name: "gross_leverage",
			rule: &compliancepb.Rule{
				RuleId: "l1", Type: compliancepb.RuleType_RULE_TYPE_GROSS_LEVERAGE,
				Params: &compliancepb.Rule_LeverageCap{LeverageCap: &compliancepb.LeverageCap{
					// 2.0x, ABOVE the book's own 1.0x baseline, so the baseline
					// passes and only the swept order can breach the cap.
					MaxGrossLeverage: dec(20, -1),
				}},
			},
			orderCurrency:  "USD",
			deltaSensitive: true,
			// (mv + 90000) / 100000 > 2.0  ⇔  mv > 110000.
			breaches: func(mv *big.Rat) bool {
				gross := new(big.Rat).Add(mv, sweepOtherGross())
				lev := new(big.Rat).Quo(gross, sweepNAV())
				return lev.Cmp(big.NewRat(2, 1)) > 0
			},
		},
		{
			name: "restriction",
			rule: &compliancepb.Rule{
				RuleId: "r1", Type: compliancepb.RuleType_RULE_TYPE_RESTRICTION,
				Params: &compliancepb.Rule_Restriction{Restriction: &compliancepb.RestrictionList{
					Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT,
					Values:    []string{"AAPL"},
					Mode:      compliancepb.RestrictionMode_RESTRICTION_MODE_DENY,
				}},
			},
			orderCurrency:  "USD",
			deltaSensitive: false,
			// Held AAPL at any non-zero value breaches; erased, it does not.
			breaches: func(mv *big.Rat) bool { return mv.Sign() != 0 },
		},
		{
			name: "issuer_exclusion",
			rule: &compliancepb.Rule{
				RuleId: "e1", Type: compliancepb.RuleType_RULE_TYPE_ISSUER_EXCLUSION,
				Params: &compliancepb.Rule_IssuerExclusion{IssuerExclusion: &compliancepb.IssuerExclusion{
					IssuerIds: []string{"AAPL"}, // GOVT is NOT excluded
				}},
			},
			orderCurrency:  "USD",
			deltaSensitive: false,
			breaches:       func(mv *big.Rat) bool { return mv.Sign() != 0 },
		},
		{
			name: "currency",
			rule: &compliancepb.Rule{
				RuleId: "cc1", Type: compliancepb.RuleType_RULE_TYPE_CURRENCY,
				Params: &compliancepb.Rule_CurrencyRestriction{CurrencyRestriction: &compliancepb.CurrencyRestriction{
					AllowedCurrencies: []string{"USD"},
				}},
			},
			// The book is entirely USD, so with USD allowed the baseline passes.
			// The swept order is JPY, which project() stamps onto the AAPL
			// position it re-marks — so the projected book breaches, unless the
			// arithmetic erases that position first. (Gross exposure is summed
			// without FX conversion across the whole rule set; that is a known
			// simplification deferred with the FX layer, RISK-06, and it does
			// not affect this arm, which reads only the currency code.)
			orderCurrency:  "JPY",
			deltaSensitive: false,
			breaches:       func(mv *big.Rat) bool { return mv.Sign() != 0 },
		},
	}
}

// sweepExponents straddles the gap of 19 where the old pow10's int64 overflowed
// and -25 where the alignment used to go negative.
var sweepExponents = []int32{0, -1, -2, -8, -17, -18, -19, -20, -25, -30, -38, -40}

var sweepCoefficients = []int64{
	// 0 is here because of the zero-coefficient guard in addDecimal (see
	// TestAddDecimal_ZeroCoefficientDoesNotAnnihilateTheOtherOperand): the
	// sweep carried no zero at all, which is why it never explored the one
	// operand shape that can annihilate the other. At these exponents the
	// guard is not load-bearing — the gate-level erasure proof below is what
	// pins it — but a zero quantity must still project the book unchanged, and
	// that is a case worth sweeping across every rule.
	0,
	1, 5, 1000,
	1 << 62, -(1 << 62),
	math.MaxInt64, -math.MaxInt64,
	1844674407, 9223372036,
	// These two are not arbitrary: against the book's existing 1000@exp0 AAPL
	// quantity, the pre-fix addDecimal computes 1000*oldPow10(exponentGap) for
	// the aligned existing side, and oldPow10 wraps at a gap of 19 and again at
	// 25. These coefficients are exactly -(that wrapped value), so paired with
	// exponent -19 (resp. -25) the wrapped sum lands on EXACTLY ZERO — not a
	// small residual, zero. heldPositions (rules.go) drops any position whose
	// market value is exactly zero, so a client sending this coefficient at
	// this exponent — a completely ordinary *commonpb.Decimal, nothing the wire
	// format forbids — makes an existing AAPL position VANISH from every rule's
	// view for one instruction cycle, not because it was sold, but because the
	// alignment arithmetic cancelled it by accident of overflow.
	-1864712049423024128, // pairs with exponent -19
	-4477988020393345024, // pairs with exponent -25
}

// projectedAAPLValue is the expectation model: the exact |1000 + coeff×10^exp| ×
// sweepPrice that a correct addDecimal/mulDecimal pair must produce, in *big.Rat.
// No floats anywhere.
func projectedAAPLValue(coeff int64, exp int32) *big.Rat {
	delta := new(big.Rat).SetInt64(coeff)
	pow := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(abs32(exp))), nil))
	if exp < 0 {
		delta.Quo(delta, pow)
	} else {
		delta.Mul(delta, pow)
	}
	qty := delta.Add(delta, big.NewRat(1000, 1))
	mv := qty.Mul(qty, new(big.Rat).SetInt64(sweepPrice))
	return mv.Abs(mv)
}

func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

// nearThreshold reports whether mv sits within a 1e-12 relative band of t.
// addDecimal and mulDecimal are exact until the coefficient outgrows an int64,
// at which point both RAISE the exponent (half-up) and drop digits that cannot
// matter at that magnitude — so a projected value within a hair of a cap may
// legitimately land on either side of it. Those cases are excluded from the
// verdict assertion rather than pinned to an arbitrary rounding direction; the
// non-vacuity check below guarantees plenty of cases remain on both sides.
func nearThreshold(mv, t *big.Rat) bool {
	if t.Sign() == 0 {
		return mv.Sign() == 0
	}
	diff := new(big.Rat).Sub(mv, t)
	diff.Abs(diff)
	band := new(big.Rat).Mul(new(big.Rat).Abs(t), big.NewRat(1, 1_000_000_000_000))
	return diff.Cmp(band) <= 0
}

// thresholdFor is the projected AAPL value at which each delta-sensitive arm
// flips. Erasure-only arms flip at zero.
func thresholdFor(arm sweepArm) *big.Rat {
	switch arm.name {
	case "concentration":
		return big.NewRat(22500, 1) // mv/(mv+90000) = 0.20
	case "gross_leverage":
		return big.NewRat(110000, 1) // (mv+90000)/100000 = 2.0
	default:
		return new(big.Rat)
	}
}

// TestArithmeticSweep_ProjectedQuantityDrivesEveryRule drives every rule type
// against a wide range of quantity exponents and coefficients and asserts, per
// case, that the gate's verdict matches what the rule SHOULD say about an
// exactly-computed projected position — not merely that everything is refused.
//
// The board recorded this hole as "not demonstrated exploitable via
// CONCENTRATION" on the strength of a 738-case sweep against that one rule —
// whose numerator and denominator move together, which is exactly why it masked
// the defect. That was one negative result about one of five rules. This is the
// whole rule set, and it is a permanent test rather than a one-off script.
//
// Run against the pre-fix arithmetic (commit 0450d62's single-return, unclamped
// pow10 addDecimal) this sweep fails hard, and the failures are of both kinds:
// wrapped alignment sums that make a genuinely enormous order look small enough
// to ADMIT under the value caps, and the engineered exact-zero collisions that
// erase the AAPL position outright and let all five arms — the three
// identity-based ones included — pass a book that plainly breaches them.
func TestArithmeticSweep_ProjectedQuantityDrivesEveryRule(t *testing.T) {
	ctx := context.Background()

	for _, arm := range sweepArms() {
		t.Run(arm.name, func(t *testing.T) {
			reg := NewMandateRegistry()
			reg.Put(mandate(arm.rule))
			g := NewPreTradeGate(nil, MapBookSource{"p1": sweepBook()}, reg, sweepClassifier, nil, nil)

			threshold := thresholdFor(arm)
			var wantAllowed, wantRefused, skipped int

			for _, exp := range sweepExponents {
				for _, coeff := range sweepCoefficients {
					mv := projectedAAPLValue(coeff, exp)
					if arm.deltaSensitive && nearThreshold(mv, threshold) {
						skipped++
						continue
					}
					want := !arm.breaches(mv) // want == "should be allowed"
					if want {
						wantAllowed++
					} else {
						wantRefused++
					}

					d := OrderDelta{
						PortfolioID:    "p1",
						InstrumentID:   "AAPL",
						SignedQuantity: &commonpb.Decimal{Coefficient: coeff, Exponent: exp},
						Price:          dec(sweepPrice, 0),
						Currency:       arm.orderCurrency,
						OrderID:        "o1",
						AsOf:           t0,
					}
					verdict, err := g.Evaluate(ctx, d)
					if err != nil {
						t.Fatalf("Evaluate(coeff=%d exp=%d): %v", coeff, exp, err)
					}
					if verdict.Allowed != want {
						t.Errorf("coeff=%d exp=%d: Allowed = %v, want %v — the projected AAPL "+
							"market value is exactly %s, which %s breach %s",
							coeff, exp, verdict.Allowed, want, mv.FloatString(4),
							map[bool]string{true: "does", false: "does not"}[!want], arm.name)
					}
				}
			}

			// NON-VACUITY, for the arms that can have it. The previous version
			// of this sweep asserted "nothing is admitted" against a fixture
			// that breached every cap unaided, so it passed without ever
			// consulting the projected value — 132/132 refused in all five
			// arms, and an order in an unrelated instrument with a negligible
			// quantity refused too. A delta-sensitive arm must therefore expect
			// BOTH verdicts somewhere in the matrix; if the fixture drifts back
			// to predetermined this fails rather than going quietly green.
			//
			// The three identity-based arms cannot clear that bar and are not
			// asked to. They key off instrument/issuer/currency, so their
			// verdict is a function of WHETHER AAPL is held, never of how much
			// of it — and the swept order is in AAPL, so under correct
			// arithmetic the projected book always holds it and they always
			// refuse. They are kept as ERASURE guards and nothing more: the two
			// engineered zero-collision coefficients above make the pre-fix
			// arithmetic cancel the position to exactly zero, heldPositions
			// drops it, and these three admit. That is a real regression to
			// hold the line on; it is not evidence the arithmetic is correct.
			if arm.deltaSensitive && wantAllowed == 0 {
				t.Fatalf("%s: no case in the matrix is expected to be ALLOWED — the fixture "+
					"breaches on its own again and the sweep proves nothing", arm.name)
			}
			if wantRefused == 0 {
				t.Fatalf("%s: no case in the matrix is expected to be REFUSED — the caps are "+
					"unreachable and the sweep proves nothing", arm.name)
			}
			t.Logf("%s: %d expected-allow, %d expected-refuse, %d skipped near threshold (delta-sensitive=%v)",
				arm.name, wantAllowed, wantRefused, skipped, arm.deltaSensitive)
		})
	}
}

// TestArithmeticSweep_CorrectValuationIsWhatBreaches is the sweep's claim
// stated as a single, readable pair of cases, and it is the one that would
// catch an arithmetic regression the matrix above might round past.
//
// A BUY of 20,000 AAPL on top of the existing 1,000 projects to 21,000 units ×
// $10 = $210,000: 70% of the book, 3.0x NAV. Both value caps must refuse it.
// Under an addDecimal that loses or wraps the delta, the same order projects to
// something near the untouched baseline and both caps ADMIT it — which is the
// whole exploit in one line.
//
// The negligible order is the control: a delta of 10^-40 units genuinely leaves
// the book where it was, and both value arms must ALLOW it. Without this half,
// an arm that refuses unconditionally would still pass the half above.
func TestArithmeticSweep_CorrectValuationIsWhatBreaches(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name        string
		qty         *commonpb.Decimal
		wantAllowed bool
	}{
		{"a genuinely large BUY breaches the value caps", dec(20000, 0), false},
		{"a negligible BUY leaves the compliant book compliant", dec(1, -40), true},
	}
	for _, arm := range sweepArms() {
		if !arm.deltaSensitive {
			continue
		}
		t.Run(arm.name, func(t *testing.T) {
			reg := NewMandateRegistry()
			reg.Put(mandate(arm.rule))
			g := NewPreTradeGate(nil, MapBookSource{"p1": sweepBook()}, reg, sweepClassifier, nil, nil)
			for _, tc := range cases {
				verdict, err := g.Evaluate(ctx, OrderDelta{
					PortfolioID:    "p1",
					InstrumentID:   "AAPL",
					SignedQuantity: tc.qty,
					Price:          dec(sweepPrice, 0),
					Currency:       arm.orderCurrency,
					OrderID:        "o1",
					AsOf:           t0,
				})
				if err != nil {
					t.Fatalf("%s: %v", tc.name, err)
				}
				if verdict.Allowed != tc.wantAllowed {
					t.Fatalf("%s: Allowed = %v, want %v", tc.name, verdict.Allowed, tc.wantAllowed)
				}
			}
		})
	}
}

// TestArithmeticSweep_ShrinkingTheStandingPositionCannotAdmit covers the
// direction the sweep above structurally cannot, and the omission is worth
// stating because it is the reason this test exists.
//
// The sweep's fixture is COMPLIANT on its own, so its caps only bite from
// ABOVE: they catch an arithmetic error that makes a position look too big.
// The pre-fix addDecimal's error runs the other way. Aligning to the order's
// exponent multiplied the STANDING 1,000-unit quantity by an int64 pow10 that
// wrapped for any gap past 18, collapsing a $10,000 holding to $776, then
// $38.76, then $1.86, then to nothing as the exponent went further negative —
// measured, not assumed. Both the true value and the collapsed value sit under
// a cap set above the baseline, so both arms agree and the sweep sees nothing.
//
// Under-valuation is the dangerous direction: it is how a breach gets admitted.
// Catching it needs a book that ALREADY breaches, where a negligible order must
// change nothing. So this fixture deliberately inverts the sweep's: the same
// book, caps set just under its own baseline (AAPL at 10% of gross against a 5%
// cap; 1.0x gross leverage against a 0.95x cap), and an order too small to move
// either. Correct arithmetic leaves the breach standing and refuses. The pre-fix
// arithmetic shrinks AAPL below both caps and ADMITS — the whole exploit, in the
// two arms that can measure it.
func TestArithmeticSweep_ShrinkingTheStandingPositionCannotAdmit(t *testing.T) {
	ctx := context.Background()
	arms := []struct {
		name string
		rule *compliancepb.Rule
	}{
		{"concentration", &compliancepb.Rule{
			RuleId: "c2", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
			Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
				Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT,
				Bucket:    "AAPL",
				MaxWeight: dec(5, -2), // baseline AAPL weight is 10% — already breaching
			}},
		}},
		{"gross_leverage", &compliancepb.Rule{
			RuleId: "l2", Type: compliancepb.RuleType_RULE_TYPE_GROSS_LEVERAGE,
			Params: &compliancepb.Rule_LeverageCap{LeverageCap: &compliancepb.LeverageCap{
				// 0.95x against a 1.0x baseline. Set just under, not far under:
				// dropping AAPL entirely still leaves GOVT at 0.90x, so a cap
				// below 0.90 would refuse even a fully erased book and the test
				// would pass for the wrong reason.
				MaxGrossLeverage: dec(95, -2),
			}},
		}},
	}
	// Quantities far too small to move a $10,000 position, at the exponents
	// where the old int64 pow10 alignment wrapped (gap 19) and beyond.
	negligible := []*commonpb.Decimal{
		{Coefficient: 1, Exponent: -19},
		{Coefficient: 1, Exponent: -25},
		{Coefficient: 1, Exponent: -40},
		{Coefficient: -1, Exponent: -19}, // a negligible SELL, same requirement
	}
	for _, arm := range arms {
		t.Run(arm.name, func(t *testing.T) {
			reg := NewMandateRegistry()
			reg.Put(mandate(arm.rule))
			g := NewPreTradeGate(nil, MapBookSource{"p1": sweepBook()}, reg, sweepClassifier, nil, nil)
			for _, qty := range negligible {
				verdict, err := g.Evaluate(ctx, OrderDelta{
					PortfolioID:    "p1",
					InstrumentID:   "AAPL",
					SignedQuantity: qty,
					Price:          dec(sweepPrice, 0),
					Currency:       "USD",
					OrderID:        "o1",
					AsOf:           t0,
				})
				if err != nil {
					t.Fatalf("coeff=%d exp=%d: %v", qty.GetCoefficient(), qty.GetExponent(), err)
				}
				if verdict.Allowed {
					t.Errorf("coeff=%d exp=%d: ADMITTED — the book breaches %s on its own and this "+
						"order is too small to change that, so the projected AAPL position was "+
						"under-valued on the way in",
						qty.GetCoefficient(), qty.GetExponent(), arm.name)
				}
			}
		})
	}
}

// TestPreTradeGate_ZeroCoefficientCannotEraseTheBook is the downstream half of
// TestAddDecimal_ZeroCoefficientDoesNotAnnihilateTheOtherOperand (mul_test.go):
// it drives the same operand shape through the real gate and pins what the
// guard actually protects.
//
// A zero-coefficient quantity carrying an absurd exponent is an ordinary
// *commonpb.Decimal — Quantity.Exponent is an unvalidated wire field and the
// gate runs before Accept validates the order — so this is attacker-reachable.
// Without addDecimal's zero-coefficient guard the projected quantity comes back
// 0, mulDecimal values the position at 0, heldPositions (rules.go) drops it as
// flat, and the projected book loses the AAPL holding entirely. Every one of
// these five rules then passes a book that plainly breaches it. That is
// COMP-M1's defect reached through a different door, which is why it is pinned
// at both levels.
//
// A zero quantity is a no-op order: the projected book is the current book, and
// every rule must say exactly what it says about the current book.
func TestPreTradeGate_ZeroCoefficientCannotEraseTheBook(t *testing.T) {
	ctx := context.Background()
	for _, arm := range sweepArms() {
		t.Run(arm.name, func(t *testing.T) {
			reg := NewMandateRegistry()
			reg.Put(mandate(arm.rule))
			g := NewPreTradeGate(nil, MapBookSource{"p1": sweepBook()}, reg, sweepClassifier, nil, nil)

			// Exponents comfortably past alignWindow (40) in both directions —
			// far enough that an unguarded alignExponent clamps the real
			// operand away entirely, but small enough that the resulting
			// Decimal's exponent stays modest. That second property is
			// deliberate: ratFromDecimal (engine.go) materialises 10^|exponent|
			// unconditionally, so a projected value carrying an exponent in the
			// billions does not fail this test, it HANGS it. The billion-scale
			// exponents are pinned one level down, on addDecimal itself, where
			// nothing materialises the scale
			// (TestAddDecimal_ZeroCoefficientDoesNotAnnihilateTheOtherOperand).
			for _, exp := range []int32{100, 1000, -100, -1000} {
				verdict, err := g.Evaluate(ctx, OrderDelta{
					PortfolioID:    "p1",
					InstrumentID:   "AAPL",
					SignedQuantity: &commonpb.Decimal{Coefficient: 0, Exponent: exp},
					Price:          dec(sweepPrice, 0),
					Currency:       arm.orderCurrency,
					OrderID:        "o1",
					AsOf:           t0,
				})
				if err != nil {
					t.Fatalf("exp=%d: %v", exp, err)
				}
				// The book unchanged: AAPL still $10,000 (10% weight, gross
				// 1.0x). The two value arms allow that; the three identity
				// arms breach on the AAPL holding itself.
				want := !arm.breaches(big.NewRat(10000, 1))
				if verdict.Allowed != want {
					t.Fatalf("exp=%d: Allowed = %v, want %v — a zero-coefficient quantity with "+
						"an absurd exponent erased the AAPL position from %s's view",
						exp, verdict.Allowed, want, arm.name)
				}
			}
		})
	}
}
