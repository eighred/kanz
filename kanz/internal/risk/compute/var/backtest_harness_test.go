package varmodel

import (
	"errors"
	"math"
	"math/rand"
	"testing"
)

// A correctly calibrated 99% VaR over N(0,σ) P&L: ~1% exceptions, all tests
// pass, Basel GREEN.
func TestBacktest_CalibratedModelPasses(t *testing.T) {
	const n, sigma = 250, 0.01
	rng := rand.New(rand.NewSource(7))
	pnl := make([]float64, n)
	vars := make([]float64, n)
	for i := range pnl {
		pnl[i] = rng.NormFloat64() * sigma
		vars[i] = 2.3263478740408408 * sigma // z(0.99)·σ
	}
	r, err := Backtest(pnl, vars, 0.99)
	if err != nil {
		t.Fatalf("Backtest: %v", err)
	}
	if !r.KupiecPass || !r.IndependencePass || !r.ConditionalPass {
		t.Errorf("calibrated model must pass all tests: %+v", r)
	}
	if r.BaselZone != "GREEN" {
		t.Errorf("calibrated model must be GREEN, got %s (%d exceptions)", r.BaselZone, r.Exceptions)
	}
}

// A VaR published at half its true level takes ~4-5x the exceptions: Kupiec
// must reject and the zone must leave green.
func TestBacktest_UnderestimatedVaRFailsKupiec(t *testing.T) {
	const n, sigma = 250, 0.01
	rng := rand.New(rand.NewSource(7))
	pnl := make([]float64, n)
	vars := make([]float64, n)
	for i := range pnl {
		pnl[i] = rng.NormFloat64() * sigma
		vars[i] = 2.3263478740408408 * sigma / 2
	}
	r, err := Backtest(pnl, vars, 0.99)
	if err != nil {
		t.Fatalf("Backtest: %v", err)
	}
	if r.KupiecPass {
		t.Errorf("halved VaR must fail Kupiec: %+v", r)
	}
	if r.BaselZone == "GREEN" {
		t.Errorf("halved VaR must not stay GREEN: %d exceptions", r.Exceptions)
	}
}

// Same exception COUNT, different pattern: ten clustered exceptions must fail
// independence while ten evenly spread ones pass it — the property Kupiec
// alone cannot see.
func TestBacktest_ChristoffersenSeparatesClustering(t *testing.T) {
	const n = 250
	mk := func(exceptionAt func(int) bool) ([]float64, []float64) {
		pnl := make([]float64, n)
		vars := make([]float64, n)
		for i := range pnl {
			vars[i] = 1
			if exceptionAt(i) {
				pnl[i] = -2
			}
		}
		return pnl, vars
	}

	clusteredPnl, clusteredVars := mk(func(i int) bool { return i >= 100 && i < 110 })
	clustered, err := Backtest(clusteredPnl, clusteredVars, 0.99)
	if err != nil {
		t.Fatalf("Backtest: %v", err)
	}
	spreadPnl, spreadVars := mk(func(i int) bool { return i%25 == 12 })
	spread, err := Backtest(spreadPnl, spreadVars, 0.99)
	if err != nil {
		t.Fatalf("Backtest: %v", err)
	}

	if clustered.Exceptions != spread.Exceptions {
		t.Fatalf("fixture: exception counts differ (%d vs %d)", clustered.Exceptions, spread.Exceptions)
	}
	if clustered.IndependencePass {
		t.Errorf("a 10-day exception run must fail independence: LR=%v", clustered.ChristoffersenLR)
	}
	if !spread.IndependencePass {
		t.Errorf("evenly spread exceptions must pass independence: LR=%v", spread.ChristoffersenLR)
	}
	if clustered.BaselZone != "RED" {
		t.Errorf("10 exceptions is Basel RED, got %s", clustered.BaselZone)
	}
}

// Zero exceptions is a degenerate-but-legal window: LRs finite, tests pass.
func TestBacktest_NoExceptions(t *testing.T) {
	pnl := make([]float64, 100)
	vars := make([]float64, 100)
	for i := range vars {
		vars[i] = 1
	}
	r, err := Backtest(pnl, vars, 0.99)
	if err != nil {
		t.Fatalf("Backtest: %v", err)
	}
	if math.IsNaN(r.KupiecLR) || math.IsInf(r.KupiecLR, 0) {
		t.Errorf("zero-exception Kupiec LR must be finite, got %v", r.KupiecLR)
	}
	if !r.IndependencePass || r.ChristoffersenLR != 0 {
		t.Errorf("no exceptions ⇒ no clustering evidence: %+v", r)
	}
}

func TestBacktest_Errors(t *testing.T) {
	ok := []float64{0, 0}
	cases := map[string]func() error{
		"empty":          func() error { _, err := Backtest(nil, nil, 0.99); return err },
		"misaligned":     func() error { _, err := Backtest(ok, ok[:1], 0.99); return err },
		"bad confidence": func() error { _, err := Backtest(ok, ok, 1.0); return err },
	}
	for name, call := range cases {
		if err := call(); !errors.Is(err, ErrBacktest) {
			t.Errorf("%s: want ErrBacktest, got %v", name, err)
		}
	}
}
