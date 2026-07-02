package performance

import (
	"errors"
	"math"
	"testing"
)

// A clean book reconciles with zero breaks: every period's NAV movement is
// exactly flows + attributed P&L.
func TestReconcileNAV_CleanBook(t *testing.T) {
	navs := []float64{1000, 1100, 1080, 1230}
	flows := []float64{50, -40, 100}
	pnl := []float64{50, 20, 50}
	breaks, err := ReconcileNAV(navs, flows, pnl, 0.01)
	if err != nil {
		t.Fatalf("ReconcileNAV: %v", err)
	}
	if len(breaks) != 0 {
		t.Errorf("clean book must have no breaks, got %+v", breaks)
	}
}

// An unexplained movement surfaces as a named break with the exact residual —
// never silently plugged.
func TestReconcileNAV_SurfacesBreak(t *testing.T) {
	navs := []float64{1000, 1100, 1085}
	flows := []float64{50, 0}
	pnl := []float64{50, -20} // period 1 explains only −20 of the −15 move: residual +5
	breaks, err := ReconcileNAV(navs, flows, pnl, 0.01)
	if err != nil {
		t.Fatalf("ReconcileNAV: %v", err)
	}
	if len(breaks) != 1 {
		t.Fatalf("want exactly one break, got %+v", breaks)
	}
	b := breaks[0]
	if b.Period != 1 || math.Abs(b.Residual-5) > 1e-9 || b.Actual != 1085 || math.Abs(b.Expected-1080) > 1e-9 {
		t.Errorf("break details wrong: %+v", b)
	}
}

// Residuals within tolerance (rounding, accrual timing) are not breaks.
func TestReconcileNAV_ToleranceAbsorbsRounding(t *testing.T) {
	navs := []float64{1000, 1050.004}
	flows := []float64{0}
	pnl := []float64{50}
	breaks, err := ReconcileNAV(navs, flows, pnl, 0.01)
	if err != nil {
		t.Fatalf("ReconcileNAV: %v", err)
	}
	if len(breaks) != 0 {
		t.Errorf("sub-tolerance residual must not break: %+v", breaks)
	}
}

func TestReconcileNAV_Errors(t *testing.T) {
	cases := map[string]func() error{
		"one mark":     func() error { _, err := ReconcileNAV([]float64{1000}, nil, nil, 0); return err },
		"misaligned":   func() error { _, err := ReconcileNAV([]float64{1, 2, 3}, []float64{0}, []float64{1, 1}, 0); return err },
		"negative tol": func() error { _, err := ReconcileNAV([]float64{1, 2}, []float64{0}, []float64{1}, -1); return err },
	}
	for name, call := range cases {
		if err := call(); !errors.Is(err, ErrNAVReconcile) {
			t.Errorf("%s: want ErrNAVReconcile, got %v", name, err)
		}
	}
}
