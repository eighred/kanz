package varmodel

import (
	"errors"
	"fmt"
	"math"
)

// Production VaR backtesting (PARITY-03i). MODEL-01i proved calibration inside
// a test; this is the PRODUCTION harness the regulatory backtest runs on live
// realized P&L: Kupiec's proportion-of-failures test (are there the right
// NUMBER of exceptions?), Christoffersen's independence test (do exceptions
// CLUSTER? — a clustered model fails in exactly the crisis it exists for),
// their joint conditional-coverage statistic, and the Basel traffic-light
// zone. Inputs are plain P&L and VaR series — the caller reads them off the
// live books (IBOR P&L, the published VaR per day).

// BacktestResult is one backtest window's statistics and decisions.
type BacktestResult struct {
	Observations int
	Exceptions   int
	// KupiecLR is the POF likelihood ratio ~ χ²(1) under correct coverage.
	KupiecLR   float64
	KupiecPass bool
	// ChristoffersenLR is the independence LR ~ χ²(1) under no clustering.
	ChristoffersenLR float64
	IndependencePass bool
	// ConditionalLR is the joint (coverage + independence) LR ~ χ²(2).
	ConditionalLR   float64
	ConditionalPass bool
	// BaselZone is the traffic-light zone from the exception count, per the
	// Basel 250-day/99% table (GREEN ≤4, AMBER 5–9, RED ≥10) — meaningful for
	// the standard 250-observation window.
	BaselZone string
}

// 95% non-rejection thresholds: χ²(1) and χ²(2).
const (
	chi2_1_95 = 3.841459
	chi2_2_95 = 5.991465
)

// ErrBacktest is returned for unusable backtest inputs.
var ErrBacktest = errors.New("varmodel: invalid backtest inputs")

// Backtest runs the exception tests: pnl[t] is the day's realized P&L
// (negative = loss), vars[t] the VaR published FOR that day (a positive loss
// number, the varmodel convention); an exception is pnl[t] < −vars[t].
// confidence is the VaR confidence (e.g. 0.99).
func Backtest(pnl, vars []float64, confidence float64) (BacktestResult, error) {
	n := len(pnl)
	if n == 0 || len(vars) != n {
		return BacktestResult{}, fmt.Errorf("%w: need equal, non-empty pnl and VaR series", ErrBacktest)
	}
	if confidence <= 0 || confidence >= 1 {
		return BacktestResult{}, fmt.Errorf("%w: confidence %.4g outside (0,1)", ErrBacktest, confidence)
	}
	exceptions := make([]bool, n)
	x := 0
	for t := range pnl {
		if pnl[t] < -vars[t] {
			exceptions[t] = true
			x++
		}
	}

	r := BacktestResult{Observations: n, Exceptions: x, BaselZone: baselZone(x)}
	p := 1 - confidence
	r.KupiecLR = kupiecLR(n, x, p)
	r.KupiecPass = r.KupiecLR <= chi2_1_95
	r.ChristoffersenLR = christoffersenLR(exceptions)
	r.IndependencePass = r.ChristoffersenLR <= chi2_1_95
	r.ConditionalLR = r.KupiecLR + r.ChristoffersenLR
	r.ConditionalPass = r.ConditionalLR <= chi2_2_95
	return r, nil
}

// kupiecLR is the POF likelihood ratio:
// −2·[ (n−x)·ln(1−p) + x·ln p − (n−x)·ln(1−π) − x·ln π ], π = x/n, 0·ln0 := 0.
func kupiecLR(n, x int, p float64) float64 {
	pi := float64(x) / float64(n)
	return -2 * (xlny(n-x, 1-p) + xlny(x, p) - xlny(n-x, 1-pi) - xlny(x, pi))
}

// christoffersenLR is the first-order Markov independence LR from the
// exception transition counts. Degenerate windows (no transitions out of one
// state) carry no clustering evidence and score 0.
func christoffersenLR(ex []bool) float64 {
	var n00, n01, n10, n11 int
	for t := 1; t < len(ex); t++ {
		switch {
		case !ex[t-1] && !ex[t]:
			n00++
		case !ex[t-1] && ex[t]:
			n01++
		case ex[t-1] && !ex[t]:
			n10++
		default:
			n11++
		}
	}
	if n01+n11 == 0 || (n00+n01 == 0) || (n10+n11 == 0) {
		return 0
	}
	pi := float64(n01+n11) / float64(n00+n01+n10+n11)
	pi0 := float64(n01) / float64(n00+n01)
	pi1 := float64(n11) / float64(n10+n11)
	null := xlny(n00+n10, 1-pi) + xlny(n01+n11, pi)
	alt := xlny(n00, 1-pi0) + xlny(n01, pi0) + xlny(n10, 1-pi1) + xlny(n11, pi1)
	lr := -2 * (null - alt)
	if lr < 0 {
		lr = 0 // floating-point guard; the LR is non-negative by construction
	}
	return lr
}

// xlny is x·ln(y) with the 0·ln0 := 0 convention.
func xlny(x int, y float64) float64 {
	if x == 0 {
		return 0
	}
	return float64(x) * math.Log(y)
}

func baselZone(exceptions int) string {
	switch {
	case exceptions <= 4:
		return "GREEN"
	case exceptions <= 9:
		return "AMBER"
	default:
		return "RED"
	}
}
