package benchmarks

import (
	"math"
	"strconv"

	"github.com/eighred/kanz/internal/risk/xva"
	"github.com/eighred/kanz/internal/validation"
)

// THE CDS BOOTSTRAP AND THE CREDIT CURVE IT PRODUCES (#471).
//
// # What this validates, precisely
//
// xva.BootstrapCDS and the xva.CreditCurve it returns. internal/risk/pricing/credit
// is the scheduling wrapper — it pulls quotes and publishes the result
// point-in-time — so the analytic being graded is the bootstrap itself, which is
// where a wrong number would come from.
//
// It matters more than its position in the inventory suggests. CVA integrates
// against Survival(t), so a curve that understates default probability
// understates the charge on every counterparty exposure, and it does so
// silently: a smaller CVA is not an error condition.
//
// # The round trip is the defining property
//
// A bootstrap's whole contract is that the curve it builds REPRICES the quotes
// it was built from. Feed it par spreads, ask the curve for the model par spread
// at each tenor, and get the quotes back. That is definitional rather than
// approximate, so it carries a tight tolerance — and it fails on any error in
// the hazard solve, the RPV01, the protection leg or the discounting, because
// all four enter the par spread.
//
// # Why the credit triangle is here as a loose case, not a tight one
//
// h ≈ s/(1−R) is the published approximation everyone quotes, and it IS an
// approximation: it drops discounting and the accrual-on-default half period. It
// is included because it catches an answer of the wrong ORDER — a hazard of 1.7%
// against a 100bp spread at 40% recovery is right, 0.17% or 17% is not — and
// excluded from any tight comparison because the exact bootstrap is supposed to
// differ from it.

// AnalyticCDSBootstrap is the CDS curve bootstrap.
const AnalyticCDSBootstrap = "cds_bootstrap"

// A conventional investment-grade quote set at a conventional recovery.
var (
	cdsRecovery = 0.40
	cdsQuotes   = []xva.CDSQuote{
		{Tenor: 1, Spread: 0.0080},
		{Tenor: 3, Spread: 0.0100},
		{Tenor: 5, Spread: 0.0120},
		{Tenor: 10, Spread: 0.0150},
	}
	// Discounting at a flat continuously-compounded rate. Present so the round
	// trip exercises the discounted path rather than the DF≡1 shortcut — an
	// error in the discount handling cancels out of an undiscounted test.
	cdsDF = func(t float64) float64 { return math.Exp(-0.03 * t) }
)

// CDSBootstrap grades the bootstrap and the survival curve it produces.
func CDSBootstrap() []validation.Case {
	cases := make([]validation.Case, 0, 16)

	curve, err := xva.BootstrapCDS(cdsQuotes, cdsRecovery, cdsDF)
	if err != nil {
		// A benchmark set that cannot build its own fixture must not silently
		// return fewer cases — that reads as a smaller, passing suite.
		return []validation.Case{{
			Name: "cds_bootstrap_builds_a_curve_from_a_conventional_quote_set",
			Got:  0, Want: 1, Tolerance: 0.5,
		}}
	}

	// ===== THE ROUND TRIP: the defining property =====
	for _, q := range cdsQuotes {
		cases = append(cases, validation.Case{
			Name: "bootstrapped_curve_reprices_its_own_" + trimFloat(q.Tenor) + "y_quote_definitional",
			Got:  curve.ParSpread(q.Tenor, cdsDF), Want: q.Spread, Tolerance: 1e-8,
		})
	}

	// ===== THE SURVIVAL CURVE'S PUBLISHED PROPERTIES =====
	cases = append(cases,
		validation.Case{
			// Q(0) = 1 by definition: nothing has defaulted at inception.
			Name: "survival_at_zero_is_one_published",
			Got:  curve.Survival(0), Want: 1, Tolerance: 1e-12,
		},
		validation.Case{
			// STRICTLY DECREASING. A hazard is non-negative, so survival cannot
			// rise — and a curve that rises somewhere implies a NEGATIVE default
			// probability over that interval, which prices as a credit rebate.
			Name: "survival_is_monotone_decreasing_published",
			Got:  boolAsFloat(survivalDecreases(curve)), Want: 1, Tolerance: 0.5,
		},
		validation.Case{
			// Q(t) ∈ (0,1]. A survival above 1 or below 0 is not a probability.
			Name: "survival_stays_within_zero_and_one_published",
			Got:  boolAsFloat(survivalInBounds(curve)), Want: 1, Tolerance: 0.5,
		},
		validation.Case{
			// IDENTITY: the marginal default probabilities over a partition sum
			// to the total, Q(0) − Q(T). It catches a double-count or a gap in
			// the piecewise-constant integration that each interval alone hides.
			Name: "marginal_default_probabilities_sum_to_the_total_identity",
			Got:  marginalSum(curve, 10), Want: 1 - curve.Survival(10), Tolerance: 1e-12,
		},
		validation.Case{
			// LGD = 1 − R, definitional, and it is the multiplier on the whole
			// protection leg.
			Name: "lgd_is_one_minus_recovery_definitional",
			Got:  curve.LGD(), Want: 1 - cdsRecovery, Tolerance: 1e-12,
		},
	)

	// ===== THE FLAT-HAZARD CLOSED FORM =====
	//
	// Q(t) = exp(−h·t) exactly, evaluated on the INPUTS. It is the one place the
	// piecewise integration has a published answer to be checked against rather
	// than a property to satisfy.
	const flatH = 0.02
	flat := xva.FlatHazard(flatH, cdsRecovery)
	for _, t := range []float64{0.5, 2, 7} {
		cases = append(cases, validation.Case{
			Name: "flat_hazard_survival_matches_exp_minus_ht_at_" + trimFloat(t) + "y_published",
			Got:  flat.Survival(t), Want: math.Exp(-flatH * t), Tolerance: 1e-12,
		})
	}

	// ===== THE CREDIT TRIANGLE, as an order-of-magnitude check =====
	//
	// FromCDS applies it directly, so this grades that helper rather than the
	// bootstrap: h = s/(1−R) exactly, by construction.
	tri := xva.FromCDS(0.0100, cdsRecovery)
	cases = append(cases, validation.Case{
		Name: "credit_triangle_hazard_is_spread_over_lgd_published",
		Got:  tri.Hazards[0], Want: 0.0100 / (1 - cdsRecovery), Tolerance: 1e-12,
	})
	// AND THE BOOTSTRAP AGREES WITH IT TO THE RIGHT ORDER. Loose on purpose: the
	// exact bootstrap discounts and accrues on default, so it is SUPPOSED to
	// differ. What it must not do is differ by a factor.
	if boot5 := hazardAt(curve, 5); boot5 > 0 {
		cases = append(cases, validation.Case{
			Name: "bootstrapped_hazard_is_within_an_order_of_the_credit_triangle_published",
			Got:  boolAsFloat(boot5 > 0.5*0.0120/(1-cdsRecovery) && boot5 < 2*0.0120/(1-cdsRecovery)),
			Want: 1, Tolerance: 0.5,
		})
	}

	// ===== AN ARBITRAGEABLE QUOTE SET IS REFUSED =====
	//
	// A spread curve inverted enough to need a NEGATIVE forward hazard implies a
	// negative default probability over that segment. Accepting it would produce
	// a curve that prices a credit rebate, and CVA would come out too low with
	// nothing failing — so the bootstrap must refuse rather than clamp.
	_, invErr := xva.BootstrapCDS([]xva.CDSQuote{
		{Tenor: 1, Spread: 0.0500}, {Tenor: 2, Spread: 0.0001},
	}, cdsRecovery, cdsDF)
	cases = append(cases, validation.Case{
		Name: "an_arbitrageable_quote_set_is_refused_not_clamped",
		Got:  boolAsFloat(invErr != nil), Want: 1, Tolerance: 0.5,
	})

	return cases
}

// survivalDecreases checks monotonicity on a fine grid past the last pillar, so
// the flat extrapolation is covered too.
func survivalDecreases(c xva.CreditCurve) bool {
	prev := c.Survival(0)
	for t := 0.05; t <= 12; t += 0.05 {
		q := c.Survival(t)
		if q > prev+1e-15 {
			return false
		}
		prev = q
	}
	return true
}

func survivalInBounds(c xva.CreditCurve) bool {
	for t := 0.0; t <= 12; t += 0.05 {
		q := c.Survival(t)
		if q <= 0 || q > 1+1e-15 {
			return false
		}
	}
	return true
}

// marginalSum accumulates the marginal default probabilities over yearly buckets
// to T.
func marginalSum(c xva.CreditCurve, T float64) float64 {
	var sum float64
	for a := 0.0; a < T; a++ {
		sum += c.DefaultProbability(a, a+1)
	}
	return sum
}

// hazardAt returns the piecewise-constant forward hazard covering t.
func hazardAt(c xva.CreditCurve, t float64) float64 {
	for k, end := range c.Tenors {
		if t <= end {
			return c.Hazards[k]
		}
	}
	if len(c.Hazards) == 0 {
		return 0
	}
	return c.Hazards[len(c.Hazards)-1]
}

// trimFloat renders a tenor for a case NAME, with "p" for the decimal point.
//
// A case name is an identifier a failure is read by, so 0.5 must not collapse to
// "0" — two cases that differ only in tenor would then share a name, and the
// report would say which tenor failed only by luck of ordering.
func trimFloat(f float64) string {
	whole := int(f)
	frac := int(math.Round((f-float64(whole))*10)) % 10
	if frac == 0 {
		return strconv.Itoa(whole)
	}
	return strconv.Itoa(whole) + "p" + strconv.Itoa(frac)
}
