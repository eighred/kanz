package wealth

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
)

// ErrModelCatalogueUnarmed means the catalogue has NOT YET REPLAYED the models
// published on wealth.model.published.>, so "this profile has no model" cannot be
// distinguished from "the replay has not landed".
//
// It exists because the two states are the same empty map, and treating an
// unarmed catalogue as an empty one is how a control reports every household as
// having no target allocation for the first seconds of every rolling restart —
// the EXEC-M13 disarmed-mandate-registry shape, which this platform has now paid
// for three times (the pre-trade gate, the post-trade position book, and the
// wealth service's own household store, #261).
var ErrModelCatalogueUnarmed = errors.New("wealth: model catalogue has not finished arming from the stream")

// ErrNoModelForProfile means the catalogue IS armed and holds no model serving
// that risk profile. This is a real, actionable answer — the firm has not
// published targets for that profile — and it is deliberately a DIFFERENT value
// from ErrModelCatalogueUnarmed above and from a clean in-band evaluation.
var ErrNoModelForProfile = errors.New("wealth: no model portfolio serves this risk profile")

// ErrModelProfileAmbiguous means two or more DIFFERENT model ids claim the same
// risk profile, so which target allocation a household should be measured against
// is not decidable.
//
// IT FAILS CLOSED, and that is the point. One model per profile is the stated
// WEALTH-01d invariant, and SelectModel resolves by first match — so with two
// models resident, whichever the compacted replay happened to deliver first would
// silently become the target for every household on that profile, and the answer
// would change between pods and between restarts. A household measured against an
// arbitrary one of two allocations is worse than one that is not measured at all,
// because the drift number looks authoritative. Same stance as
// ErrMandateTenantUnresolved (internal/compliance): the ambiguity is in the data,
// so retrying resolves nothing and an operator has to purge the retired model's
// subject.
var ErrModelProfileAmbiguous = errors.New("wealth: two model portfolios claim the same risk profile")

// modelKey is the catalogue key. It is COMPOSITE for the reason
// internal/compliance's mandateKey is: model_id is an operator-chosen string, so
// two tenants naming a model "growth" is a coincidence rather than an attack, and
// keyed by model id alone one tenant's published targets would become the other's
// on the next replay.
type modelKey struct{ tenant, modelID string }

// ModelRegistry is the in-memory catalogue of the model portfolio IN FORCE per
// (tenant, model id) — the target allocations household drift is measured
// against. It is the WEALTH-01d twin of internal/compliance.MandateRegistry and
// is deliberately the same shape rather than a new one.
//
// # It is CURRENT STATE, not a history
//
// Models arrive on wealth.model.published.<tenant>.<model_id>, which rides the
// compacted wealth.> stream (one retained message per subject, no max-age), and a
// booting pod arms from DeliverLastPerSubject. So a replica holds exactly ONE
// version per model id and nothing may depend on a superseded model still being
// resident — on every rolling restart it is not.
//
// # Retiring a model is an operator action on the subject, not a message
//
// There is no tombstone. A model whose subject still holds a message is still
// delivered on every replay, forever, so publishing a REPLACEMENT model under a
// new id while the old id's subject survives leaves two models claiming one
// profile, and Model returns ErrModelProfileAmbiguous until the retired subject is
// purged. That is loud on purpose; the alternative — last-writer-wins across two
// ids with no ordering between subjects — resolves differently on each pod.
type ModelRegistry struct {
	mu sync.RWMutex
	// byKey is keyed on the subject's compaction key, so a republish of a model id
	// REPLACES rather than accumulating.
	byKey map[modelKey]ModelPortfolio
	// rejected names the models that arrived and could NOT be admitted, with the
	// reason. A rejected model is not an absent one: this stream is compacted, so
	// the message that failed is the LAST one on that model's subject and every
	// consumer that boots re-reads it, forever (the #619 shape). Kept so a caller
	// can say "unreadable" rather than "not published".
	rejected         map[modelKey]error
	rejectedProfiles map[modelKey]RiskProfile
	armed            bool
	logger           *slog.Logger
}

// ModelRegistryOption customizes the registry.
type ModelRegistryOption func(*ModelRegistry)

// WithModelLogger supplies the logger the catalogue announces refusals on.
// Without it the registry falls back to slog.Default() — it never refuses a model
// silently, because a model the catalogue dropped is invisible everywhere else:
// the household it would have served simply reports no target allocation.
func WithModelLogger(l *slog.Logger) ModelRegistryOption {
	return func(r *ModelRegistry) {
		if l != nil {
			r.logger = l
		}
	}
}

// NewModelRegistry returns an empty, UNARMED catalogue. Every Model lookup
// returns ErrModelCatalogueUnarmed until Arm is called.
func NewModelRegistry(opts ...ModelRegistryOption) *ModelRegistry {
	r := &ModelRegistry{
		byKey:    make(map[modelKey]ModelPortfolio),
		rejected: make(map[modelKey]error), rejectedProfiles: make(map[modelKey]RiskProfile),
		logger: slog.Default(),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Arm marks the initial replay complete — mirrors MandateRegistry.Arm, the
// pattern this copies rather than invents. Call it from the ready callback of
// bus.Consumer.SubscribeBroadcastReady, never from the goroutine that merely
// LAUNCHED the subscription: between "the subscription started" and "the replay
// has landed" every profile resolves as having no model, which is the exact
// window EXEC-M13 is about.
func (r *ModelRegistry) Arm() {
	r.mu.Lock()
	already := r.armed
	r.armed = true
	n := len(r.byKey)
	rejects := len(r.rejected)
	r.mu.Unlock()
	if already {
		return
	}
	// A catalogue that arms EMPTY is precisely the state this capability was
	// missing for: no target allocation is stored, so no household's drift can be
	// computed, and after this line nothing else will say so.
	if n == 0 {
		r.logger.Warn("model catalogue armed with NO models — every household's drift is UNEVALUABLE "+
			"and no rebalance will ever be flagged",
			"subject", SubjectModelAll,
			"fix", "publish a model portfolio per risk profile with cmd/kanz-model",
			"rejected", rejects)
		return
	}
	r.logger.Info("model catalogue armed", "models", n, "rejected", rejects)
}

// Armed reports whether the initial replay has landed.
func (r *ModelRegistry) Armed() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.armed
}

// Count is the number of models resident. It backs the gauge that makes an empty
// catalogue alertable rather than merely warned about once at startup.
func (r *ModelRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byKey)
}

// Put admits one model portfolio, replacing any previous model with the same id.
// It returns the validation error rather than swallowing it, so the bus handler
// can DLQ the delivery — and it also RECORDS the rejection, because on a compacted
// subject a refused message is re-delivered on every boot and the catalogue must
// be able to say "this model exists and is unreadable" rather than "no model".
func (r *ModelRegistry) Put(tenantID string, m ModelPortfolio) error {
	if tenantID == "" {
		return errors.New("wealth: model portfolio has no tenant; the catalogue is keyed per tenant and an " +
			"untenanted model would be served to every one of them")
	}
	key := modelKey{tenant: tenantID, modelID: m.ModelID}
	if err := m.Validate(); err != nil {
		r.mu.Lock()
		r.rejected[key] = err
		r.rejectedProfiles[key] = m.Profile
		delete(r.byKey, key)
		r.mu.Unlock()
		r.logger.Error("model portfolio REFUSED — no household on this profile has a target allocation",
			"tenant", tenantID, "model_id", m.ModelID, "err", err)
		return fmt.Errorf("wealth: model %s: %w", m.ModelID, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.rejected, key)
	delete(r.rejectedProfiles, key)
	r.byKey[key] = m.Clone()
	return nil
}

// Rejections lists the models that arrived and were refused, in deterministic
// order, for a readiness/inspection surface.
func (r *ModelRegistry) Rejections() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.rejected))
	for k, err := range r.rejected {
		out = append(out, fmt.Sprintf("%s/%s: %v", k.tenant, k.modelID, err))
	}
	sort.Strings(out)
	return out
}

// Model resolves the model portfolio a tenant's household of this profile is
// measured against. The failure modes are DISTINCT VALUES on purpose — unarmed,
// none published, and ambiguous are three different operator problems, and none of
// them is "in band".
//
// Resolution itself is SelectModel, the WEALTH-01d selector, rather than a second
// lookup written here: one implementation per concept, and this is the production
// caller it never had (#1010).
func (r *ModelRegistry) Model(tenantID string, profile RiskProfile) (ModelPortfolio, error) {
	if profile == ProfileUnspecified {
		// Not a statement about the catalogue — the household itself asserted no
		// profile, so no lookup is possible. Callers label this separately; see
		// services/wealth/internal/drift.
		return ModelPortfolio{}, fmt.Errorf("%w: household asserts no risk profile", ErrNoModelForProfile)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.armed {
		return ModelPortfolio{}, ErrModelCatalogueUnarmed
	}

	for key, rejectedProfile := range r.rejectedProfiles {
		if key.tenant == tenantID && rejectedProfile == profile {
			return ModelPortfolio{}, ErrModelRejected
		}
	}
	candidates := make([]ModelPortfolio, 0, len(r.byKey))
	claiming := make([]string, 0, 2)
	for k, m := range r.byKey {
		if k.tenant != tenantID {
			continue
		}
		candidates = append(candidates, m)
		if m.Profile == profile {
			claiming = append(claiming, m.ModelID)
		}
	}
	switch len(claiming) {
	case 0:
		return ModelPortfolio{}, ErrNoModelForProfile
	case 1:
	default:
		sort.Strings(claiming)
		return ModelPortfolio{}, fmt.Errorf("%w: %v both claim %v — purge the retired model's subject",
			ErrModelProfileAmbiguous, claiming, profile)
	}
	// Deterministic input to a first-match-wins selector. With exactly one
	// claimant the order cannot change the answer; sorting anyway is what keeps
	// that true if the ambiguity arm above is ever weakened, because Go's map
	// iteration order would otherwise decide which allocation a household is
	// measured against.
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ModelID < candidates[j].ModelID })
	m, ok := SelectModel(profile, candidates)
	if !ok {
		// Unreachable while the arms above hold. Kept because the alternative on a
		// future edit is a zero-valued ModelPortfolio being measured against as
		// though it were a real target: no instruments, so every held name reports
		// as fully drifted.
		return ModelPortfolio{}, ErrNoModelForProfile
	}
	return m, nil
}

var ErrModelRejected = errors.New("wealth: model precision or policy is invalid; exact republish required")
