package compute_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/marketdata/returns"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
)

// volStubProvider returns a fixed series for every instrument and records the
// asOf it was queried with (point-in-time check).
type volStubProvider struct {
	series  []float64
	gotAsOf time.Time
	err     error
}

func (p *volStubProvider) Returns(_ context.Context, _ string, asOf time.Time, _ int) ([]float64, error) {
	p.gotAsOf = asOf
	return p.series, p.err
}

// expectedStdDev mirrors the model's unbiased (n-1) sample stddev so the test
// asserts against an independently-computed expectation, not a magic number.
func expectedStdDev(xs []float64) float64 {
	n := float64(len(xs))
	var mean float64
	for _, x := range xs {
		mean += x
	}
	mean /= n
	var ss float64
	for _, x := range xs {
		ss += (x - mean) * (x - mean)
	}
	return math.Sqrt(ss / (n - 1))
}

// ramp builds a deterministic n-length return series with non-trivial spread.
func ramp(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = float64(i%7-3) * 0.01 // -0.03..0.03, varied
	}
	return out
}

// TestReturnsVolModel_* moved with the model to internal/marketdata/returns
// (DEBT-ARCH-01). The tests below keep the risk-side contract: that a
// returns.ReturnsVolModel (satisfying compute.VolModel structurally) drives
// PopulateUncertainty's per-position band and RISK-08 propagation.

func TestPopulateUncertainty_DeltaNormalBand(t *testing.T) {
	series := ramp(40)
	sigma := expectedStdDev(series)
	prov := &volStubProvider{series: series}
	vm := returns.NewReturnsVolModel(prov, 0)

	p := makePortfolio("PORT-1", "USD",
		domain.Position{InstrumentID: "A", MarketValue: mkMoneyUnc(-200, 0, "USD"), AsOf: baseTime},
	)
	compute.PopulateUncertainty(context.Background(), p, vm)

	pos, _ := p.Position("A")
	if pos.MarketValueUncertainty == nil {
		t.Fatal("band not populated")
	}
	// |MV| × σ, the delta-normal one-sigma band; |MV| = 200.
	if want := 200 * sigma; math.Abs(decValue(pos.MarketValueUncertainty.Amount)-want) > uncertaintyEpsilon {
		t.Errorf("band = %v want %v", decValue(pos.MarketValueUncertainty.Amount), want)
	}
	if pos.MarketValueUncertainty.CurrencyCode != "USD" {
		t.Errorf("band ccy = %q want USD", pos.MarketValueUncertainty.CurrencyCode)
	}
	// Point-in-time: estimated as of the portfolio's state time.
	if !prov.gotAsOf.Equal(baseTime) {
		t.Errorf("provider asOf = %v want %v", prov.gotAsOf, baseTime)
	}
}

func TestPopulateUncertainty_FlowsToMeasurePropagation(t *testing.T) {
	// End-to-end: the populated band feeds GrossExposure's RISK-08 propagation.
	series := ramp(40)
	sigma := expectedStdDev(series)
	vm := returns.NewReturnsVolModel(&volStubProvider{series: series}, 0)

	p := makePortfolio("PORT-1", "USD",
		domain.Position{InstrumentID: "A", MarketValue: mkMoneyUnc(100, 0, "USD"), AsOf: baseTime},
		domain.Position{InstrumentID: "B", MarketValue: mkMoneyUnc(300, 0, "USD"), AsOf: baseTime},
	)
	compute.PopulateUncertainty(context.Background(), p, vm)

	// √((100σ)² + (300σ)²) = σ·√(100²+300²).
	want := sigma * math.Sqrt(100*100+300*300)
	got := compute.GrossExposure(p)
	if math.Abs(decValue(got.UncertaintyAbs)-want) > uncertaintyEpsilon {
		t.Errorf("propagated σ = %v want %v", decValue(got.UncertaintyAbs), want)
	}
}

func TestPopulateUncertainty_NilModelIsNoOp(t *testing.T) {
	p := makePortfolio("PORT-1", "USD",
		domain.Position{InstrumentID: "A", MarketValue: mkMoneyUnc(100, 0, "USD"), AsOf: baseTime},
	)
	compute.PopulateUncertainty(context.Background(), p, nil)
	if pos, _ := p.Position("A"); pos.MarketValueUncertainty != nil {
		t.Errorf("nil model populated a band: %v", pos.MarketValueUncertainty)
	}
}

func TestPopulateUncertainty_InsufficientHistoryLeavesNil(t *testing.T) {
	vm := returns.NewReturnsVolModel(&volStubProvider{series: ramp(3)}, 0)
	p := makePortfolio("PORT-1", "USD",
		domain.Position{InstrumentID: "A", MarketValue: mkMoneyUnc(100, 0, "USD"), AsOf: baseTime},
	)
	compute.PopulateUncertainty(context.Background(), p, vm)
	if pos, _ := p.Position("A"); pos.MarketValueUncertainty != nil {
		t.Errorf("short history populated a band: %v", pos.MarketValueUncertainty)
	}
}
