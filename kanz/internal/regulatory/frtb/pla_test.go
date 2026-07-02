package frtb

import (
	"errors"
	"math"
	"testing"
)

func seq(n int, f func(i int) float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = f(i)
	}
	return out
}

// A desk whose risk model exactly explains the front-office P&L: Spearman 1,
// KS 0, GREEN.
func TestPLATest_PerfectTrackingIsGreen(t *testing.T) {
	rtpl := seq(100, func(i int) float64 { return math.Sin(float64(i)) * 100 })
	r, err := PLATest(rtpl, rtpl)
	if err != nil {
		t.Fatalf("PLATest: %v", err)
	}
	if r.Spearman != 1 || r.KS != 0 || r.Zone != PLAGreen {
		t.Errorf("perfect tracking: %+v", r)
	}
}

// A constant shift keeps the ranks identical (Spearman green) but moves the
// distribution: shift 10 over a 1..100 range ⇒ KS ≈ 0.10 ⇒ AMBER; shift 15 ⇒
// KS 0.15 ⇒ RED. The desk's zone is the WORSE statistic.
func TestPLATest_DistributionShiftZones(t *testing.T) {
	rtpl := seq(100, func(i int) float64 { return float64(i + 1) })

	amber, err := PLATest(rtpl, seq(100, func(i int) float64 { return float64(i + 11) }))
	if err != nil {
		t.Fatalf("PLATest: %v", err)
	}
	if amber.Spearman != 1 {
		t.Errorf("shift keeps ranks: Spearman %v", amber.Spearman)
	}
	if math.Abs(amber.KS-0.10) > 1e-12 || amber.Zone != PLAAmber {
		t.Errorf("10-unit shift: KS %v zone %s, want 0.10 AMBER", amber.KS, amber.Zone)
	}

	red, err := PLATest(rtpl, seq(100, func(i int) float64 { return float64(i + 16) }))
	if err != nil {
		t.Fatalf("PLATest: %v", err)
	}
	if red.Zone != PLARed {
		t.Errorf("15-unit shift must be RED, got %s (KS %v)", red.Zone, red.KS)
	}
}

// An uncorrelated risk model is RED regardless of distribution match.
func TestPLATest_UncorrelatedIsRed(t *testing.T) {
	rtpl := seq(101, func(i int) float64 { return float64(i) })
	hpl := seq(101, func(i int) float64 { return float64((i * 37) % 101) }) // rank-shuffled
	r, err := PLATest(rtpl, hpl)
	if err != nil {
		t.Fatalf("PLATest: %v", err)
	}
	if r.Spearman > plaCorrRed || r.Zone != PLARed {
		t.Errorf("shuffled desk must be RED: %+v", r)
	}
}

func TestPLATest_Errors(t *testing.T) {
	if _, err := PLATest([]float64{1}, []float64{1}); !errors.Is(err, ErrPLA) {
		t.Error("single observation must error")
	}
	if _, err := PLATest([]float64{1, 2}, []float64{1}); !errors.Is(err, ErrPLA) {
		t.Error("misaligned series must error")
	}
}
