package performance

import (
	"context"
	"errors"
	"strings"
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
	data, err := BucketBySector(context.Background(), cl, time.Now(), port, bench)
	if err != nil {
		t.Fatalf("BucketBySector: %v", err)
	}

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

// TestBucketBySector_NoClassifierIsRefused pins the distinction #640 was filed
// about, in the one Classifier consumer that never had it: a book with holdings
// and no classifier must REFUSE, not come back as a decomposition. Before this,
// every row bucketed as UNCLASSIFIED and the Brinson effects still summed to the
// active return, so the report was indistinguishable from a correctly resolved
// single-sector book — a fabricated answer, not a missing one.
func TestBucketBySector_NoClassifierIsRefused(t *testing.T) {
	port := []WeightedReturn{
		{InstrumentID: "A", Weight: 0.6, Return: 0.10},
		{InstrumentID: "B", Weight: 0.4, Return: 0.05},
	}
	bench := []WeightedReturn{{InstrumentID: "A", Weight: 1.0, Return: 0.08}}

	data, err := BucketBySector(context.Background(), nil, time.Now(), port, bench)
	if !errors.Is(err, ErrNoClassifier) {
		t.Fatalf("err=%v want ErrNoClassifier — with no classifier wired this returned a "+
			"decomposition that reconciled, booking the whole active return to selection inside "+
			"UNCLASSIFIED; a caller cannot tell that from a real single-sector attribution", err)
	}
	if data != nil {
		t.Fatalf("refusal returned %d sector row(s); a refused decomposition must carry no "+
			"partial buckets a caller could render as a result", len(data))
	}
	// The message must say how much book it declined to attribute, so the refusal
	// is actionable without re-running it under a debugger.
	if got := err.Error(); !strings.Contains(got, "2 portfolio and 1 benchmark row(s)") {
		t.Errorf("refusal %q does not name the row counts it declined", got)
	}
}

// TestBucketBySector_NoClassifierAndAnEmptyBookIsNotRefused: with nothing held,
// no holding's sector is in question and there is nothing unclassified to hide —
// the same cut compliance's unresolvedDimension makes on heldPositions. Refusing
// here would make an empty portfolio look like a reference-data outage.
func TestBucketBySector_NoClassifierAndAnEmptyBookIsNotRefused(t *testing.T) {
	data, err := BucketBySector(context.Background(), nil, time.Now(), nil, nil)
	if err != nil {
		t.Fatalf("empty book with no classifier: err=%v want nil", err)
	}
	if len(data) != 0 {
		t.Fatalf("empty book produced %d sector row(s)", len(data))
	}
}
