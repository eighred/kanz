package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	risk "github.com/eighred/kanz/internal/risk"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/compute/factor"
	"github.com/eighred/kanz/internal/risk/engine"
	"github.com/eighred/kanz/internal/risk/scenario/library"
	"github.com/eighred/kanz/internal/risk/state"
)

type unknownScenarioShock struct{}

func (unknownScenarioShock) Description() string { return "unknown" }

// A NAMED STRESS SCENARIO WITH NO CLASSIFIER MUST NOT RETURN THE UNSHOCKED BOOK
// (#640).
//
// The engine is built by services/risk-engine with sharding.EngineOptions()
// only, so engine.WithClassifier has never been called in production and the
// classifier is nil on every deployed replica. Every scenario in the library —
// GFC_2008, COVID_2020, and the credit, factor, liquidity and climate stresses —
// is built from library.SectorCurve, which emits v1.SectorShock exclusively. So
// a 2008 replay on a live book came back through the authorized
// POST /v1/portfolios/{id}/scenario route reporting no impact, which is
// byte-identical to the answer for a portfolio holding none of the shocked
// sectors.
//
// These tests are written against the ENGINE rather than the scenario package
// because that is where the decision to answer or refuse is made, and because
// the scenario package's own coverage assertions would still pass if the engine
// chose to ignore them.

// TestEvaluateScenario_NamedScenarioWithNoClassifierIsRefused is the assertion
// the issue names: the response must not be the unshocked book.
//
// IT CHECKS BOTH HALVES. An error alone would be satisfied by any failure, so
// the response is checked to be empty too — a partially-populated response
// carrying real-looking measures beside an error is exactly the shape a caller
// reads past.
func TestEvaluateScenario_NamedScenarioWithNoClassifierIsRefused(t *testing.T) {
	e, s := newEngine() // no engine.WithClassifier — the production posture
	applyPosition(t, s, "PORT-1", "BANK", 1000, time.Now().Add(-1*time.Second))

	// The unshocked answer, for comparison. A -55% financials shock could not
	// possibly leave this equal.
	base, err := e.Measures(context.Background(), v1.MeasuresRequest{PortfolioID: "PORT-1"})
	if err != nil {
		t.Fatalf("Measures: %v", err)
	}
	baseGross, _ := base.Set.Lookup(compute.MeasureGrossExposure)

	resp, err := e.EvaluateScenario(context.Background(), v1.ScenarioRequest{
		PortfolioID: "PORT-1",
		Shocks:      library.GlobalFinancialCrisis2008(),
	})
	if err == nil {
		shocked, ok := resp.Projected.Lookup(compute.MeasureGrossExposure)
		t.Fatalf("GFC_2008 with no classifier returned a result (gross=%v, base=%v, present=%v) — "+
			"every shock in the curve is a SectorShock and none of them could resolve, so this is "+
			"the CURRENT book wearing the scenario's name",
			decValue(shocked.Value), decValue(baseGross.Value), ok)
	}
	if !errors.Is(err, v1.ErrScenarioUnresolvable) {
		t.Fatalf("err=%v want ErrScenarioUnresolvable — the refusal must be one a caller can "+
			"branch on, not an opaque failure", err)
	}
	if resp.Projected != nil {
		t.Errorf("Projected=%v want nil — a refused scenario must carry no measures at all",
			resp.Projected)
	}
	// The reason has to reach the operator: "no classifier is wired" is a
	// composition-root fix, and it is not the same job as loading reference data
	// for a handful of instruments.
	if !strings.Contains(err.Error(), "no_classifier") {
		t.Errorf("err=%q does not name the reason — an operator cannot tell a missing "+
			"classifier from a reference-data gap without it", err)
	}
}

func TestEvaluateScenario_UnknownShockTypeIsRefused(t *testing.T) {
	e, s := newEngine()
	applyPosition(t, s, "PORT-1", "BANK", 1000, time.Now().Add(-1*time.Second))

	resp, err := e.EvaluateScenario(context.Background(), v1.ScenarioRequest{
		PortfolioID: "PORT-1",
		Shocks:      []v1.ScenarioShock{unknownScenarioShock{}},
	})
	if !errors.Is(err, v1.ErrScenarioUnresolvable) {
		t.Fatalf("err=%v want ErrScenarioUnresolvable", err)
	}
	if resp.Projected != nil {
		t.Errorf("Projected=%v want nil — an unknown shock must not return the current book", resp.Projected)
	}
	if !strings.Contains(err.Error(), "unknown_shock_type") {
		t.Errorf("err=%q does not name unknown_shock_type", err)
	}
}

// TestEvaluateScenario_UnclassifiedHoldingIsRefused covers the layer out from
// the nil seam: a classifier IS wired and does not know this holding. Fixing
// only the nil case would mean the day somebody wires a partially-populated
// classifier, the silent pass comes straight back for every instrument missing
// from it.
func TestEvaluateScenario_UnclassifiedHoldingIsRefused(t *testing.T) {
	e, s := newEngineWithClassifier(factor.StaticClassifier{
		"BANK": {Sector: factor.Sector{Taxonomy: "GICS", Code: "40"}},
	})
	applyPosition(t, s, "PORT-1", "BANK", 1000, time.Now().Add(-1*time.Second))
	applyPosition(t, s, "PORT-1", "MYSTERY", 1000, time.Now().Add(-1*time.Second))

	_, err := e.EvaluateScenario(context.Background(), v1.ScenarioRequest{
		PortfolioID: "PORT-1",
		Shocks:      library.GlobalFinancialCrisis2008(),
	})
	if !errors.Is(err, v1.ErrScenarioUnresolvable) {
		t.Fatalf("err=%v want ErrScenarioUnresolvable — MYSTERY's sector is unknown, so whether "+
			"the curve applies to half this book is unknown", err)
	}
	if !strings.Contains(err.Error(), "MYSTERY") {
		t.Errorf("err=%q does not name the holding — the sample is what lets somebody go and "+
			"load the reference data", err)
	}
}

// TestEvaluateScenario_FullyClassifiedBookIsAnswered is the FALSE-REFUSAL arm.
// Without it the refusal above is satisfied by an engine that refuses every
// scenario, which would be a worse outage than the defect.
func TestEvaluateScenario_FullyClassifiedBookIsAnswered(t *testing.T) {
	e, s := newEngineWithClassifier(factor.StaticClassifier{
		"BANK": {Sector: factor.Sector{Taxonomy: "GICS", Code: "40"}},
	})
	applyPosition(t, s, "PORT-1", "BANK", 1000, time.Now().Add(-1*time.Second))

	base, _ := e.Measures(context.Background(), v1.MeasuresRequest{PortfolioID: "PORT-1"})
	baseGross, _ := base.Set.Lookup(compute.MeasureGrossExposure)

	resp, err := e.EvaluateScenario(context.Background(), v1.ScenarioRequest{
		PortfolioID: "PORT-1",
		Shocks:      library.GlobalFinancialCrisis2008(),
	})
	if err != nil {
		t.Fatalf("EvaluateScenario: %v — a book the classifier fully covers must be answered", err)
	}
	shocked, ok := resp.Projected.Lookup(compute.MeasureGrossExposure)
	if !ok {
		t.Fatal("GrossExposure missing from the projection")
	}
	// Financials -55%: the answer must actually move, or the shock did not land
	// and the refusal is merely being skipped rather than satisfied.
	if decValue(shocked.Value) >= decValue(baseGross.Value) {
		t.Errorf("shocked gross %v not below base %v — the financials shock did not land",
			decValue(shocked.Value), decValue(baseGross.Value))
	}
}

// TestEvaluateScenario_PriceOnlyScenarioNeedsNoClassifier is the second
// false-refusal arm, and the one that matters operationally: the platform's
// price and parallel-shift scenarios consult no classifier, so they must keep
// working on a deployment that has none — which is every deployment.
func TestEvaluateScenario_PriceOnlyScenarioNeedsNoClassifier(t *testing.T) {
	e, s := newEngine() // no classifier
	applyPosition(t, s, "PORT-1", "BANK", 1000, time.Now().Add(-1*time.Second))

	if _, err := e.EvaluateScenario(context.Background(), v1.ScenarioRequest{
		PortfolioID: "PORT-1",
		Shocks: []v1.ScenarioShock{
			v1.ParallelShift{Pct: &commonpb.Decimal{Coefficient: -5, Exponent: -1}},
			v1.PriceShock{InstrumentID: "BANK", Pct: &commonpb.Decimal{Coefficient: -1, Exponent: -1}},
		},
	}); err != nil {
		t.Fatalf("EvaluateScenario: %v — no shock here resolves a sector, so the missing "+
			"classifier is irrelevant and refusing would be a self-inflicted outage", err)
	}
}

// newEngineWithClassifier mirrors newEngine (engine_test.go) with the MODEL-01f
// seam wired. Production has no caller for engine.WithClassifier, so this is the
// only place the wired path is exercised at all.
func newEngineWithClassifier(c factor.Classifier) (*engine.EngineImpl, *state.Store) {
	s := state.NewStore()
	return engine.New(s, compute.DefaultRegistry(), risk.NewCache(), risk.NewDetector(),
		engine.WithClassifier(c)), s
}
