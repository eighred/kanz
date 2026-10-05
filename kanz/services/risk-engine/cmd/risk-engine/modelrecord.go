package main

import (
	"context"
	"errors"
	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/pkg/bus"
	modelartifacts "github.com/eighred/kanz/services/risk-engine/internal/artifacts"
	"github.com/eighred/kanz/services/risk-engine/internal/config"
	"github.com/jackc/pgx/v5/pgxpool"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/factormodel"
	"github.com/eighred/kanz/internal/risk/pricing/curve"
	"github.com/eighred/kanz/internal/risk/publish"
)

// modelRecorder commits pricing artifacts before they become usable. Counters
// are registered even when the calibration/fit configuration is absent, so an
// inactive source is distinguishable from an exporter that never registered.
type modelRecorder struct {
	store     *modelartifacts.Store
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
		Help: "Model artifacts published and, when a database is configured, durably retained by this process, by artifact. Zero does not imply retained history is empty.",
	}, []string{"artifact"})
	failed := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kanz_risk_model_artifact_record_failed_total",
		Help: "Artifact persistence or publication failures that refused installation into pricing, by artifact.",
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
		compute.WithFitLookup(r.storedModel),
	}
}

// FactorModelFitted records one fitted factor-model instance. It satisfies
// compute.WithFitObserver.
//
// Failed publication refuses the fit before it enters the provider cache. The
// caller reports unresolved inputs and retries on its next evaluation.
func (r *modelRecorder) FactorModelFitted(ctx context.Context, m *factormodel.Model) error {

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
		r.logger.Error("risk-engine: factor artifact publication failed; refusing model use",
			"err", err, "model_id", modelID, "as_of", asOf,
			"subject", publish.EventTypeFactorModelFitted)
		return err
	}
	if r.store != nil {
		if err := r.store.RecordModel(ctx, m); err != nil {
			r.failed.WithLabelValues(artifactFactorModel).Inc()
			return err
		}
	}
	r.recorded.WithLabelValues(artifactFactorModel).Inc()
	return nil
}

// CurveCalibrated records one calibrated discount curve. It satisfies
// curve.Calibrator.OnCalibrated, and refuses installation if the artifact cannot be published.
func (r *modelRecorder) CurveCalibrated(ctx context.Context, currency string, asOf time.Time, c *curve.Curve) error {

	if err := r.publisher.EmitCalibratedCurve(ctx, currency, asOf, c); err != nil {
		r.failed.WithLabelValues(artifactCurve).Inc()
		r.logger.Error("risk-engine: curve artifact publication failed; refusing installation",
			"err", err, "currency", currency, "as_of", asOf,
			"subject", publish.EventTypeCurveCalibrated)
		return err
	}
	if r.store != nil {
		if err := r.store.RecordCurve(ctx, currency, asOf, c); err != nil {
			r.failed.WithLabelValues(artifactCurve).Inc()
			return err
		}
	}
	r.recorded.WithLabelValues(artifactCurve).Inc()
	return nil
}

// The same tenant pool backs recovery snapshots and retained pricing inputs;
// adding reconstruction must not double the per-replica connection budget.
func newDurableModelRecorder(ctx context.Context, cfg config.Config, publisher *publish.Publisher, logger *slog.Logger, reg prometheus.Registerer) (*pgxpool.Pool, *modelRecorder, error) {
	r := newModelRecorder(publisher, logger, reg)
	if cfg.DatabaseURL == "" {
		return nil, r, nil
	}
	pool, err := pg.NewTenantPool(ctx, cfg.DatabaseURL, cfg.Tenant)
	if err != nil {
		return nil, nil, err
	}
	r.store = modelartifacts.New(pool)
	if err := r.store.Check(ctx); err != nil {
		pool.Close()
		return nil, nil, err
	}
	return pool, r, nil
}

func (r *modelRecorder) storedModel(ctx context.Context, asOf time.Time) (*factormodel.Model, error) {
	if r.store == nil {
		return nil, nil
	}
	id := factormodel.DefaultModelID(factormodel.Config{Type: factormodel.Statistical})
	m, err := r.store.ModelAt(ctx, id, asOf, time.Now())
	if errors.Is(err, modelartifacts.ErrMissing) {
		return nil, nil
	}
	return m, err
}

func (r *modelRecorder) storedCurve(ctx context.Context, currency string, asOf time.Time) (*curve.Curve, bool) {
	if r.store == nil {
		return nil, false
	}
	c, err := r.store.CurveAt(ctx, currency, asOf, time.Now())
	return c, err == nil
}

func (r *modelRecorder) handler(tenant string) bus.EventHandler {
	if r.store == nil {
		return nil
	}
	return r.store.Handler(tenant)
}
