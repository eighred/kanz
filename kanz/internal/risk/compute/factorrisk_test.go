package compute

import (
	"context"
	decutil "github.com/eighred/kanz/internal/dec"
	"math"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/factormodel"
)

type fmReturns map[string][]float64

func (m fmReturns) Returns(_ context.Context, id string, _ time.Time, _ int) ([]float64, error) {
	return m[id], nil
}

type staticModel struct{ m *factormodel.Model }

func (s staticModel) Model(context.Context, time.Time) (*factormodel.Model, bool) {
	return s.m, s.m != nil
}

func factorTestModel(t *testing.T) *factormodel.Model {
	t.Helper()
	rp := fmReturns{
		"A": {0.010, -0.020, 0.015, 0.000, -0.010, 0.020},
		"B": {0.012, -0.018, 0.013, 0.002, -0.011, 0.019},
		"C": {-0.005, 0.010, -0.008, 0.004, 0.012, -0.015},
	}
	m, err := factormodel.Fit(context.Background(), factormodel.Config{Type: factormodel.Statistical, StatFactors: 3},
		[]string{"A", "B", "C"}, time.Now(), factormodel.Providers{Returns: rp})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// factorTestModelSplit fits ONE statistical factor over the same returns, so the
// model leaves a residual and SpecificRisk is non-zero.
//
// factorTestModel asks for three factors over three instruments: the PCA then
// explains the covariance exactly and every specific variance is zero. A test
// asserting "an uncovered position contributes nothing to the specific half"
// against that model asserts 0 == 0 and would pass with the specific half
// deleted. Separate fixture rather than an edit to the shared one, so the
// measure-consistency test above keeps the inputs it was written for.
func factorTestModelSplit(t *testing.T) *factormodel.Model {
	t.Helper()
	rp := fmReturns{
		"A": {0.010, -0.020, 0.015, 0.000, -0.010, 0.020},
		"B": {0.012, -0.018, 0.013, 0.002, -0.011, 0.019},
		"C": {-0.005, 0.010, -0.008, 0.004, 0.012, -0.015},
	}
	m, err := factormodel.Fit(context.Background(), factormodel.Config{Type: factormodel.Statistical, StatFactors: 1},
		[]string{"A", "B", "C"}, time.Now(), factormodel.Providers{Returns: rp})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func factorTestPortfolio() *domain.Portfolio {
	p := domain.NewPortfolio("p1", "USD")
	usd := func(a int64) *commonpb.Money { return &commonpb.Money{Amount: dec(a, 0), CurrencyCode: "USD"} }
	p.SetPosition(domain.Position{InstrumentID: "A", Quantity: dec(100, 0), MarketValue: usd(100_000)})
	p.SetPosition(domain.Position{InstrumentID: "B", Quantity: dec(-50, 0), MarketValue: usd(-50_000)})
	p.SetPosition(domain.Position{InstrumentID: "C", Quantity: dec(30, 0), MarketValue: usd(30_000)})
	return p
}

func TestRegisterFactorRisk_VaRConsistentWithSplit(t *testing.T) {
	r := DefaultRegistry()
	RegisterFactorRisk(context.Background(), r, FactorProviders{Model: staticModel{m: factorTestModel(t)}})
	ms := ComputeMeasures(factorTestPortfolio(), r, nil)

	sys, _ := ms.Lookup(MeasureSystematicRisk)
	spec, _ := ms.Lookup(MeasureSpecificRisk)
	fvar, ok := ms.Lookup(MeasureFactorVaR99)
	if !ok {
		t.Fatal("FactorVaR99 missing")
	}
	total := math.Hypot(decutil.Float64Or(sys.Value, 0), decutil.Float64Or(spec.Value, 0))
	if total <= 0 {
		t.Fatalf("total factor risk must be positive, got %.4f", total)
	}
	// FactorVaR99 = z₀.₉₉ · total, z₀.₉₉ ≈ 2.3263.
	wantVaR := 2.3263 * total
	if d := math.Abs(decutil.Float64Or(fvar.Value, 0) - wantVaR); d > 0.01*wantVaR {
		t.Fatalf("FactorVaR99 %.2f must be ≈ 2.3263·total %.2f", decutil.Float64Or(fvar.Value, 0), wantVaR)
	}
}

func TestRegisterFactorRisk_NoModelIsZero(t *testing.T) {
	r := DefaultRegistry()
	RegisterFactorRisk(context.Background(), r, FactorProviders{Model: staticModel{m: nil}})
	ms := ComputeMeasures(factorTestPortfolio(), r, nil)
	fvar, _ := ms.Lookup(MeasureFactorVaR99)
	if decutil.Float64Or(fvar.Value, 0) != 0 {
		t.Fatalf("FactorVaR99 with no model must be 0, got %.4f", decutil.Float64Or(fvar.Value, 0))
	}
}

// A MISSING MODEL IS REPORTED, NOT SILENTLY ZERO (#509 shape, one layer up).
//
// The test above pins the zero; this one pins that the zero says so. Every path
// that produces it — an empty universe, a failed fit, too little return history
// — produces it identically, and a FactorVaR99 of zero is indistinguishable from
// a portfolio with no factor risk. Here the whole book's factor risk vanishes at
// once rather than one bond's DV01.
func TestRegisterFactorRisk_NoModelIsReported(t *testing.T) {
	var skipped []string
	providers := FactorProviders{
		Model:  staticModel{m: nil}, // the universe was empty / the fit failed
		OnSkip: func(id, reason string) { skipped = append(skipped, id+":"+reason) },
	}
	r := DefaultRegistry()
	RegisterFactorRisk(context.Background(), r, providers)

	ms := ComputeMeasures(factorTestPortfolio(), r, nil)
	fvar, _ := ms.Lookup(MeasureFactorVaR99)
	if got := decutil.Float64Or(fvar.Value, 0); got != 0 {
		t.Fatalf("FactorVaR99 = %.4f with no model, want 0 — the fixture is not exercising the skip", got)
	}
	if len(skipped) == 0 {
		t.Fatal("a book with three positions reported zero factor risk and nothing said why — " +
			"that reads exactly like a portfolio with no factor risk")
	}
	want := ":" + SkipNoModel // empty instrument: the evaluation, not a position, is what failed
	for _, s := range skipped {
		if s != want {
			t.Errorf("reported %q, want %q — the reason set is closed, and no model is a property "+
				"of the (portfolio, asOf) evaluation with no instrument to attribute it to", s, want)
		}
	}
	// ONCE PER MEASURE, NOT ONCE PER PORTFOLIO. Three factor measures share one
	// path, so one missing model reports three times. This is pinned so the fan-out
	// is a documented property rather than a surprise in a counter: a caller may
	// build "measure evaluations with no model" from this and may NOT build
	// "portfolios affected".
	if len(skipped) != 3 {
		t.Errorf("one missing model produced %d reports, want 3 (one per factor measure): %v",
			len(skipped), skipped)
	}
}

// A MODEL THAT COVERS THE BOOK IS NOT REPORTED, or the signal is useless.
//
// Without this the test above is satisfied by an observer that fires on every
// evaluation, which would make the counter constant and the alert unbuildable.
func TestRegisterFactorRisk_AnAvailableModelIsNotReportedAsSkipped(t *testing.T) {
	var skipped []string
	providers := FactorProviders{
		Model:  staticModel{m: factorTestModel(t)}, // universe A, B, C — the whole book
		OnSkip: func(id, reason string) { skipped = append(skipped, id+":"+reason) },
	}
	r := DefaultRegistry()
	RegisterFactorRisk(context.Background(), r, providers)

	ms := ComputeMeasures(factorTestPortfolio(), r, nil)
	sys, _ := ms.Lookup(MeasureSystematicRisk)
	if decutil.Float64Or(sys.Value, 0) <= 0 {
		t.Fatal("SystematicRisk is not positive — the fixture is not exercising the covered path")
	}
	if len(skipped) != 0 {
		t.Errorf("a fully covered book reported %d factor skips: %v — a measure that reports on "+
			"every evaluation reports nothing", len(skipped), skipped)
	}
}

// A POSITION OUTSIDE THE MODEL UNIVERSE IS REPORTED — the second hazard, and a
// different one from a missing model.
//
// The model resolves, so the measures look healthy; one holding simply has no
// loadings and no specific variance. It contributes zero to BOTH halves of the
// split (factormodel.FactorExposures skips an unknown id; the specific term
// reads a zero-valued SpecificVar), so the book's measured factor risk shrinks
// by exactly the risk of the holding nobody modelled — low, in the direction
// that makes a limit pass. The unchanged SystematicRisk/SpecificRisk below is
// the proof that the drop is real and not a hypothetical.
func TestRegisterFactorRisk_APositionOutsideTheUniverseIsReported(t *testing.T) {
	var skipped []string
	providers := FactorProviders{
		Model:  staticModel{m: factorTestModelSplit(t)}, // universe A, B, C; both halves non-zero
		OnSkip: func(id, reason string) { skipped = append(skipped, id+":"+reason) },
	}
	r := DefaultRegistry()
	RegisterFactorRisk(context.Background(), r, providers)

	covered := ComputeMeasures(factorTestPortfolio(), r, nil)
	skipped = nil // the covered book reports nothing; assert only on what D adds

	p := factorTestPortfolio()
	usd := func(a int64) *commonpb.Money { return &commonpb.Money{Amount: dec(a, 0), CurrencyCode: "USD"} }
	// D: a half-million-dollar holding nobody fitted a loading for.
	p.SetPosition(domain.Position{InstrumentID: "D", Quantity: dec(500, 0), MarketValue: usd(500_000)})
	// E: outside the universe AND outside the base currency. It is excluded one
	// step earlier and already reported as v1.QualityFlagCurrencyExcluded (#257);
	// reporting it here too would count one exclusion under two names.
	p.SetPosition(domain.Position{InstrumentID: "E", Quantity: dec(10, 0),
		MarketValue: &commonpb.Money{Amount: dec(70_000, 0), CurrencyCode: "EUR"}})
	uncovered := ComputeMeasures(p, r, nil)

	if len(skipped) == 0 {
		t.Fatal("a $500k holding outside the factor universe contributed nothing to either half " +
			"of the risk split and nothing was reported — the coverage gap is invisible")
	}
	want := "D:" + SkipNotInModel
	for _, s := range skipped {
		if s != want {
			t.Errorf("reported %q, want %q — a foreign-currency position is excluded by the "+
				"currency rule and must not be counted a second time as a coverage gap", s, want)
		}
	}
	if len(skipped) != 3 {
		t.Errorf("one uncovered position produced %d reports, want 3 (one per factor measure): %v",
			len(skipped), skipped)
	}

	// The hazard itself: adding D changed neither half of the split.
	before, _ := covered.Lookup(MeasureSpecificRisk)
	after, _ := uncovered.Lookup(MeasureSpecificRisk)
	// Vacuity guard: with three statistical factors over three instruments the PCA
	// explains the covariance exactly, every specific variance is zero, and the
	// comparison below is 0 == 0 — it would pass with the specific half deleted.
	if decutil.Float64Or(before.Value, 0) <= 0 {
		t.Fatal("SpecificRisk is zero on the covered book — the fixture no longer exercises the " +
			"specific half and the comparison below asserts nothing")
	}
	if decutil.Float64Or(before.Value, 0) != decutil.Float64Or(after.Value, 0) {
		t.Fatalf("SpecificRisk moved from %.2f to %.2f when D was added — the fixture is not "+
			"exercising the silent drop this reports", decutil.Float64Or(before.Value, 0), decutil.Float64Or(after.Value, 0))
	}
	sysBefore, _ := covered.Lookup(MeasureSystematicRisk)
	sysAfter, _ := uncovered.Lookup(MeasureSystematicRisk)
	if decutil.Float64Or(sysBefore.Value, 0) != decutil.Float64Or(sysAfter.Value, 0) {
		t.Fatalf("SystematicRisk moved from %.2f to %.2f when D was added — the fixture is not "+
			"exercising the silent drop this reports", decutil.Float64Or(sysBefore.Value, 0), decutil.Float64Or(sysAfter.Value, 0))
	}
}
