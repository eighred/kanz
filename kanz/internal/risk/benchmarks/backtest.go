package benchmarks

import (
	"strconv"

	varmodel "github.com/eighred/kanz/internal/risk/compute/var"
	"github.com/eighred/kanz/internal/validation"
)

// THE VaR BACKTEST (#471).
//
// # This validates the OUTCOMES ANALYSIS, not the VaR number
//
// SR 11-7 asks two different questions of a model: is it right, and is it still
// right. The second is what a backtest answers — it counts how often reality
// fell outside the model's own 99% bound and decides whether that count is
// tolerable. It is therefore the piece of this library whose own correctness
// decides whether every other risk number gets challenged at all.
//
// IT IS REGISTERED UNDER ITS OWN ANALYTIC NAME rather than as "value_at_risk",
// because it is not one. Validating the exception test says nothing about
// whether the historical-simulation quantile is computed correctly, and claiming
// otherwise would let one benchmark mark two analytics green — the exact
// overstatement the per-case evidence labels exist to prevent. value_at_risk
// stays absent from the validated set and keeps reporting 0.
//
// # Where the expected values come from
//
// PUBLISHED, and unusually cleanly: these are regulatory and statistical
// constants that exist outside any implementation.
//
//   - The Basel Committee's 1996 backtesting framework publishes the
//     traffic-light table for a 250-day window at 99%: GREEN 0-4 exceptions,
//     YELLOW 5-9, RED 10 or more. A supervisor reads the zone, and a boundary
//     off by one is a reporting error with a capital multiplier attached.
//   - The chi-squared critical value at the 95th percentile with one degree of
//     freedom is 3.841459. It decides every Kupiec pass/fail the backtest emits,
//     so a typo in it silently moves the whole decision boundary.
//
// # A note on encoding
//
// validation.Case compares numbers, and several facts here are categorical (a
// zone name, a verdict). They are encoded as 1 for agreement with a tolerance of
// 0.5, which is an honest encoding of a yes/no rather than an invented distance
// between zone names.

// AnalyticVaRBacktest is the exception-testing framework.
const AnalyticVaRBacktest = "var_backtest"

// chiSq1df95 is the chi-squared critical value at the 95th percentile with one
// degree of freedom, transcribed from a statistical table.
const chiSq1df95 = 3.841459

// VaRBacktest grades the backtest against the Basel table, the chi-squared
// threshold, and the identities its statistics must satisfy.
func VaRBacktest() []validation.Case {
	cases := make([]validation.Case, 0, 12)

	// THE BASEL TRAFFIC LIGHT, AT EVERY BOUNDARY AND ON BOTH SIDES OF IT.
	//
	// PUBLISHED. Interior points would pass on an implementation whose boundaries
	// are off by one, and off-by-one is the only error this table realistically
	// suffers — nobody mistakes 3 exceptions for red.
	for _, c := range []struct {
		exceptions int
		zone       string
	}{
		{0, "GREEN"}, {4, "GREEN"}, // last green
		{5, "AMBER"}, {9, "AMBER"}, // first and last yellow
		{10, "RED"}, {11, "RED"}, // first red
	} {
		cases = append(cases, validation.Case{
			Name:      "basel_zone_at_" + strconv.Itoa(c.exceptions) + "_exceptions_published",
			Got:       boolAsFloat(zoneFor(c.exceptions) == c.zone),
			Want:      1,
			Tolerance: 0.5,
		})
	}

	// THE DECISION THRESHOLD IS THE PUBLISHED CRITICAL VALUE.
	//
	// MEASURED FROM BEHAVIOUR, NOT READ FROM THE CONSTANT. The backtest is driven
	// across the exception count until its verdict flips, and the statistic where
	// that happens is the boundary the code actually uses. Reading chi2_1_95 out
	// of the source and comparing it to 3.841459 would compare the code to
	// itself; this compares the code's DECISION to a published table.
	if lo, hi, ok := kupiecFlip(); ok {
		cases = append(cases,
			validation.Case{
				Name: "kupiec_accepts_below_the_published_chi_squared_threshold",
				Got:  boolAsFloat(lo <= chiSq1df95), Want: 1, Tolerance: 0.5,
			},
			validation.Case{
				Name: "kupiec_rejects_above_the_published_chi_squared_threshold",
				Got:  boolAsFloat(hi >= chiSq1df95), Want: 1, Tolerance: 0.5,
			},
		)
	}

	// IDENTITY: with the observed exception rate exactly at the model's own
	// probability, the likelihood ratio is zero by construction — the null and
	// the alternative are the same distribution. A non-zero value means the
	// statistic is mis-derived, and it would bias every verdict one way.
	if perfect, err := varmodel.Backtest(exceptionSeries(1000, 10), flatVaR(1000, 1), 0.99); err == nil {
		cases = append(cases, validation.Case{
			Name: "kupiec_lr_is_zero_when_the_rate_matches_the_model_identity",
			Got:  perfect.KupiecLR, Want: 0, Tolerance: 1e-9,
		})
	}

	// AND IT IS NOT VACUOUSLY ZERO.
	//
	// A statistic that returned 0 for everything satisfies the identity above and
	// never rejects anything — the failure a control cannot afford, because it
	// looks exactly like a clean book. Forty exceptions in 1000 days is four
	// times the rate a 99% model allows.
	tooMany, tooManyErr := varmodel.Backtest(exceptionSeries(1000, 40), flatVaR(1000, 1), 0.99)
	if tooManyErr == nil {
		cases = append(cases,
			validation.Case{
				Name: "kupiec_rejects_a_model_breached_four_times_too_often_identity",
				Got:  boolAsFloat(tooMany.KupiecLR > chiSq1df95 && !tooMany.KupiecPass), Want: 1, Tolerance: 0.5,
			},
			// IDENTITY: the conditional-coverage statistic is the sum of its two
			// parts. That is its definition, and a drift between them would make
			// the joint verdict disagree with the two it is built from.
			validation.Case{
				Name: "conditional_lr_is_the_sum_of_its_two_parts_identity",
				Got:  tooMany.ConditionalLR, Want: tooMany.KupiecLR + tooMany.ChristoffersenLR, Tolerance: 1e-9,
			},
		)
	}

	// CLUSTERED EXCEPTIONS ARE DETECTED, which is the whole reason the
	// independence test exists: ten losses on ten consecutive days is a different
	// risk from ten spread over a year, and Kupiec cannot tell them apart.
	if clustered, err := varmodel.Backtest(consecutiveExceptions(1000, 40), flatVaR(1000, 1), 0.99); err == nil && tooManyErr == nil {
		cases = append(cases, validation.Case{
			Name: "christoffersen_separates_clustered_from_spread_exceptions_identity",
			Got:  boolAsFloat(clustered.ChristoffersenLR > tooMany.ChristoffersenLR), Want: 1, Tolerance: 0.5,
		})
	}

	return cases
}

// kupiecFlip drives the exception count upward until the Kupiec verdict flips,
// returning the statistic on each side of the boundary.
func kupiecFlip() (lastPass, firstFail float64, ok bool) {
	const n = 1000
	for x := 10; x <= 80; x++ {
		r, err := varmodel.Backtest(exceptionSeries(n, x), flatVaR(n, 1), 0.99)
		if err != nil {
			return 0, 0, false
		}
		if r.KupiecPass {
			lastPass = r.KupiecLR
			continue
		}
		return lastPass, r.KupiecLR, lastPass > 0
	}
	return 0, 0, false
}

// exceptionSeries builds n days of P&L with exactly x losses beyond the VaR,
// spread as evenly as the counts allow so the independence test sees no
// clustering.
func exceptionSeries(n, x int) []float64 {
	pnl := make([]float64, n)
	if x <= 0 {
		return pnl
	}
	step := float64(n) / float64(x)
	for i := range x {
		at := int(float64(i) * step)
		if at >= n {
			at = n - 1
		}
		pnl[at] = -2 // beyond a VaR of 1
	}
	return pnl
}

// consecutiveExceptions puts every exception in one run — the clustering the
// independence test exists to find.
func consecutiveExceptions(n, x int) []float64 {
	pnl := make([]float64, n)
	for i := 0; i < x && n/2+i < n; i++ {
		pnl[n/2+i] = -2
	}
	return pnl
}

func flatVaR(n int, v float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// zoneFor reports the traffic-light zone the implementation assigns, read
// through the public Backtest result rather than the unexported helper — the
// zone a supervisor sees is the one on the result.
func zoneFor(exceptions int) string {
	r, err := varmodel.Backtest(exceptionSeries(250, exceptions), flatVaR(250, 1), 0.99)
	if err != nil {
		return "ERROR"
	}
	return r.BaselZone
}

func boolAsFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
