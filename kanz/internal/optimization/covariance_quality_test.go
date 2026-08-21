package optimization

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// A COVARIANCE THAT COULD NOT BE ESTIMATED MUST NOT REACH A CALLER AS ZERO RISK (#621).
//
// Every expected value below is derived from the arithmetic by hand, never read
// off the implementation. The two-asset diagonal Σ = diag(0.01, 0.04) has
// long-only min-variance weights ∝ 1/varᵢ = [100, 25] ⇒ [0.8, 0.2], and
// wᵀΣw = 0.64·0.01 + 0.04·0.04 = 0.0064 + 0.0016 = 0.008, so √(wᵀΣw) =
// 0.0894427190999916.

const minVarRiskDiag001004 = 0.0894427190999916

// zeroCov is the matrix from the #621 mutation: symmetric, PSD by every test
// this package had, and a claim that no instrument in the universe moves.
func zeroCov(n int) [][]float64 {
	m := make([][]float64, n)
	for i := range m {
		m[i] = make([]float64, n)
	}
	return m
}

func TestZeroCovarianceDoesNotProduceAComputedZeroRisk(t *testing.T) {
	in := MarketInputs{Instruments: []string{"A", "B"}, Covariance: zeroCov(2)}
	res, err := Optimize(in, Objective{Type: MinVariance}, nil)
	if err != nil {
		t.Fatalf("an all-zero Σ is still accepted (it is PSD); Optimize must return weights: %v", err)
	}
	// NON-VACUITY: the call really did produce a portfolio, so the assertion below
	// is about a result and not about an early return.
	if len(res.Weights) != 2 {
		t.Fatalf("expected weights for both instruments, got %v", res.Weights)
	}
	if res.ExpectedRisk != nil {
		t.Fatalf("ExpectedRisk is %v — a covariance of all zeros carries no risk number, and "+
			"reporting √(wᵀΣw)=0 makes an absence of data indistinguishable from a riskless "+
			"book (#621)", *res.ExpectedRisk)
	}
	if res.CovarianceQuality != CovarianceRankDeficient {
		t.Fatalf("quality: got %s want RANK_DEFICIENT — the zero matrix has rank 0 over a "+
			"2-asset universe", res.CovarianceQuality)
	}
	if res.CovarianceQuality.SupportsRisk() {
		t.Fatal("SupportsRisk() is true for RANK_DEFICIENT, so every caller branching on it will " +
			"go on to trust a number that is not there")
	}
}

func TestWellConditionedCovarianceStillReportsRisk(t *testing.T) {
	in := MarketInputs{Instruments: []string{"A", "B"}, Covariance: diag(0.01, 0.04)}
	res, err := Optimize(in, Objective{Type: MinVariance}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.CovarianceQuality != CovarianceFullRank {
		t.Fatalf("quality: got %s want FULL_RANK — Σ has rank 2 and no observation count was "+
			"stated, which is not the same claim as OBSERVED", res.CovarianceQuality)
	}
	if res.ExpectedRisk == nil {
		t.Fatal("a full-rank Σ must still produce a risk number — withholding it here would make " +
			"the fix useless rather than honest")
	}
	approx(t, "ExpectedRisk", *res.ExpectedRisk, minVarRiskDiag001004, 1e-6)
}

func TestStatedObservationsEarnTheObservedQuality(t *testing.T) {
	in := MarketInputs{
		Instruments:  []string{"A", "B"},
		Covariance:   diag(0.01, 0.04),
		Observations: 250,
	}
	res, err := Optimize(in, Objective{Type: MinVariance}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.CovarianceQuality != CovarianceObserved {
		t.Fatalf("quality: got %s want OBSERVED — 250 observations over a 2-asset universe is the "+
			"only state that asserts both full rank and an adequate sample", res.CovarianceQuality)
	}
	if res.ExpectedRisk == nil {
		t.Fatal("OBSERVED must support a risk number")
	}
	approx(t, "ExpectedRisk", *res.ExpectedRisk, minVarRiskDiag001004, 1e-6)
}

// T ≤ n IS REFUSED A RISK NUMBER EVEN WHEN Σ HAPPENS TO BE FULL RANK.
//
// This is the case the rank check alone cannot catch: a shrunk or factor-model Σ
// is invertible while resting on fewer periods than assets. It is the same
// n <= k refusal internal/alternatives.CalibrateProxy already makes.
func TestUnderObservedCovarianceIsDistinguishableFromCollinearity(t *testing.T) {
	in := MarketInputs{
		Instruments:  []string{"A", "B", "C"},
		Covariance:   diag(0.01, 0.04, 0.09), // full rank: three non-zero pivots
		Observations: 3,                      // T = n, so the estimate cannot be independent of the fit
	}
	res, err := Optimize(in, Objective{Type: MinVariance}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rank, rerr := psdRank(in.Covariance); rerr != nil || rank != 3 {
		t.Fatalf("non-vacuity: this Σ must be FULL RANK (got rank %d, err %v), or the test is "+
			"proving the rank rule rather than the observation rule", rank, rerr)
	}
	if res.CovarianceQuality != CovarianceUnderObserved {
		t.Fatalf("quality: got %s want UNDER_OBSERVED for T=3 over a 3-asset universe",
			res.CovarianceQuality)
	}
	if res.ExpectedRisk != nil {
		t.Fatalf("ExpectedRisk is %v for an estimate over as many periods as assets", *res.ExpectedRisk)
	}
}

// UNDER_OBSERVED OUTRANKS RANK_DEFICIENT when both hold, because it names the
// cause. Without the observation count these two inputs are the same matrix, and
// telling them apart is the sentence #621 was filed on.
func TestUnderObservedOutranksRankDeficient(t *testing.T) {
	collinear := [][]float64{{0.04, 0.04}, {0.04, 0.04}} // rank 1, genuinely collinear
	withCount := MarketInputs{Instruments: []string{"A", "B"}, Covariance: collinear, Observations: 2}
	withoutCount := MarketInputs{Instruments: []string{"A", "B"}, Covariance: collinear}

	a, err := Optimize(withCount, Objective{Type: MinVariance}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Optimize(withoutCount, Objective{Type: MinVariance}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.CovarianceQuality != CovarianceUnderObserved {
		t.Fatalf("with T=2 over 2 assets the quality must name the cause: got %s", a.CovarianceQuality)
	}
	if b.CovarianceQuality != CovarianceRankDeficient {
		t.Fatalf("with no observation count only the symptom is knowable: got %s", b.CovarianceQuality)
	}
	if a.CovarianceQuality == b.CovarianceQuality {
		t.Fatal("the same singular Σ reports the same quality with and without an observation " +
			"count — an estimate over too few periods is again indistinguishable from genuine " +
			"collinearity (#621)")
	}
}

func TestNoCovarianceLeavesTheQualityUnchecked(t *testing.T) {
	in := MarketInputs{Instruments: []string{"A", "B"}, ExpectedReturns: []float64{0.05, 0.10}}
	res, err := Optimize(in, Objective{Type: MaxReturn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.CovarianceQuality != CovarianceUnchecked {
		t.Fatalf("no Σ was supplied, so nothing was checked: got %s", res.CovarianceQuality)
	}
	if res.ExpectedRisk != nil {
		t.Fatalf("ExpectedRisk is %v with no covariance at all", *res.ExpectedRisk)
	}
}

func TestNegativeObservationsAreRefused(t *testing.T) {
	in := MarketInputs{Instruments: []string{"A"}, Covariance: diag(0.01), Observations: -1}
	if _, err := Optimize(in, Objective{Type: MinVariance}, nil); !errors.Is(err, ErrNegativeObservations) {
		t.Fatalf("want ErrNegativeObservations, got %v", err)
	}
}

// --- the two silent equal-weight fallbacks ----------------------------------

// A SINGULAR Σ MUST NOT YIELD AN EQUAL-WEIGHT BOOK CALLED THE TANGENCY PORTFOLIO.
//
// maxSharpe seeds at 1/n and used to return that seed whenever solveLinear
// reported Σ singular. With Σ = [[0.04,0.04],[0.04,0.04]] and μ = [0.05, 0.10]
// the elimination fails, and the old code answered [0.5, 0.5] — which is exactly
// what a 2-asset equal-weight portfolio is, so no caller could spot it.
func TestMaxSharpeRefusesInsteadOfReturningTheEqualWeightSeed(t *testing.T) {
	collinear := [][]float64{{0.04, 0.04}, {0.04, 0.04}}
	in := MarketInputs{
		Instruments:     []string{"A", "B"},
		ExpectedReturns: []float64{0.05, 0.10},
		Covariance:      collinear,
	}
	res, err := Optimize(in, Objective{Type: MaxSharpe}, nil)
	if !errors.Is(err, ErrTangencyUndefined) {
		t.Fatalf("want ErrTangencyUndefined, got err=%v weights=%v", err, res.Weights)
	}
	// The refusal must name what it saw, or an operator has a bare error and no
	// way to tell which of the five covariance states produced it.
	if got := err.Error(); !strings.Contains(got, "RANK_DEFICIENT") {
		t.Fatalf("the refusal must name the covariance quality, got %q", got)
	}
	if len(res.Weights) != 0 {
		t.Fatalf("a refused optimization must return no weights, got %v", res.Weights)
	}
}

func TestRiskParityRefusesInsteadOfReturningTheEqualWeightSeed(t *testing.T) {
	in := MarketInputs{Instruments: []string{"A", "B"}, Covariance: zeroCov(2)}
	res, err := Optimize(in, Objective{Type: RiskParity}, nil)
	if !errors.Is(err, ErrRiskParityUndefined) {
		t.Fatalf("want ErrRiskParityUndefined, got err=%v weights=%v", err, res.Weights)
	}
	if len(res.Weights) != 0 {
		t.Fatalf("a refused optimization must return no weights, got %v", res.Weights)
	}
}

// HRP AND MIN-VARIANCE STILL ANSWER on a singular Σ — the refusals above are
// scoped to the two objectives whose answer is undefined, not to every use of a
// degenerate covariance. HRP never inverts Σ, which is the documented reason it
// is in this package.
func TestDegenerateCovarianceStillAllocatesUnderHRPAndMinVariance(t *testing.T) {
	collinear := [][]float64{{0.04, 0.04}, {0.04, 0.04}}
	for _, obj := range []ObjectiveType{HRP, MinVariance} {
		in := MarketInputs{Instruments: []string{"A", "B"}, Covariance: collinear}
		res, err := Optimize(in, Objective{Type: obj}, nil)
		if err != nil {
			t.Fatalf("objective %d must still allocate on a singular Σ: %v", obj, err)
		}
		if len(res.Weights) != 2 {
			t.Fatalf("objective %d returned %v", obj, res.Weights)
		}
		if res.ExpectedRisk != nil {
			t.Fatalf("objective %d reported a risk number from a rank-1 Σ: %v", obj, *res.ExpectedRisk)
		}
		if res.CovarianceQuality != CovarianceRankDeficient {
			t.Fatalf("objective %d: quality %s", obj, res.CovarianceQuality)
		}
	}
}

// --- SampleCovariance --------------------------------------------------------

func TestSampleCovarianceRefusesRatherThanReturningZeros(t *testing.T) {
	cases := []struct {
		name    string
		returns [][]float64
	}{
		{"one observation", [][]float64{{0.01}, {0.02}}},
		{"no observations", [][]float64{{}, {}}},
		{"no series", nil},
		{"ragged series", [][]float64{{0.01, 0.02, 0.03}, {0.01, 0.02}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cov, obs, err := SampleCovariance(tc.returns)
			if !errors.Is(err, ErrCovarianceEstimate) {
				t.Fatalf("want ErrCovarianceEstimate, got cov=%v obs=%d err=%v", cov, obs, err)
			}
			if cov != nil {
				t.Fatalf("a refusal must hand back no matrix; a zero matrix is a covariance "+
					"asserting that nothing in the book moves (#621). Got %v", cov)
			}
			if obs != 0 {
				t.Fatalf("a refusal must report no observation count, got %d", obs)
			}
		})
	}
}

// --- the wire ----------------------------------------------------------------

func TestCovarianceQualityCrossesTheWireAsAName(t *testing.T) {
	for _, q := range []CovarianceQuality{
		CovarianceUnchecked, CovarianceRankDeficient, CovarianceUnderObserved,
		CovarianceFullRank, CovarianceObserved,
	} {
		b, err := json.Marshal(q)
		if err != nil {
			t.Fatalf("marshal %v: %v", int(q), err)
		}
		if b[0] != '"' {
			t.Fatalf("quality %s marshalled as %s — as a number the unchecked state is 0, which "+
				"every JSON client reads as absent, false and fine", q, b)
		}
		var back CovarianceQuality
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("round-trip %s: %v", q, err)
		}
		if back != q {
			t.Fatalf("round-trip %s became %s", q, back)
		}
	}
	if _, err := json.Marshal(CovarianceQuality(99)); err == nil {
		t.Fatal("an unknown quality must not serialize — a proposal whose provenance cannot be " +
			"named must not leave this process")
	}
	var q CovarianceQuality
	if err := json.Unmarshal([]byte(`"probably fine"`), &q); err == nil {
		t.Fatal("an unknown quality name must be refused, not silently read as UNCHECKED")
	}
	if err := json.Unmarshal([]byte(`3`), &q); err == nil {
		t.Fatal("a bare integer must be refused: it is the pre-#621 shape and 0 means UNCHECKED")
	}
}

// --- RiskContributions -------------------------------------------------------

func TestRiskContributionsRefuseWhereTheyAreUndefined(t *testing.T) {
	in := MarketInputs{Instruments: []string{"A", "B"}, Covariance: zeroCov(2)}
	if rc, err := RiskContributions(map[string]float64{"A": 0.5, "B": 0.5}, in); !errors.Is(err, ErrRiskContributions) {
		t.Fatalf("zero total variance ⇒ ErrRiskContributions, got rc=%v err=%v", rc, err)
	}

	// A MISSING WEIGHT IS NOT A ZERO WEIGHT. The map lookup used to answer 0 for an
	// instrument the caller never supplied, so a mismatched pair of arguments
	// produced a plausible-looking answer about a different book.
	good := MarketInputs{Instruments: []string{"A", "B"}, Covariance: diag(0.01, 0.04)}
	rc, err := RiskContributions(map[string]float64{"A": 0.5}, good)
	if !errors.Is(err, ErrRiskContributions) {
		t.Fatalf("an instrument with no weight ⇒ ErrRiskContributions, got rc=%v err=%v", rc, err)
	}
	// NON-VACUITY: the same call with every weight present must succeed, or the
	// assertion above is passing because RiskContributions refuses everything.
	rc, err = RiskContributions(map[string]float64{"A": 0.5, "B": 0.5}, good)
	if err != nil {
		t.Fatalf("a complete weight map over a full-rank Σ must succeed: %v", err)
	}
	// w=[0.5,0.5], Σ=diag(0.01,0.04): Σw = [0.005, 0.02], wᵀΣw = 0.0025 + 0.01 =
	// 0.0125, so RC_A = 0.5·0.005/0.0125 = 0.2 and RC_B = 0.8. Derived from the
	// definition, not from this package.
	approx(t, "RC_A", rc["A"], 0.2, 1e-12)
	approx(t, "RC_B", rc["B"], 0.8, 1e-12)
}
