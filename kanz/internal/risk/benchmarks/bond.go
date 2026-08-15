package benchmarks

import (
	"math"
	"time"

	"github.com/eighred/kanz/internal/risk/pricing"
	"github.com/eighred/kanz/internal/validation"
)

// BOND ANALYTICS (#471).
//
// Fixed-income yield maths has the same property that made Black-Scholes worth
// benchmarking first: the answers are PUBLISHED, they are exact, and they were
// written down long before this implementation existed. Every expected value
// below is either a closed form evaluated on the INPUTS or a textbook relation
// between two outputs — never a number read out of the code.
//
// # Why these particular facts
//
// They are the ones a wrong implementation cannot satisfy by accident:
//
//   - A bond priced at its own coupon rate is worth PAR. It is the definition of
//     par, and it fails on any error in the discounting, the schedule or the
//     accrual — all three have to be right for the cashflows to sum to the face.
//   - A zero-coupon bond's Macaulay duration EQUALS its maturity, because there
//     is one cashflow and the weighted mean of one number is that number. An
//     implementation that mis-weights cashflows passes a price test and fails
//     this one.
//   - Modified duration is Macaulay divided by (1 + y/f). Getting the
//     compounding frequency wrong here is the classic fixed-income error, and it
//     is invisible in the price: both durations come out plausible and the
//     hedge ratio is wrong by a factor nobody notices until a rate move.

// AnalyticBondAnalytics is the yield-based fixed-income pricer and its risk
// measures.
const AnalyticBondAnalytics = "bond_analytics"

// The benchmark bond: a five-year 5% annual, settled on its issue date so there
// is no accrued interest to confound the par identity.
var (
	bondSettle         = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	bondYears          = 5
	bondCoupon         = 0.05
	bondYield          = 0.05
	bondFace   float64 = 100
)

func parBond() pricing.Bond {
	return pricing.Bond{
		Face: bondFace, CouponRate: bondCoupon, Frequency: 1,
		Issue: bondSettle, Maturity: bondSettle.AddDate(bondYears, 0, 0),
		DayCount: pricing.Thirty360,
	}
}

// semiAnnualBond exists for ONE reason: on the annual bonds above, f = 1, so
// (1 + y/f) and (1 + y) are the same number and the modified/Macaulay relation
// cannot tell them apart.
//
// THAT IS NOT HYPOTHETICAL — it was measured. A mutation replacing the code's
// (1 + y/f) with (1 + y) SURVIVED the annual-only benchmark set, while the
// comment above claimed the set caught exactly that error. The frequency is what
// makes the relation a test rather than a restatement.
func semiAnnualBond() pricing.Bond {
	return pricing.Bond{
		Face: bondFace, CouponRate: 0.06, Frequency: 2,
		Issue: bondSettle, Maturity: bondSettle.AddDate(bondYears, 0, 0),
		DayCount: pricing.Thirty360,
	}
}

func zeroBond() pricing.Bond {
	return pricing.Bond{
		Face: bondFace, CouponRate: 0, Frequency: 0,
		Issue: bondSettle, Maturity: bondSettle.AddDate(bondYears, 0, 0),
		DayCount: pricing.Thirty360,
	}
}

// BondAnalytics grades the yield-based pricer and its duration measures.
func BondAnalytics() []validation.Case {
	par := parBond()
	zero := zeroBond()
	parRisk := par.Risk(bondSettle, bondYield)
	zeroRisk := zero.Risk(bondSettle, bondYield)
	const semiYield = 0.06
	semi := semiAnnualBond()
	semiRisk := semi.Risk(bondSettle, semiYield)

	// PUBLISHED closed form for a zero-coupon bond: P = F / (1+y)^n. Evaluated
	// on the INPUTS, so it is independent of everything the pricer does.
	zeroClosedForm := bondFace / math.Pow(1+bondYield, float64(bondYears))

	return []validation.Case{
		{
			// PUBLISHED. A bond whose coupon equals its yield prices at par —
			// the definition of par, and a joint test of the discounting, the
			// coupon schedule and the accrual convention.
			Name: "a_bond_at_its_own_coupon_rate_prices_at_par_published",
			Got:  par.PriceFromYield(bondSettle, bondYield), Want: bondFace, Tolerance: 1e-9,
		},
		{
			// PUBLISHED closed form. 100 / 1.05^5 = 78.352616646845...
			Name: "zero_coupon_price_matches_the_closed_form_published",
			Got:  zeroRisk.DirtyPrice, Want: zeroClosedForm, Tolerance: 1e-9,
		},
		{
			// PUBLISHED. One cashflow, so the cashflow-weighted mean time IS the
			// time to maturity. Catches a mis-weighting a price test cannot see.
			Name: "zero_coupon_macaulay_duration_equals_its_maturity_published",
			Got:  zeroRisk.MacaulayDuration, Want: float64(bondYears), Tolerance: 1e-9,
		},
		{
			// PUBLISHED relation, on the ZERO where Macaulay is known exactly, so
			// this is an absolute check rather than a consistency one:
			// 5 / 1.05 = 4.7619047619...
			Name: "zero_coupon_modified_duration_matches_the_published_relation",
			Got:  zeroRisk.ModifiedDuration, Want: float64(bondYears) / (1 + bondYield), Tolerance: 1e-9,
		},
		{
			// PUBLISHED relation again, now on the COUPON bond, where Macaulay is
			// not known in closed form. This is the frequency error's home: get
			// the compounding wrong and both durations stay plausible while the
			// hedge ratio is wrong by a factor nobody notices until rates move.
			Name: "coupon_bond_modified_is_macaulay_over_one_plus_y_over_f_published",
			Got:  parRisk.ModifiedDuration, Want: parRisk.MacaulayDuration / (1 + bondYield/1), Tolerance: 1e-12,
		},
		{
			// PUBLISHED: a coupon bond's Macaulay duration is strictly LESS than
			// its maturity, because coupons return capital earlier. Equality
			// would mean the coupons are being ignored — which prices correctly
			// only if they are also missing from the price.
			Name: "coupon_bond_duration_is_shorter_than_its_maturity_published",
			Got:  boolAsFloat(parRisk.MacaulayDuration < float64(bondYears)), Want: 1, Tolerance: 0.5,
		},
		{
			// PUBLISHED, and the case with actual teeth: a SEMIANNUAL bond, where
			// f = 2 so (1 + y/f) and (1 + y) are different numbers. The annual
			// cases above cannot distinguish them — measured, not assumed: a
			// mutation swapping one for the other survived until this case existed.
			Name: "semiannual_modified_is_macaulay_over_one_plus_y_over_two_published",
			Got:  semiRisk.ModifiedDuration,
			Want: semiRisk.MacaulayDuration / (1 + semiYield/2), Tolerance: 1e-12,
		},
		{
			// PUBLISHED. The par identity again at a different frequency, so the
			// semiannual coupon schedule is exercised rather than assumed to work
			// because the annual one does.
			Name: "a_semiannual_bond_at_its_own_coupon_rate_prices_at_par_published",
			Got:  semi.PriceFromYield(bondSettle, semiYield), Want: bondFace, Tolerance: 1e-9,
		},
		{
			// IDENTITY: yield and price are inverses of each other. The solver is
			// a bisection over the pricer, so this checks the two agree rather
			// than that either is right — which is why it sits beside the
			// published cases rather than replacing them.
			Name: "price_and_yield_round_trip_identity",
			Got:  par.YieldFromPrice(bondSettle, par.PriceFromYield(bondSettle, 0.037)), Want: 0.037, Tolerance: 1e-8,
		},
		{
			// IDENTITY: price falls as yield rises. A sign error in the
			// discounting inverts this and is otherwise invisible at a single
			// yield, where the number still looks like a price.
			Name: "price_falls_as_yield_rises_identity",
			Got: boolAsFloat(par.PriceFromYield(bondSettle, 0.07) <
				par.PriceFromYield(bondSettle, 0.03)), Want: 1, Tolerance: 0.5,
		},
	}
}
