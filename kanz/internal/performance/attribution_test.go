package performance

import (
	"context"
	"testing"
	"time"
)

func TestBrinson_EffectsSumToActiveReturn(t *testing.T) {
	sectors := []SectorData{
		{Sector: "GICS:10", PortfolioWeight: 0.6, BenchmarkWeight: 0.5, PortfolioReturn: 0.10, BenchmarkReturn: 0.08},
		{Sector: "GICS:40", PortfolioWeight: 0.4, BenchmarkWeight: 0.5, PortfolioReturn: 0.05, BenchmarkReturn: 0.06},
	}
	effects := Brinson(sectors)
	var sum float64
	for _, e := range effects {
		sum += e.Total()
	}
	// Active = Σwp·Rp − Σwb·Rb = 0.08 − 0.07 = 0.01.
	rp := weightedReturn(sectors, true)
	rb := weightedReturn(sectors, false)
	near(t, "active return", rp-rb, 0.01, 1e-12)
	near(t, "effects sum to active", sum, rp-rb, 1e-12)
}

func TestSingleAttribution_Reconciles(t *testing.T) {
	sectors := []SectorData{
		{Sector: "A", PortfolioWeight: 0.7, BenchmarkWeight: 0.5, PortfolioReturn: 0.12, BenchmarkReturn: 0.09},
		{Sector: "B", PortfolioWeight: 0.3, BenchmarkWeight: 0.5, PortfolioReturn: -0.02, BenchmarkReturn: 0.01},
	}
	res := SingleAttribution(sectors)
	near(t, "totals reconcile", res.TotalAllocation+res.TotalSelection+res.TotalInteraction, res.ActiveReturn, 1e-12)
}

func TestLinkCarino_MultiPeriodSumsToGeometricActive(t *testing.T) {
	// Two periods, single sector "ALL" whose effects equal that period's
	// arithmetic active return (put entirely in Allocation).
	p1 := PeriodBrinson{Returns: PeriodReturn{Portfolio: 0.10, Benchmark: 0.05}, Effects: []SectorEffect{{Sector: "ALL", Allocation: 0.05}}}
	p2 := PeriodBrinson{Returns: PeriodReturn{Portfolio: 0.05, Benchmark: 0.08}, Effects: []SectorEffect{{Sector: "ALL", Allocation: -0.03}}}
	res := LinkCarino([]PeriodBrinson{p1, p2})

	// Geometric active: (1.10·1.05) − (1.05·1.08) = 1.155 − 1.134 = 0.021.
	near(t, "geometric active", res.ActiveReturn, 0.021, 1e-12)
	// Carino linking makes the summed effects equal the geometric active (not the
	// arithmetic 0.05−0.03 = 0.02).
	near(t, "linked effects = geometric active", res.TotalAllocation+res.TotalSelection+res.TotalInteraction, res.ActiveReturn, 1e-12)
}

type staticClassifier map[string]string

func (m staticClassifier) Sector(_ context.Context, id string, _ time.Time) (string, bool) {
	s, ok := m[id]
	return s, ok
}

func TestBucketBySector_AggregatesAndCatchesUnclassified(t *testing.T) {
	cl := staticClassifier{"A": "GICS:10", "B": "GICS:10", "C": "GICS:40"}
	port := []WeightedReturn{
		{InstrumentID: "A", Weight: 0.3, Return: 0.10},
		{InstrumentID: "B", Weight: 0.3, Return: 0.20},
		{InstrumentID: "C", Weight: 0.2, Return: 0.05},
		{InstrumentID: "Z", Weight: 0.2, Return: 0.00}, // unknown ⇒ UNCLASSIFIED
	}
	bench := []WeightedReturn{{InstrumentID: "A", Weight: 1.0, Return: 0.08}}
	data := BucketBySector(context.Background(), cl, time.Now(), port, bench)

	got := map[string]SectorData{}
	for _, d := range data {
		got[d.Sector] = d
	}
	g10 := got["GICS:10"]
	near(t, "GICS:10 weight", g10.PortfolioWeight, 0.6, 1e-12)
	// weighted avg return = (0.3·0.10 + 0.3·0.20)/0.6 = 0.15.
	near(t, "GICS:10 return", g10.PortfolioReturn, 0.15, 1e-12)
	if _, ok := got[UnclassifiedSector]; !ok {
		t.Fatal("unknown instrument must land in UNCLASSIFIED, not be dropped")
	}
	near(t, "unclassified weight", got[UnclassifiedSector].PortfolioWeight, 0.2, 1e-12)
}
