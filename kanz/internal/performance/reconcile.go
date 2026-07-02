package performance

import (
	"errors"
	"fmt"
	"math"
)

// NAV-attribution reconciliation (PARITY-03i). Attribution that does not add
// back to the book's NAV movement is fiction: over every period the IBOR
// identity NAV_end = NAV_start + net flows + attributed P&L must hold within
// an explainable tolerance, and any residual is a named BREAK to investigate
// (never silently plugged — the no-silent-failure rule). The inputs are the
// live IBOR NAV marks and the attribution's P&L per period; the function is
// pure so it runs identically in the nightly reconciliation job and in tests.

// NAVBreak is one period whose attribution fails to explain the NAV movement.
type NAVBreak struct {
	// Period indexes the reconciled interval (navs[Period] → navs[Period+1]).
	Period int
	// Expected is NAV_start + flows + attributed P&L; Actual is NAV_end.
	Expected, Actual float64
	// Residual is Actual − Expected (the unexplained amount).
	Residual float64
}

// ErrNAVReconcile is returned for unusable reconciliation inputs.
var ErrNAVReconcile = errors.New("performance: invalid NAV reconciliation inputs")

// ReconcileNAV checks the identity over every period: navs has T+1 marks
// (period boundaries), flows[t] and pnl[t] cover period t. It returns the
// breaks exceeding tolerance — an empty slice means the attribution fully
// explains the book. tolerance must be ≥ 0 (money units).
func ReconcileNAV(navs, flows, pnl []float64, tolerance float64) ([]NAVBreak, error) {
	periods := len(navs) - 1
	if periods < 1 {
		return nil, fmt.Errorf("%w: need at least two NAV marks", ErrNAVReconcile)
	}
	if len(flows) != periods || len(pnl) != periods {
		return nil, fmt.Errorf("%w: %d periods need %d flows and pnl entries (got %d, %d)",
			ErrNAVReconcile, periods, periods, len(flows), len(pnl))
	}
	if tolerance < 0 {
		return nil, fmt.Errorf("%w: negative tolerance", ErrNAVReconcile)
	}
	var breaks []NAVBreak
	for t := 0; t < periods; t++ {
		expected := navs[t] + flows[t] + pnl[t]
		if residual := navs[t+1] - expected; math.Abs(residual) > tolerance {
			breaks = append(breaks, NAVBreak{
				Period:   t,
				Expected: expected,
				Actual:   navs[t+1],
				Residual: residual,
			})
		}
	}
	return breaks, nil
}
