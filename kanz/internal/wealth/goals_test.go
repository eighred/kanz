package wealth

import (
	"math"
	"testing"
)

func baseProjection() Projection {
	return Projection{
		InitialWealth:      100000,
		AnnualContribution: 10000,
		ExpectedReturn:     0.06,
		Volatility:         0.12,
		Years:              20,
		Draws:              5000,
		Seed:               42,
	}
}

func TestProbabilityOfSuccess_MonotoneInInitialWealth(t *testing.T) {
	target := 500000.0
	var prev float64
	for i, w0 := range []float64{50000, 100000, 200000, 400000} {
		pr := baseProjection()
		pr.InitialWealth = w0
		p := pr.ProbabilityOfSuccess(target)
		if i > 0 && p < prev-1e-12 {
			t.Errorf("probability dropped as initial wealth rose: w0=%v p=%v prev=%v", w0, p, prev)
		}
		prev = p
	}
}

func TestProbabilityOfSuccess_MonotoneInContribution(t *testing.T) {
	target := 500000.0
	var prev float64
	for i, c := range []float64{0, 5000, 10000, 25000} {
		pr := baseProjection()
		pr.AnnualContribution = c
		p := pr.ProbabilityOfSuccess(target)
		if i > 0 && p < prev-1e-12 {
			t.Errorf("probability dropped as contribution rose: c=%v p=%v prev=%v", c, p, prev)
		}
		prev = p
	}
}

func TestProbabilityOfSuccess_BoundsAndDeterminism(t *testing.T) {
	pr := baseProjection()
	p := pr.ProbabilityOfSuccess(500000)
	if p < 0 || p > 1 {
		t.Fatalf("probability %v out of [0,1]", p)
	}
	// Same seed ⇒ identical result (EVT-21d determinism).
	if p2 := pr.ProbabilityOfSuccess(500000); p != p2 {
		t.Errorf("non-deterministic: %v vs %v", p, p2)
	}
	// A non-positive target is certain.
	if pr.ProbabilityOfSuccess(0) != 1 {
		t.Errorf("zero target should be certain")
	}
	// An astronomically high target is (essentially) unreachable.
	if pr.ProbabilityOfSuccess(1e12) > 0.01 {
		t.Errorf("absurd target should be ~unreachable, got %v", pr.ProbabilityOfSuccess(1e12))
	}
}

func TestFundingGap(t *testing.T) {
	// Target 100k in 10y at 5%: PV = 100000 / 1.05^10 ≈ 61391.
	pv := 100000 / math.Pow(1.05, 10)
	gap := FundingGap(100000, 40000, 0.05, 10)
	if math.Abs(gap-(pv-40000)) > 1e-6 {
		t.Errorf("gap = %v, want %v", gap, pv-40000)
	}
	// Fully funded vs the discounted target ⇒ no gap.
	if g := FundingGap(100000, pv+1, 0.05, 10); g != 0 {
		t.Errorf("over-funded gap = %v, want 0", g)
	}
	// Zero horizon ⇒ no discounting, gap = target − funding.
	if g := FundingGap(100000, 30000, 0.05, 0); math.Abs(g-70000) > 1e-9 {
		t.Errorf("zero-horizon gap = %v, want 70000", g)
	}
}

func TestMedianOutcome_GrowsWithFunding(t *testing.T) {
	lo := baseProjection()
	lo.InitialWealth = 50000
	hi := baseProjection()
	hi.InitialWealth = 200000
	if hi.MedianOutcome() <= lo.MedianOutcome() {
		t.Errorf("median outcome not increasing in funding: lo=%v hi=%v", lo.MedianOutcome(), hi.MedianOutcome())
	}
}
