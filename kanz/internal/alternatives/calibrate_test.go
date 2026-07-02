package alternatives

import (
	"errors"
	"math"
	"testing"
	"time"
)

// A noise-free linear strategy must recover its generating betas exactly and
// R² ≈ 1; the intercept (alpha) must not leak into the mapping.
func TestCalibrateProxy_RecoversBetas(t *testing.T) {
	n := 48
	equity := make([]float64, n)
	size := make([]float64, n)
	strategy := make([]float64, n)
	for i := 0; i < n; i++ {
		equity[i] = 0.01 * math.Sin(float64(i))
		size[i] = 0.008 * math.Cos(2*float64(i))
		strategy[i] = 0.001 + 1.3*equity[i] + 0.4*size[i] // alpha 10bp + loadings
	}
	m, r2, err := CalibrateProxy("buyout", strategy, map[string][]float64{"equity": equity, "size": size})
	if err != nil {
		t.Fatalf("CalibrateProxy: %v", err)
	}
	if math.Abs(m.Betas["equity"]-1.3) > 1e-9 || math.Abs(m.Betas["size"]-0.4) > 1e-9 {
		t.Errorf("betas: got %v want equity=1.3 size=0.4", m.Betas)
	}
	if r2 < 0.999999 {
		t.Errorf("noise-free fit must have R²≈1, got %v", r2)
	}
	if _, ok := m.Betas["alpha"]; ok || len(m.Betas) != 2 {
		t.Errorf("intercept must not leak into the mapping: %v", m.Betas)
	}
}

func TestCalibrateProxy_Errors(t *testing.T) {
	base := []float64{0.01, -0.02, 0.03, 0.01, -0.01, 0.02}
	cases := map[string]struct {
		strategy []float64
		factors  map[string][]float64
	}{
		"no factors":  {base, nil},
		"misaligned":  {base, map[string][]float64{"equity": base[:4]}},
		"too few obs": {base[:3], map[string][]float64{"equity": base[:3], "size": base[:3]}},
		"collinear":   {base, map[string][]float64{"equity": base, "equity2": base}},
	}
	for name, tc := range cases {
		if _, _, err := CalibrateProxy("x", tc.strategy, tc.factors); !errors.Is(err, ErrProxyCalibration) {
			t.Errorf("%s: want ErrProxyCalibration, got %v", name, err)
		}
	}
}

func TestBenchmarkSeries_PointInTimeAndPME(t *testing.T) {
	dates := []time.Time{day(2020, 1, 1), day(2021, 1, 1), day(2022, 1, 1)}
	levels := []float64{100, 120, 150}
	// Deliberately shuffled input — the constructor sorts.
	s, err := NewBenchmarkSeries([]time.Time{dates[2], dates[0], dates[1]}, []float64{150, 100, 120})
	if err != nil {
		t.Fatalf("NewBenchmarkSeries: %v", err)
	}
	if got := s.Level(day(2021, 6, 1)); got != 120 {
		t.Errorf("mid-period level: got %v want 120 (at-or-before)", got)
	}
	if got := s.Level(day(2019, 6, 1)); got != 0 {
		t.Errorf("pre-series level must be 0 (loud PME failure), got %v", got)
	}

	// Live-series PME: call 100 at t0, NAV 150 at valuation — the fund exactly
	// matched the index (100 → 150), so KS-PME = 1.
	p := Replay("C1", []*Event{
		ev("e1", EventCall, "100", day(2020, 1, 1)),
		ev("e2", EventNAVMark, "150", day(2022, 1, 1)),
	})
	pme, err := PME(p, day(2022, 1, 1), s.LevelFunc())
	if err != nil {
		t.Fatalf("PME: %v", err)
	}
	if math.Abs(pme-1.0) > 1e-9 {
		t.Errorf("index-matching fund: PME=%v want 1.0", pme)
	}
	_ = levels

	// A flow before the series start must error, not rescale silently.
	early := Replay("C2", []*Event{
		ev("e1", EventCall, "100", day(2019, 1, 1)),
		ev("e2", EventNAVMark, "150", day(2022, 1, 1)),
	})
	if _, err := PME(early, day(2022, 1, 1), s.LevelFunc()); err == nil {
		t.Error("flow before the benchmark series must fail PME loudly")
	}
}

func TestBenchmarkSeries_Errors(t *testing.T) {
	d := day(2020, 1, 1)
	if _, err := NewBenchmarkSeries(nil, nil); !errors.Is(err, ErrBenchmarkSeries) {
		t.Error("empty series must error")
	}
	if _, err := NewBenchmarkSeries([]time.Time{d}, []float64{-1}); !errors.Is(err, ErrBenchmarkSeries) {
		t.Error("non-positive level must error")
	}
	if _, err := NewBenchmarkSeries([]time.Time{d, d}, []float64{100, 101}); !errors.Is(err, ErrBenchmarkSeries) {
		t.Error("duplicate date must error")
	}
}

func TestVintageBenchmark_QuartilesAndRanking(t *testing.T) {
	peers := []PeerFund{
		{2018, 0.05, 1.2}, {2018, 0.10, 1.5}, {2018, 0.15, 1.8}, {2018, 0.20, 2.1}, {2018, 0.25, 2.4},
		{2020, 0.50, 3.0}, // other vintage — must be excluded
	}
	stats, err := VintageBenchmark(peers, 2018)
	if err != nil {
		t.Fatalf("VintageBenchmark: %v", err)
	}
	if stats.N != 5 {
		t.Fatalf("peer count: got %d want 5 (vintage-mixing?)", stats.N)
	}
	if stats.IRR.Median != 0.15 {
		t.Errorf("median IRR: got %v want 0.15", stats.IRR.Median)
	}
	if stats.IRR.Q1 != 0.10 || stats.IRR.Q3 != 0.20 {
		t.Errorf("IRR quartiles: got %+v want Q1=0.10 Q3=0.20", stats.IRR)
	}
	if q := stats.IRR.Quartile(0.22); q != 1 {
		t.Errorf("0.22 IRR must rank top quartile, got %d", q)
	}
	if q := stats.IRR.Quartile(0.02); q != 4 {
		t.Errorf("0.02 IRR must rank bottom quartile, got %d", q)
	}

	if _, err := VintageBenchmark(peers, 2020); !errors.Is(err, ErrPeerGroup) {
		t.Errorf("1-fund vintage must be too small, got %v", err)
	}
}
