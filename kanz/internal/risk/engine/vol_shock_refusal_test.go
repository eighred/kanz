package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/scenario/library"
)

// A VOL STRESS THIS DEPLOYMENT CANNOT PRICE MUST NOT COME BACK AS "NO IMPACT"
// (#1035).
//
// #1004 put VolShock on the query.v1 oneof so an external caller could finally
// request a vol stress over POST /v1/portfolios/{id}/scenario. Nothing on the
// reachable path applied one: Engine.EvaluateScenario has a single evaluation
// path (the linear scenario.Evaluate), whose VolShock arm was a comment, and
// the one path where a vol bump means anything — full revaluation — is blocked
// on a calibrated vol surface nothing on this estate carries (#509/#203).
//
// So the request cloned the book, mutated nothing, and answered with
// contributed=0, excluded_count=0 — a CLEAN coverage record, which is
// affirmative evidence that the stress ran. A desk stressing a short-vega book
// by +15 vol points was told gross, net, VaR99 and Delta are unchanged. That is
// #640's answer ("the stress returned the book") reissued under the one shock
// kind that was added specifically so it could be asked for, and it is worse
// than #640 because the number is not unresolvable, it is confidently
// unchanged.
//
// These are engine tests rather than scenario tests for the reason
// scenario_refusal_test.go gives: the engine is where the decision to answer or
// refuse is made, and the scenario package's coverage assertions would still
// pass if the engine chose to ignore them.

// volBump is +15 vol points, the magnitude library.VolSpikeRiskOff uses.
func volBump() *commonpb.Decimal { return &commonpb.Decimal{Coefficient: 15, Exponent: -2} }

// TestEvaluateScenario_VolShockWithNoRevaluerIsRefused is the assertion the
// issue names. It checks the refusal AND the emptiness of the response, because
// an error beside a populated projection is the shape a caller reads past.
func TestEvaluateScenario_VolShockWithNoRevaluerIsRefused(t *testing.T) {
	e, s := newEngine() // no Revaluer — the production posture, and the only one
	applyPosition(t, s, "PORT-1", "BANK", 1000, time.Now().Add(-1*time.Second))

	base, err := e.Measures(context.Background(), v1.MeasuresRequest{PortfolioID: "PORT-1"})
	if err != nil {
		t.Fatalf("Measures: %v", err)
	}
	baseGross, _ := base.Set.Lookup(compute.MeasureGrossExposure)

	resp, err := e.EvaluateScenario(context.Background(), v1.ScenarioRequest{
		PortfolioID: "PORT-1",
		Shocks:      []v1.ScenarioShock{v1.VolShock{AbsBump: volBump()}},
	})
	if err == nil {
		shocked, ok := resp.Projected.Lookup(compute.MeasureGrossExposure)
		t.Fatalf("a +15 vol-point stress was ANSWERED: gross=%v, unshocked gross=%v, present=%v, "+
			"quality flags=%v — no revaluer exists on this estate, so nothing repriced and this "+
			"is the current book wearing a vol stress's name",
			decValue(shocked.Value), decValue(baseGross.Value), ok, resp.QualityFlags)
	}
	if !errors.Is(err, v1.ErrScenarioUnresolvable) {
		t.Fatalf("err=%v want ErrScenarioUnresolvable — the refusal must be one a caller can "+
			"branch on, not an opaque failure", err)
	}
	if resp.Projected != nil {
		t.Errorf("Projected=%v want nil — a refused scenario must carry no measures at all",
			resp.Projected)
	}
	// The reason has to reach the operator, and it is not a reference-data load:
	// no Revaluer is a composition-root gap blocked on an option-premium source
	// (#509). Sending it to the same place as no_classifier would waste the trip.
	if !strings.Contains(err.Error(), "no_revaluer") {
		t.Errorf("err=%q does not name the reason — an operator cannot tell a missing revaluer "+
			"from a reference-data gap without it", err)
	}
}

// TestEvaluateScenario_VolSpikeRiskOffIsRefused is the same defect through the
// curated stress it was written for. VolSpikeRiskOff pairs a −20% spot drop with
// a +15-point vol bump precisely because an option book's convexity and vega
// only both bite together; served on the linear path it degrades to the spot
// leg alone, and a short-vol book is told its worst regime costs it only the
// delta. A PARTIALLY applied scenario is not a partial number — it is a
// different scenario.
func TestEvaluateScenario_VolSpikeRiskOffIsRefused(t *testing.T) {
	e, s := newEngine()
	applyPosition(t, s, "PORT-1", "BANK", 1000, time.Now().Add(-1*time.Second))

	resp, err := e.EvaluateScenario(context.Background(), v1.ScenarioRequest{
		PortfolioID: "PORT-1",
		Shocks:      library.VolSpikeRiskOff(),
	})
	if err == nil {
		shocked, _ := resp.Projected.Lookup(compute.MeasureGrossExposure)
		t.Fatalf("VolSpikeRiskOff was answered: gross=%v — only the ParallelShift landed, so the "+
			"vol half of a vol stress silently became zero", decValue(shocked.Value))
	}
	if !errors.Is(err, v1.ErrScenarioUnresolvable) {
		t.Fatalf("err=%v want ErrScenarioUnresolvable", err)
	}
}

// TestEvaluateScenario_PriceScenarioIsStillAnswered is the FALSE-REFUSAL arm for
// this change specifically: a scenario carrying no VolShock must be unaffected.
// Without it the refusal above is satisfied by an engine that refuses every
// scenario, which is a worse outage than the defect.
func TestEvaluateScenario_PriceScenarioIsStillAnswered(t *testing.T) {
	e, s := newEngine()
	applyPosition(t, s, "PORT-1", "BANK", 1000, time.Now().Add(-1*time.Second))

	base, _ := e.Measures(context.Background(), v1.MeasuresRequest{PortfolioID: "PORT-1"})
	baseGross, _ := base.Set.Lookup(compute.MeasureGrossExposure)

	resp, err := e.EvaluateScenario(context.Background(), v1.ScenarioRequest{
		PortfolioID: "PORT-1",
		Shocks: []v1.ScenarioShock{
			v1.ParallelShift{Pct: &commonpb.Decimal{Coefficient: -5, Exponent: -1}},
		},
	})
	if err != nil {
		t.Fatalf("EvaluateScenario: %v — a price-only scenario needs no revaluer, and refusing "+
			"it would be a self-inflicted outage", err)
	}
	shocked, ok := resp.Projected.Lookup(compute.MeasureGrossExposure)
	if !ok {
		t.Fatal("GrossExposure missing from the projection")
	}
	if decValue(shocked.Value) >= decValue(baseGross.Value) {
		t.Errorf("shocked gross %v not below base %v — the parallel shift did not land",
			decValue(shocked.Value), decValue(baseGross.Value))
	}
}
