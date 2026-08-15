package app

// The bridge from the risk engine's own measures to the AI layer's features (AI-M1).
//
// internal/prediction shipped a feature publisher, a resilient inference client and a model
// registry — and had ZERO importers outside its own tests. Nothing computed a feature,
// nothing published one, nothing consumed a prediction. The platform's entire AI capability
// was an exceptionally well-specified contract with no traffic on it.
//
// KANZ_BRAIN's hybrid design says why the bridge belongs HERE: features are computed in GO,
// because the engine holds the source state, and scored in PYTHON, where the model serving
// stack lives. The two halves were built to the same contract and never connected.
//
// The features ARE the risk measures. They are not a parallel set of numbers computed for
// the model's benefit — that is precisely how a feature and the measure it claims to be
// drift apart, and how a model ends up scoring a portfolio nobody else recognises.

import (
	"context"
	"log/slog"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/prediction"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
)

// FeatureSetPortfolioRisk is the ROUTING KEY.
//
// The model registry resolves a model by feature_set_ref, never by name, so a model cannot
// be handed a vector it was not trained on. Changing this string re-points every model on
// the platform; it is a contract, versioned like one.
//
// THAT RESOLUTION IS REAL SINCE #112 AND WAS NOT BEFORE IT. This comment, the registry's
// own package doc and the dark-capability exemption all asserted the property while
// internal/prediction/registry had no FeatureSetRef field at all and its only lookup was
// Get(modelID) — resolution by NAME, the exact thing all three said it prevented.
//
// NOTHING RESOLVES A MODEL IN GO YET, and that is worth stating beside the key rather than
// leaving it to be discovered: this constant is published on the feature vector and consumed
// by the Python scoring worker. The Go-side registry has no producer (platform.model is not
// a declared subject here, and kanz-py's RegistryPublisher is a Protocol with no
// implementation), so nothing on this side can currently answer "which model serves
// portfolio-risk:1". #112 tracks that, and it is a transport-and-producer problem rather
// than the composition-root problem its title suggests.
const FeatureSetPortfolioRisk prediction.FeatureSetRef = "portfolio-risk:1"

// modelInputs are the measures a portfolio-risk model is entitled to see.
//
// It is an explicit list, not "whatever the engine happened to compute". A feature vector
// whose contents vary with the engine's configuration is a vector the model cannot be
// validated against — and MLOPS-01a's promotion gate exists precisely to stop a model
// serving against inputs nobody signed off.
var modelInputs = []v1.MeasureName{"GrossExposure", "VaR99"}

// FeaturesFromMeasures turns one recomputed MeasureSet into a FeatureVector.
//
// It reports false when ANY model input is absent. A partial vector is not a smaller vector
// — it is a HOLE presented as an input. The model refuses to score one (it degrades, and
// says which feature it could not see), but the honest place to stop is here, before a
// half-vector is published as a FACT that every downstream consumer will fold.
func FeaturesFromMeasures(ms *domain.MeasureSet) (prediction.FeatureVector, bool) {
	if ms == nil {
		return prediction.FeatureVector{}, false
	}
	values := make(map[prediction.FeatureName]prediction.FeatureValue, len(modelInputs))
	for _, name := range modelInputs {
		m, ok := ms.Lookup(name)
		if !ok || m.Value == nil {
			return prediction.FeatureVector{}, false
		}
		// Exact decimal → float64 at the model boundary, and ONLY here. The capital paths
		// stay on big.Rat (doubles are banned on them); a feature is an input to a
		// statistical model, which is float-valued by nature. The conversion is confined to
		// this one function so it can never leak back toward the ledger.
		f, _ := dec.FromProto(m.Value).Float64()
		values[prediction.FeatureName(name)] = prediction.Scalar(f)
	}
	return prediction.FeatureVector{
		SubjectID:     prediction.SubjectID(ms.PortfolioID()),
		FeatureSetRef: FeatureSetPortfolioRisk,
		AsOf:          ms.AsOf(),
		Values:        values,
	}, true
}

// FeaturePublisher publishes the engine's features so the model can score them.
type FeaturePublisher interface {
	PublishFeatures(ctx context.Context, fv prediction.FeatureVector) error
}

// PublishFeaturesOn returns a measure observer for engine.WithMeasureObserver.
//
// A publish failure is LOGGED, never propagated. The prediction layer is an OBSERVER of the
// risk engine, not a dependency of it: a model that cannot be scored must never stop the
// engine from computing, publishing and serving the risk numbers the platform actually
// trades on. That asymmetry is the safety boundary of this whole task, and it is enforced
// here rather than asserted in a comment somewhere.
func PublishFeaturesOn(pub FeaturePublisher, logger *slog.Logger) func(context.Context, *domain.MeasureSet) {
	return func(ctx context.Context, ms *domain.MeasureSet) {
		fv, ok := FeaturesFromMeasures(ms)
		if !ok {
			return // a measure the model needs was not computed; publish nothing
		}
		if err := pub.PublishFeatures(ctx, fv); err != nil {
			logger.Warn("feature publish failed — the model will not score this recompute",
				"portfolio", ms.PortfolioID(), "err", err)
		}
	}
}
