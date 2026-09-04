// Package drift is the wealth service's target-weight drift evaluation
// (WEALTH-01d): it asks, of every household valuation the service folds, how far
// the household's actual book has moved from the model portfolio its risk profile
// selects, and says so when the model's own band is breached.
//
// # Why this exists
//
// internal/wealth has carried ComputeDrift, Drift.Breached, SelectModel and
// Propose since WEALTH-01d, tested and correct, with NO PRODUCTION CALLER
// ANYWHERE — the band comparison that decides a rebalance is due was called from
// nothing in the repository (#1010). The platform could execute a rebalance
// somebody asked for and could not notice that one was due: a book drifted past
// its stated allocation, nothing was wrong, nothing alerted, and the tracking
// error accumulated against a target the system held a schema for and no data.
// That is a different failure from an execution defect, and it is silent by
// construction.
//
// # It observes the fold; it does not gate it
//
// Evaluation hangs off the FACT that changed the book — the same trigger the
// compliance monitor uses — rather than a new ticker or a CronJob. The household
// book is the primary record and drift is derived from it, so a household whose
// drift cannot be computed is still FOLDED: Observe never fails a delivery.
// Refusing the valuation would trade a missing drift number for a missing
// household, which is strictly worse.
//
// # What is deliberately NOT here
//
// No wealth.v1.Proposal is emitted and no trade list is built. internal/wealth's
// Propose already produces one by reusing the OPT-01 rebalance engine, and the
// optimization service already serves that path — what was missing is the
// DETECTION, which is what this package supplies. Publishing a Proposal FACT is a
// recommendation with a lifecycle (an id, an approval, an expiry) and today it has
// no consumer, no store and no approval surface, so emitting one would recreate
// exactly the dark-capability shape this package exists to close.
package drift

import (
	"errors"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/wealth"
)

// Outcome is why a household's drift evaluation ended the way it did. It is the
// metric label, and the values are deliberately SEPARATE rather than collapsed
// into breached/not-breached: "measured, and in band" and "could not be measured"
// must never read the same, which is the platform rule this capability was
// missing for.
type Outcome string

const (
	// OutcomeInBand — a model was resolved and the largest per-instrument drift is
	// within the model's own tolerance. The only outcome that means "checked, and
	// fine".
	OutcomeInBand Outcome = "in_band"
	// OutcomeBreached — a model was resolved and the band is breached. This is the
	// signal a rebalance is due.
	OutcomeBreached Outcome = "breached"
	// OutcomeNoProfile — the valuation asserted RISK_PROFILE_UNSPECIFIED, so no
	// model can be selected for this household at all. An operator problem on the
	// publisher, not on the catalogue.
	OutcomeNoProfile Outcome = "no_profile"
	// OutcomeNoModel — the catalogue is armed and holds no model for this
	// household's profile. The firm has not published targets for that profile.
	OutcomeNoModel Outcome = "no_model"
	// OutcomeCatalogueUnarmed — the model replay has not landed yet, so the
	// catalogue cannot answer. Transient by construction; it is its own label so a
	// rolling restart does not masquerade as a firm with no models.
	OutcomeCatalogueUnarmed Outcome = "catalogue_unarmed"
	// OutcomeAmbiguousModel — two model ids claim one risk profile, so the target
	// allocation is not decidable and the registry fails closed. An operator has to
	// purge the retired model's subject.
	OutcomeAmbiguousModel Outcome = "ambiguous_model"
)

// outcomes is the full label set, enumerated so NewMetrics can create every
// series at zero. A CounterVec label that has never been incremented exports NO
// series at all, and an alert written `increase(...) == 0` over a missing series
// is silent in exactly the state it was written to detect — which this repository
// has now shipped twice (#963, #973).
var outcomes = []Outcome{
	OutcomeInBand, OutcomeBreached, OutcomeNoProfile,
	OutcomeNoModel, OutcomeCatalogueUnarmed, OutcomeAmbiguousModel,
}

// Metrics is the drift evaluation's observable surface. Construct it at the
// composition root UNCONDITIONALLY — before any `if broker configured` branch —
// so a deployment with no bus still exports the series showing that nothing is
// being evaluated. Collectors registered inside a broker branch export nothing in
// the degraded state, which is the failure #963 and #973 both were.
type Metrics struct {
	evaluations *prometheus.CounterVec
}

// Catalogue is the part of the model registry the gauges below read. It is an
// interface so NewMetrics states exactly what it needs — the catalogue's posture,
// never its contents.
type Catalogue interface {
	Armed() bool
	Count() int
}

// NewMetrics registers the drift collectors and initialises every outcome series
// to zero.
//
// THE TWO GAUGES ARE GaugeFuncs, READ AT SCRAPE TIME, and that is not a style
// choice. The first version Set them from Observe, which meant a service that had
// armed its catalogue correctly but had not yet folded a valuation reported
// kanz_wealth_model_catalogue_armed=0 — "the replay never landed" — indefinitely.
// Caught by running the binary, not by a test: every unit test called Observe.
// A posture gauge must be a function of the posture, not of traffic.
func NewMetrics(reg prometheus.Registerer, cat Catalogue) *Metrics {
	m := &Metrics{
		evaluations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_wealth_drift_evaluations_total",
			Help: "Household target-weight drift evaluations by outcome. Only outcome=\"in_band\" means the book was measured and is within its model's band.",
		}, []string{"outcome"}),
	}
	reg.MustRegister(m.evaluations)
	for _, o := range outcomes {
		m.evaluations.WithLabelValues(string(o)).Add(0)
	}
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_wealth_model_catalogue_armed",
		Help: "1 once the model-portfolio replay has landed. 0 means no household's drift can be evaluated.",
	}, func() float64 { return boolGauge(cat != nil && cat.Armed()) }))
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_wealth_models_registered",
		Help: "Model portfolios resident in the catalogue. 0 with armed=1 means the firm has published no target allocations.",
	}, func() float64 {
		if cat == nil {
			return 0
		}
		return float64(cat.Count())
	}))
	return m
}

// Result is one household's drift against its model. Model is empty and Evaluated
// is false when the evaluation could not be made; Outcome always says which of the
// reasons applied.
type Result struct {
	HouseholdID string
	Profile     wealth.RiskProfile
	Outcome     Outcome
	// Evaluated is true only for OutcomeInBand and OutcomeBreached — the two
	// outcomes where Drift and Tolerance below carry meaning. A caller rendering
	// this must branch on it: a zero Drift on an unevaluated household is the
	// absence of a measurement, not a book that matches its model exactly.
	Evaluated bool
	ModelID   string
	Tolerance float64
	Drift     wealth.Drift
	Breached  bool
	// Reason is the registry's own error text for an unevaluated household, so an
	// operator reading the read surface or the log sees which of the three
	// catalogue states applied without correlating against a metric.
	Reason string
}

// Monitor evaluates household drift against the model catalogue.
type Monitor struct {
	tenant  string
	models  *wealth.ModelRegistry
	metrics *Metrics
	logger  *slog.Logger
}

// New wires a Monitor to the catalogue it resolves models from. metrics may be
// nil only in a test; the composition root always supplies one, because a Monitor
// that evaluates without counting is indistinguishable from one that never ran.
func New(tenant string, models *wealth.ModelRegistry, metrics *Metrics, logger *slog.Logger) *Monitor {
	if logger == nil {
		logger = slog.Default()
	}
	return &Monitor{tenant: tenant, models: models, metrics: metrics, logger: logger}
}

// Evaluate measures one household against its model WITHOUT recording anything.
// It is the read path: the household exposure endpoint calls it so an advisor can
// ask "has this household drifted" on demand, and a query must not move the
// counters that measure how the BOOK is behaving.
func (m *Monitor) Evaluate(h wealth.Household) Result {
	res := Result{HouseholdID: h.HouseholdID, Profile: h.RiskProfile}
	if m == nil || m.models == nil {
		res.Outcome = OutcomeCatalogueUnarmed
		res.Reason = "no model catalogue is wired into this instance"
		return res
	}

	model, err := m.models.Model(m.tenant, h.RiskProfile)
	if err != nil {
		res.Reason = err.Error()
		switch {
		case errors.Is(err, wealth.ErrModelCatalogueUnarmed):
			res.Outcome = OutcomeCatalogueUnarmed
		case errors.Is(err, wealth.ErrModelProfileAmbiguous):
			res.Outcome = OutcomeAmbiguousModel
		case h.RiskProfile == wealth.ProfileUnspecified:
			// Ordered BEFORE the ErrNoModelForProfile arm because the registry
			// wraps that sentinel for the no-profile case too: the household
			// asserting no profile and the firm publishing no model for a profile
			// the household did assert are different people's problems.
			res.Outcome = OutcomeNoProfile
		default:
			res.Outcome = OutcomeNoModel
		}
		return res
	}

	vp := wealth.Aggregate(h)
	d := wealth.ComputeDrift(vp.Weights(), model)
	res.Evaluated = true
	res.ModelID = model.ModelID
	res.Tolerance = model.Tolerance
	res.Drift = d
	res.Breached = d.Breached(model.Tolerance)
	if res.Breached {
		res.Outcome = OutcomeBreached
	} else {
		res.Outcome = OutcomeInBand
	}
	return res
}

// Observe evaluates one household and RECORDS the outcome — the write path,
// called from the valuation fold. It returns NOTHING, and that is the contract:
// the household book is the primary record and this is derived from it, so a
// drift that cannot be computed must not fail the delivery that carried the
// valuation. A caller that wants the numbers calls Evaluate, which records
// nothing.
func (m *Monitor) Observe(h wealth.Household) {
	res := m.Evaluate(h)
	if m != nil && m.metrics != nil {
		m.metrics.evaluations.WithLabelValues(string(res.Outcome)).Inc()
	}
	if m == nil {
		return
	}

	switch res.Outcome {
	case OutcomeBreached:
		// WARN, with the numbers: this is the signal a rebalance is due, and it is
		// the whole reason this package exists. The instrument is named because
		// "the book drifted" without saying where sends an advisor back to the
		// data to find out.
		m.logger.Warn("household has DRIFTED past its model's band — a rebalance is due",
			"household_id", h.HouseholdID, "model_id", res.ModelID, "profile", res.Profile,
			"max_drift", res.Drift.Max, "total_drift", res.Drift.Total, "tolerance", res.Tolerance,
			"worst_instrument", worstInstrument(res.Drift))
	case OutcomeInBand:
		m.logger.Debug("household is within its model's band",
			"household_id", h.HouseholdID, "model_id", res.ModelID,
			"max_drift", res.Drift.Max, "tolerance", res.Tolerance)
	default:
		// NOT measured. Logged at WARN rather than DEBUG because an unevaluable
		// household is the state this capability was in for its entire existence,
		// and it looks identical to a healthy one from every other signal.
		m.logger.Warn("household drift NOT EVALUATED — this book is not being checked against any target allocation",
			"household_id", h.HouseholdID, "profile", res.Profile,
			"outcome", string(res.Outcome), "reason", res.Reason)
	}
}

// worstInstrument names the instrument carrying the largest absolute drift, or ""
// when the drift map is empty. Ties resolve on instrument id so the log line for a
// given book does not change between evaluations.
func worstInstrument(d wealth.Drift) string {
	worst, best := "", -1.0
	for id, dw := range d.ByInstrument {
		a := dw
		if a < 0 {
			a = -a
		}
		if a > best || (a == best && id < worst) {
			worst, best = id, a
		}
	}
	return worst
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
