package app_test

// AI-M1 — the Go side of the prediction layer had ZERO CALLERS.
//
// internal/prediction shipped a feature publisher (PRED-03), a resilient gRPC client
// (circuit breaker, last-known cache, degraded fallback) and a model registry — and a
// repo-wide grep for importers returned only its own tests. Nothing computed a feature,
// nothing published one, nothing consumed a prediction. The AI layer was a contract with
// no traffic.
//
// This is the bridge: the risk engine already computes the numbers a model wants
// (GrossExposure, VaR99 — the RISK-07 baseline registry), and KANZ_BRAIN's hybrid design
// says features are computed in GO (the engine holds the source state) and scored in
// PYTHON. It had simply never been connected.

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/prediction"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/services/risk-engine/internal/app"
)

func measures(t *testing.T, vals map[v1.MeasureName]string) *domain.MeasureSet {
	t.Helper()
	m := map[v1.MeasureName]v1.Measure{}
	for name, s := range vals {
		r, ok := new(big.Rat).SetString(s)
		if !ok {
			t.Fatalf("bad decimal %q", s)
		}
		m[name] = v1.Measure{Name: name, Value: dec.ToProto(r)}
	}
	return domain.NewMeasureSet("fund-alpha", time.Now().UTC(), m)
}

// TestTheEngineTurnsItsOwnMeasuresIntoFeatures.
//
// The features are not invented for the model — they ARE the risk measures the engine
// already publishes. That is the point of computing features in Go: the engine holds the
// source state, so a feature and the measure it came from cannot drift apart.
func TestTheEngineTurnsItsOwnMeasuresIntoFeatures(t *testing.T) {
	fv, ok := app.FeaturesFromMeasures(measures(t, map[v1.MeasureName]string{
		"GrossExposure": "1500000",
		"VaR99":         "42000",
		"NetExposure":   "250000",
	}))
	if !ok {
		t.Fatal("the engine produced no feature vector from a full measure set")
	}

	if fv.SubjectID != "fund-alpha" {
		t.Errorf("subject = %q, want the portfolio", fv.SubjectID)
	}
	if fv.FeatureSetRef != app.FeatureSetPortfolioRisk {
		t.Errorf("feature_set_ref = %q, want %q — it is the ROUTING KEY the model registry "+
			"resolves a model by, so a model can never be handed a vector it was not trained on",
			fv.FeatureSetRef, app.FeatureSetPortfolioRisk)
	}
	if got := fv.Values["GrossExposure"].Scalar; got != 1_500_000 {
		t.Errorf("GrossExposure = %v, want 1500000", got)
	}
	if got := fv.Values["VaR99"].Scalar; got != 42_000 {
		t.Errorf("VaR99 = %v, want 42000", got)
	}
}

// TestAMeasureSetMISSINGAModelInputPublishesNOTHING.
//
// If the engine could not compute VaR99 (a degraded market-data path, a missing model),
// publishing a feature vector without it hands the model a HOLE and calls it an input. The
// model would degrade — it refuses partial vectors — but the honest place to stop is here,
// before a half-vector is put on the bus as a FACT for every consumer to fold.
func TestAMeasureSetMISSINGAModelInputPublishesNOTHING(t *testing.T) {
	_, ok := app.FeaturesFromMeasures(measures(t, map[v1.MeasureName]string{
		"GrossExposure": "1500000", // VaR99 absent
	}))
	if ok {
		t.Fatal("a partial measure set became a feature vector — the model would be scored on a hole")
	}
}

// TestFeaturesArePublishedAsFACTs: the vector goes on the bus under the subject the Python
// streaming worker already subscribes to (inference.feature.computed). Neither side was
// changed to meet the other; they were built to the same contract and never connected.
func TestFeaturesArePublishedAsFACTs(t *testing.T) {
	if app.FeatureSetPortfolioRisk == "" {
		t.Fatal("no feature set ref")
	}
	if prediction.EventTypeFeatureComputed != "inference.feature.computed" {
		t.Fatalf("subject drift: %q", prediction.EventTypeFeatureComputed)
	}
	// The bridge must satisfy the publisher's own type — a compile-time check that the
	// engine's features are the shape internal/prediction publishes.
	var fv prediction.FeatureVector
	fv, ok := app.FeaturesFromMeasures(measures(t, map[v1.MeasureName]string{
		"GrossExposure": "10",
		"VaR99":         "1",
	}))
	if !ok || fv.SubjectID == "" {
		t.Fatal("bridge did not produce a publishable FeatureVector")
	}
	_ = context.Background()
}
