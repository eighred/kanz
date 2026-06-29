package wealth

import (
	"math"
	"math/rand"
	"sort"
)

// Goals-based analytics (WEALTH-01c): the probability a household reaches a goal,
// scored by a Monte-Carlo projection of wealth to the horizon, plus the funding
// gap (the shortfall of today's funding against the goal's present value).
//
// # Reuses the MODEL-01e Monte-Carlo stance, boundary-clean
//
// MODEL-01e draws correlated multivariate-normal scenarios from a fitted
// covariance. A goal projection is the single-asset case of the same idea — a
// geometric-Brownian-motion (lognormal) wealth path — so wealth ships its own
// small GBM projector rather than reaching into the risk module (the RISK-02
// boundary; the same "re-declare the concept as your own" stance optimization
// takes with SampleCovariance). It is deterministic given Seed (EVT-21d), so a
// replayed projection reproduces the same probability.

// Projection parameterizes a goal's Monte-Carlo wealth projection. Returns and
// volatility are annual; the path is stepped one year at a time over Years, with
// AnnualContribution added at the start of each year before the market step.
type Projection struct {
	// InitialWealth is the wealth allocated to the goal today.
	InitialWealth float64
	// AnnualContribution is added at the start of each year (the savings plan).
	AnnualContribution float64
	// ExpectedReturn is the annual arithmetic mean return (μ).
	ExpectedReturn float64
	// Volatility is the annual return standard deviation (σ).
	Volatility float64
	// Years is the projection horizon in whole years.
	Years int
	// Draws is the number of Monte-Carlo paths (≤0 ⇒ DefaultDraws).
	Draws int
	// Seed seeds the RNG for determinism (0 ⇒ DefaultSeed).
	Seed int64
}

// Goal projection defaults, mirroring the MODEL-01e Monte-Carlo defaults.
const (
	DefaultDraws = 10000
	DefaultSeed  = 1
)

// terminalWealths simulates Draws GBM wealth paths to the horizon and returns the
// terminal wealth of each. Each year: add the contribution, then apply a
// lognormal market step exp((μ − σ²/2) + σ·z). The per-path random draws are
// fixed by Seed, so terminal wealth is a deterministic, non-decreasing function
// of InitialWealth and AnnualContribution — the property the funding-monotonicity
// guarantee (WEALTH-01e) rests on.
func (pr Projection) terminalWealths() []float64 {
	draws := pr.Draws
	if draws <= 0 {
		draws = DefaultDraws
	}
	seed := pr.Seed
	if seed == 0 {
		seed = DefaultSeed
	}
	years := pr.Years
	if years < 0 {
		years = 0
	}
	drift := (pr.ExpectedReturn - pr.Volatility*pr.Volatility/2)
	rng := rand.New(rand.NewSource(seed))
	out := make([]float64, draws)
	for d := 0; d < draws; d++ {
		w := pr.InitialWealth
		for y := 0; y < years; y++ {
			w += pr.AnnualContribution
			step := math.Exp(drift + pr.Volatility*rng.NormFloat64())
			w *= step
		}
		out[d] = w
	}
	return out
}

// ProbabilityOfSuccess returns the fraction of Monte-Carlo paths whose terminal
// wealth meets or exceeds target. It is monotone non-decreasing in InitialWealth
// and AnnualContribution (more funding never lowers the success rate) because the
// per-path market steps are fixed by Seed. A non-positive target is certain
// (probability 1).
func (pr Projection) ProbabilityOfSuccess(target float64) float64 {
	if target <= 0 {
		return 1
	}
	w := pr.terminalWealths()
	if len(w) == 0 {
		return 0
	}
	var hits int
	for _, tw := range w {
		if tw >= target {
			hits++
		}
	}
	return float64(hits) / float64(len(w))
}

// MedianOutcome returns the median terminal wealth of the projection — the
// "expected" funded amount an advisor shows alongside the success probability.
func (pr Projection) MedianOutcome() float64 {
	w := pr.terminalWealths()
	if len(w) == 0 {
		return 0
	}
	return quantile(w, 0.5)
}

// FundingGap is the shortfall of today's funding against a goal's present value:
// gap = target·(1+r)^(−years) − currentFunding, discounting the target back at
// the (risk-free or expected) rate r. A non-positive gap (funding already covers
// the discounted target) is reported as 0 — the goal is on track, no shortfall.
func FundingGap(target, currentFunding, rate float64, years int) float64 {
	pv := target
	if rate > -1 && years > 0 {
		pv = target / math.Pow(1+rate, float64(years))
	}
	gap := pv - currentFunding
	if gap < 0 {
		return 0
	}
	return gap
}

// quantile returns the q-quantile (0..1) of xs via linear interpolation on a
// sorted copy. Empty ⇒ 0.
func quantile(xs []float64, q float64) float64 {
	n := len(xs)
	if n == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	if q <= 0 {
		return s[0]
	}
	if q >= 1 {
		return s[n-1]
	}
	pos := q * float64(n-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return s[lo]
	}
	frac := pos - float64(lo)
	return s[lo]*(1-frac) + s[hi]*frac
}
