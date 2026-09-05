package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/factormodel"
	"github.com/eighred/kanz/internal/risk/pricing/curve"
	"github.com/eighred/kanz/internal/risk/publish"
)

// THE COMPOSITION ROOT'S RECORD OF WHAT THE ENGINE PRICED WITH (#1039).
//
// # Why this file exists at all
//
// The calibration and the fit happen deep inside the risk module, which has no
// bus. Both seams — curve.Calibrator.OnCalibrated and compute.WithFitObserver —
// hand the artifact to whoever wired them, and this is the only place that holds
// both the artifact seams and the producer. It is the same closure-injection
// stance every other data seam in this service takes.
//
// # Why the counters are registered here, unconditionally
//
// Both artifacts are produced behind configuration branches: the factor fit only
// runs when RISK_ENGINE_MARKETDATA_DATABASE_URL is set, and the curve
// calibration only when RISK_ENGINE_CALIBRATION_INTERVAL and
// RISK_ENGINE_CALIBRATION_RATES both are. NEITHER IS SET BY ANY MANIFEST IN
// infra/. A collector registered inside those branches would export no series at
// all in exactly the deployment that records nothing, so an alert written over
// it — "this pod has recorded no model artifact" — would be silent in the one
// state it exists to detect. That has shipped twice here (#973, #963), so the
// vector is registered before every branch and every artifact label is seeded to
// zero.
//
// A counter at zero and a counter absent are different answers, and only the
// first one says "checked, and nothing".
type modelRecorder struct {
	publisher *publish.Publisher
	logger    *slog.Logger
	recorded  *prometheus.CounterVec
	failed    *prometheus.CounterVec
}

// Artifact labels. A closed set — they are metric labels, so they must not be
// whatever a future edit writes.
const (
	artifactFactorModel = "factor_model"
	artifactCurve       = "curve"
)

// newModelRecorder registers the counters and returns the recorder. reg and
// publisher are required; a nil publisher would make every observer a silent
// no-op, which is the state this change exists to end, so it is refused by the
// caller rather than absorbed here.
func newModelRecorder(publisher *publish.Publisher, logger *slog.Logger, reg prometheus.Registerer) *modelRecorder {
	recorded := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kanz_risk_model_artifact_recorded_total",
		Help: "Model artifacts published as FACTs, by artifact. factor_model = one fitted " +
			"factor-model instance (loadings, covariance, specific variance) on " +
			"risk.factor.model_fitted; curve = one calibrated discount curve on " +
			"risk.curve.calibrated. ZERO MEANS NOTHING THIS POD PRICED WITH CAN BE " +
			"REPRODUCED: the published risk number outlives its inputs, which is the " +
			"state #1039 records.",
	}, []string{"artifact"})
	failed := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kanz_risk_model_artifact_record_failed_total",
		Help: "Model-artifact publishes that failed, by artifact. The artifact was still USED " +
			"to price the book — the recompute is not failed for it — so a rising count is a " +
			"book being priced by a model instance that no longer exists anywhere (#1039).",
	}, []string{"artifact"})
	reg.MustRegister(recorded, failed)
	for _, a := range []string{artifactFactorModel, artifactCurve} {
		recorded.WithLabelValues(a).Add(0)
		failed.WithLabelValues(a).Add(0)
	}
	return &modelRecorder{publisher: publisher, logger: logger, recorded: recorded, failed: failed}
}

// FitOptions is how the live factor-model provider is configured to produce a
// CITABLE model rather than a transient one, kept here rather than at the call
// site because the reasoning is this file's subject and runEngine is a ratchet.
//
// DAILY, AND THE CADENCE IS THE LOAD-BEARING HALF. The fit used to run per
// evaluation: model_id + as_of keyed a different instance for every event the
// book received, so "the model that priced this" named a fit that existed for
// the duration of one call and the artifact topic would have carried one model
// per recompute. compute.DefaultFitCadence carries the full argument, including
// why the fit is NOT moved to the period boundary.
//
// The observer then records each instance as it is fitted — once per period
// rather than once per measure, which is what makes a synchronous publish on
// that path affordable at all.
func (r *modelRecorder) FitOptions() []compute.LiveModelOption {
	return []compute.LiveModelOption{
		compute.WithFitCadence(compute.DefaultFitCadence),
		compute.WithFitObserver(r.FactorModelFitted),
	}
}

// FactorModelFitted records one fitted factor-model instance. It satisfies
// compute.WithFitObserver.
//
// A FAILURE IS COUNTED AND LOGGED, NEVER PROPAGATED, and the asymmetry is
// deliberate. The observer runs inside the measure evaluation that fitted the
// model; returning an error here would mean a broker hiccup takes down a
// recompute that has a perfectly good model in hand. The book keeps being
// priced, and the counter says the price is no longer reproducible — which is a
// degradation an operator can act on rather than an outage.
func (r *modelRecorder) FactorModelFitted(ctx context.Context, m *factormodel.Model) {
	if err := r.publisher.EmitFactorModel(ctx, m); err != nil {
		r.failed.WithLabelValues(artifactFactorModel).Inc()
		// The identity is read off the model only when there is one. A nil model
		// is itself one of the refusals EmitFactorModel returns, and dereferencing
		// it to describe the failure would turn a reported degradation into a
		// crash of the recompute this observer promises not to fail.
		modelID, asOf := "", time.Time{}
		if m != nil {
			modelID, asOf = m.ModelID, m.AsOf
		}
		r.logger.Error("risk-engine: the fitted factor model was not recorded — measures citing "+
			"it name a model instance that exists nowhere",
			"err", err, "model_id", modelID, "as_of", asOf,
			"subject", publish.EventTypeFactorModelFitted)
		return
	}
	r.recorded.WithLabelValues(artifactFactorModel).Inc()
}

// CurveCalibrated records one calibrated discount curve. It satisfies
// curve.Calibrator.OnCalibrated, and degrades the same way FactorModelFitted
// does: the curve is already in the store and already pricing the book.
func (r *modelRecorder) CurveCalibrated(ctx context.Context, currency string, asOf time.Time, c *curve.Curve) {
	if err := r.publisher.EmitCalibratedCurve(ctx, currency, asOf, c); err != nil {
		r.failed.WithLabelValues(artifactCurve).Inc()
		r.logger.Error("risk-engine: the calibrated curve was not recorded — the DV01 it "+
			"discounts will outlive the curve that produced it",
			"err", err, "currency", currency, "as_of", asOf,
			"subject", publish.EventTypeCurveCalibrated)
		return
	}
	r.recorded.WithLabelValues(artifactCurve).Inc()
}
