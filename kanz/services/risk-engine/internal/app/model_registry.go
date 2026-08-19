package app

// CAN ANYTHING SCORE THE FEATURES THIS ENGINE PUBLISHES (#112)?
//
// # The gap this closes
//
// PublishFeaturesOn turns every recompute into an inference.feature.computed
// FACT on FeatureSetPortfolioRisk. Whether any model on the platform claims that
// contract was, until this file, UNKNOWABLE FROM GO. The publish succeeds, the
// FACT lands, the engine logs nothing, and a fleet with no promoted model for
// portfolio-risk:1 looks exactly like one scoring every vector. "Nothing
// configured" and "checked, and fine" were the same observation.
//
// internal/prediction/registry is the answer to that question and had no
// importer at all — the last third of the AI-M1 bridge, tracked as #112 through
// a dark-capability exemption for eight months. #504 gave the log a shared wire
// contract (inference.v1.ModelRegistryEvent), a producer (kanz-py's
// BusRegistryPublisher), a topic and a broker grant. This is the Go reader those
// three were waiting for.
//
// # This engine FOLLOWS the registry, it does not resolve against it
//
// Resolution by feature_set_ref is the property that saved the package from
// deletion, and the caller that needs it is a Go service that SCORES — which
// means internal/prediction's resilient inference client, which still has zero
// constructors anywhere (its Predict call site at sync_client.go passes
// test/arch's served-RPC guard while being unreachable in every deployment, and
// that guard's own doc says so). Wiring a scorer would decide what acts on a
// prediction, and that is a product ruling this file has no business making.
//
// What it does instead is REPORT, which needs no such ruling and is worth having
// on its own: the engine states, continuously, whether the contract it publishes
// on has a primary model, whether that model's validation is still current, and
// whether it has managed to read the log at all. A prediction layer with no model
// is a real and silent state today; after this it is a gauge and a warning.

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/prediction"
	"github.com/eighred/kanz/internal/prediction/registry"
)

// The four answers to "does a model serve this feature contract". They are kept
// APART rather than collapsed to 0/1 for this repository's standing reason: a
// registry that has not been read yet and a registry that was read and holds
// nothing are opposite findings, and only one of them is about the model fleet.
const (
	// ModelStateServing — a PRIMARY model claims the contract and its validation
	// is current.
	ModelStateServing = "serving"
	// ModelStateExpired — a PRIMARY model claims the contract and the validation
	// it was promoted on has lapsed. It IS still serving on the Python side; SR
	// 11-7 is about current validation, so this is a finding, not a fallback.
	ModelStateExpired = "expired"
	// ModelStateNone — the log has been folded and no model claims the contract.
	// Every FeatureVector published on it is being produced for nobody.
	ModelStateNone = "none"
	// ModelStateUnknown — the log has NOT been folded (the subscription has not
	// armed, or it failed). An empty registry here means nothing at all, and
	// reporting it as "none" would manufacture an outage out of a startup race.
	ModelStateUnknown = "unknown"
)

// ModelRegistry is this engine's follower of the platform.model.registered log,
// plus the posture it reports from it.
type ModelRegistry struct {
	coord     *registry.CoordinatedRegistry
	contracts []prediction.FeatureSetRef
	logger    *slog.Logger
	now       func() time.Time
	// armed is set once the backlog that existed at subscribe time has been
	// folded. Read on every scrape, so it is atomic rather than mutex-guarded.
	armed atomic.Bool
	folds *prometheus.CounterVec
}

// NewModelRegistry builds the follower and registers its posture.
//
// origin identifies this replica on the log. It is only ever used to skip a
// replica's own echoes, and a follower produces none — but it is passed
// truthfully rather than left blank, because the day something in Go does
// register a model, a blank origin makes every replica re-apply its own writes.
//
// It does NOT subscribe. Run does, and the split is deliberate: the gauge must
// exist and read "unknown" from the first scrape, including on a pod whose
// subscription never comes up. A posture that appears only once the transport
// works cannot report the transport being broken.
func NewModelRegistry(reg prometheus.Registerer, logger *slog.Logger, sub registry.LogSubscriber, origin string, contracts ...prediction.FeatureSetRef) (*ModelRegistry, error) {
	if sub == nil {
		return nil, errors.New("risk-engine: model registry needs a log subscriber")
	}
	if len(contracts) == 0 {
		return nil, errors.New("risk-engine: model registry needs at least one feature contract to report on")
	}
	m := &ModelRegistry{
		contracts: contracts,
		logger:    logger,
		now:       func() time.Time { return time.Now().UTC() },
		folds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_prediction_registry_fold_errors_total",
			Help: "Entries of the platform.model.registered log this replica could not apply, by " +
				"reason. Each one leaves the local registry missing a mutation the rest of the " +
				"fleet has, so the posture beside it may name the wrong model or no model at all. " +
				"decode = the payload is not a ModelRegistryEvent; unknown_op = this build has no " +
				"fold for an op the registrar published (it is BEHIND the schema); rejected = the " +
				"registry refused it, which for a promotion means the MLOPS-01a validation it " +
				"depends on was not in the replayed window (#112).",
		}, []string{"reason"}),
	}
	// EVERY REASON GETS A SERIES AT ZERO, for the reason the FI counters beside
	// the composition root state: a counter that appears only on first increment
	// reads as no-data, and an alert cannot fire on the none-to-some transition
	// that matters.
	for _, r := range []string{registry.FoldReasonDecode, registry.FoldReasonUnknown, registry.FoldReasonRejected} {
		m.folds.WithLabelValues(r).Add(0)
	}

	// METADATA-ONLY: nil ModelLoader. This engine never materialises a model —
	// the weights live in the Python serving stack, and a Go replica that
	// pretended to load them would fail on every entry. The registry supports
	// this shape explicitly, and the wire codec still carries artifact_uri so a
	// replica that one day DOES load has something to fetch.
	follower, err := registry.NewBusFollower(sub, m.onArmed, m.onFoldError)
	if err != nil {
		return nil, err
	}
	m.coord = registry.NewCoordinated(registry.New(nil), follower, origin)

	reg.MustRegister(m.folds, m)
	return m, nil
}

// Run folds the log and keeps folding until ctx ends. It blocks.
//
// A FAILURE HERE MUST NOT STOP THE ENGINE, and the caller is what enforces that
// — the same asymmetry features.go states for the publisher. The prediction
// layer is an observer of the risk engine: a model registry that cannot be read
// costs this gauge, and must never cost the risk numbers the platform trades on.
// The posture stays "unknown", which is the true answer.
func (m *ModelRegistry) Run(ctx context.Context) error {
	m.logger.Info("risk-engine following the model registry log",
		"subject", registry.SubjectModelRegistered,
		"contracts", contractNames(m.contracts),
		"resolves_models", false,
		"why", "this engine reports whether a model serves the contract it publishes features on; "+
			"nothing in Go scores a prediction yet (#112)")
	return m.coord.Rebuild(ctx)
}

// PrimaryFor answers the registry's one question for a contract: which model
// serves it, and in what state. Exported because it is the useful half — a
// caller that one day scores a vector asks exactly this, by contract and never
// by name.
func (m *ModelRegistry) PrimaryFor(contract prediction.FeatureSetRef) (registry.Metadata, string) {
	if !m.armed.Load() {
		return registry.Metadata{}, ModelStateUnknown
	}
	_, meta, ok := m.coord.PrimaryFor(string(contract))
	if !ok {
		return registry.Metadata{}, ModelStateNone
	}
	v, found := m.coord.ValidationFor(meta.ModelID)
	if found && v.Expired(m.now()) {
		return meta, ModelStateExpired
	}
	return meta, ModelStateServing
}

func (m *ModelRegistry) onArmed() {
	m.armed.Store(true)
	// WHAT THE LOG ACTUALLY SAID, once, at the moment it becomes knowable. The
	// gauge is the durable half; this line is what a human reads in the rollout.
	for _, c := range m.contracts {
		meta, state := m.PrimaryFor(c)
		switch state {
		case ModelStateServing:
			m.logger.Info("model registry: a primary model serves this feature contract",
				"feature_set_ref", string(c), "model_id", meta.ModelID)
		case ModelStateExpired:
			m.logger.Warn("model registry: the primary model for this feature contract holds a "+
				"LAPSED validation — it is still serving, and SR 11-7 is about CURRENT validation "+
				"rather than a one-time sign-off",
				"feature_set_ref", string(c), "model_id", meta.ModelID)
		default:
			// WARN, not Info. Every recompute publishes a FeatureVector on this
			// contract, and with no model claiming it those FACTs are produced
			// for nobody — a state that costs nothing visible and looks
			// identical to a working AI layer from every other signal.
			m.logger.Warn("model registry: NO model is promoted for a feature contract this engine "+
				"publishes on — every FeatureVector on it is scored by nothing, and the publish "+
				"still succeeds",
				"feature_set_ref", string(c),
				"subject", prediction.EventTypeFeatureComputed,
				"note", "an empty fold can also mean the registration aged out of the log's "+
					"retention window (the PLATFORM stream keeps 168h); the registrar re-announcing "+
					"is what makes the two distinguishable",
				"gauge", "kanz_prediction_model_primary")
		}
	}
}

func (m *ModelRegistry) onFoldError(reason, modelID string, err error) {
	m.folds.WithLabelValues(reason).Inc()
	m.logger.Error("model registry: an entry of the log could not be applied — this replica's "+
		"registry is now missing a mutation the rest of the fleet has",
		"reason", reason, "model_id", modelID, "err", err,
		"subject", registry.SubjectModelRegistered)
}

var modelPrimaryDesc = prometheus.NewDesc(
	"kanz_prediction_model_primary",
	"1 for the state this feature contract is actually in, 0 for the other three. serving = a "+
		"primary model claims the contract with a current validation. expired = it claims the "+
		"contract and its validation has lapsed (it still serves; that is the finding). none = the "+
		"log was folded and NO model claims it, so every FeatureVector published on this contract "+
		"is scored by nobody while the publish keeps succeeding. unknown = this replica has not "+
		"folded the log, so an empty registry means nothing — a none reported here would be an "+
		"outage manufactured out of a startup race (#112).",
	[]string{"feature_set_ref", "state"}, nil,
)

func (m *ModelRegistry) Describe(ch chan<- *prometheus.Desc) { ch <- modelPrimaryDesc }

// Collect evaluates at SCRAPE time, for the reason AnalyticsPosture states about
// itself and one more: a validation EXPIRES, so a gauge written once at boot
// would report serving for as long as the process lived — and the registry also
// MOVES, because the fold continues for the life of the process. Both inputs
// change without anything calling back in.
func (m *ModelRegistry) Collect(ch chan<- prometheus.Metric) {
	for _, c := range m.contracts {
		_, state := m.PrimaryFor(c)
		for _, s := range []string{ModelStateServing, ModelStateExpired, ModelStateNone, ModelStateUnknown} {
			v := 0.0
			if s == state {
				v = 1
			}
			ch <- prometheus.MustNewConstMetric(modelPrimaryDesc, prometheus.GaugeValue, v, string(c), s)
		}
	}
}

func contractNames(refs []prediction.FeatureSetRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, string(r))
	}
	return out
}
