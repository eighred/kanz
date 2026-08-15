// Package registry is the PRED-09 model registry and its DEBT-02b shared-state
// coordination (PARITY-04i). The process-local Registry holds the fleet's models
// keyed by id, enforcing the MLOPS-01a gate (a model can only be promoted to
// PRIMARY after a passing validation is on record). CoordinatedRegistry layers
// cross-replica convergence on top: every mutation is mirrored onto the
// platform.model append log and inbound peer events are applied, so a model
// promoted on one worker becomes visible on all of them.
//
// The append log is a Log seam (in-memory default; the concrete NATS/Kafka
// binding onto the platform.model topic wires at the composition root — the same
// inject-the-transport stance the rest of the platform takes). The live Model is
// not serialized; the log carries Metadata + role + validation and the injected
// ModelLoader rematerializes the Model on apply.
//
// # NOTHING IMPORTS THIS YET, AND THAT IS TRACKED RATHER THAN FORGOTTEN (#112)
//
// No composition root constructs a Registry today:
//
//	$ grep -rn "prediction/registry" --include=*.go . | grep -v _test
//	(nothing)
//
// It is part of the AI-M1 bridge, and the bridge is ONE THIRD WIRED, NOT TWO
// (corrected #112, 2026-08-15). The claim that "the first two are now wired at
// risk-engine's composition root" was false and had survived several audits:
//
//	$ grep -rn "SyncClient" --include=*.go .   -> only sync_client.go and its own tests
//
// The FEATURE PUBLISHER is wired (risk-engine's composition root builds
// prediction.NewPublisher). The RESILIENT INFERENCE CLIENT has no callers
// anywhere in the module, and is invisible to test/arch's dark-capability guard
// because that guard works at IMPORT granularity and internal/prediction IS
// imported — for the publisher. Its own doc names that limitation.
//
// WHAT ACTUALLY BLOCKS WIRING THIS IS NOT A COMPOSITION ROOT. The coordination
// plane has no transport and no producer, on either side:
//
//   - platform.model is not a declared subject in this module, and
//     infra/kafka/topics-job.yaml records that it was REMOVED from the
//     ground-truth topic table because "NOT ONE of which is published by any
//     service". Auto-create is disabled, so an event published to it has
//     nowhere to land.
//   - kanz-py's RegistryPublisher (kanz_inference/registry/coordination.py) is a
//     Protocol with no concrete implementation, so nothing registers a model
//     from the Python side either.
//
// So binding Log to the bus today would produce a registry nobody writes to,
// reading a topic that does not exist. #112 tracks the sequencing.
//
// DELETING IT WAS PROPOSED AND REJECTED, so the argument is recorded here rather
// than re-litigated:
//
//   - It is not an orphan. Removing it would freeze a documented three-part
//     design permanently at two thirds and turn "wire it" into "rebuild it".
//
//   - Its job is a correctness property: it resolves a model by feature_set_ref
//     and NEVER by name, which is what stops a model scoring a feature set it
//     was not trained on — the same drift features.go warns about directly.
//
//     THAT PROPERTY WAS ASSERTED HERE, IN features.go AND IN THE DARK-CAPABILITY
//     EXEMPTION WHILE BEING IMPLEMENTED IN NONE OF THEM. Metadata had no
//     FeatureSetRef and the only lookup was Get(modelID) — resolution by name,
//     the exact thing the argument said it prevented. It is implemented now
//     (PrimaryFor, and the one-primary-per-contract rule that makes its answer
//     deterministic), so the argument that saved this package is true rather
//     than aspirational.
//
//   - It cannot rot. The precedent for deleting unused code here is the Anthropic
//     adapter, and that rotted because it sat behind a build tag CI never
//     compiled. This is ordinary Go: `go build ./...` compiles it and
//     `go test ./...` runs its tests on every CI run. Inert, but verified inert.
//
// So an unused package here is not the "dead code" the estate deletes on sight.
// What it needs is a composition root, and #112 is where that is tracked.
package registry

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Role is a model's serving role in the registry.
type Role int

const (
	RoleUnspecified Role = iota
	RoleCandidate        // registered, may serve shadow/eval traffic
	RolePrimary          // the model serving production for its id
)

func (r Role) String() string {
	switch r {
	case RoleCandidate:
		return "candidate"
	case RolePrimary:
		return "primary"
	default:
		return "unspecified"
	}
}

// Metadata is the serializable model descriptor (the ModelMetadata analog that
// rides the platform.model log — never the live Model, which isn't serializable).
type Metadata struct {
	ModelID string
	Version string
	// FeatureSetRef is the feature contract this model was TRAINED ON, and it is
	// what a caller resolves by (#112).
	//
	// THE WHOLE POINT OF THIS PACKAGE IS THAT A CALLER NEVER NAMES A MODEL. Ask
	// for a model by id and you get whatever is registered under that id, which
	// may have been retrained against a different feature contract since the
	// caller was written — and the failure is silent: a well-formed vector, a
	// confident score, and a model reading columns that mean something else.
	// PrimaryFor asks by feature set instead, so a model that does not claim the
	// contract cannot be handed a vector shaped by it.
	//
	// This field did not exist until #112, while THREE places — this package's
	// doc, risk-engine's features.go and the dark-capability exemption — all
	// asserted the property as though it were implemented. It was the argument
	// that saved this package from deletion. Empty is allowed and means the model
	// declares no contract, which PrimaryFor refuses to match rather than
	// treating as a wildcard.
	FeatureSetRef string
	Extra         map[string]string
}

// Validation is the MLOPS-01a gate outcome for a model version.
type Validation struct {
	Passed bool
	Report string // signed report id / summary (AUDIT-01 evidence)
}

// Model is the live, materialized model. It is intentionally opaque here — the
// registry stores whatever the ModelLoader returns.
type Model any

// ModelLoader rematerializes a live Model from its Metadata (loading weights,
// building the predictor). Injected so the registry stays free of any concrete
// model type. A load error leaves the model registered but not serving.
type ModelLoader func(Metadata) (Model, error)

var (
	// ErrNotValidated is returned when a model is promoted to PRIMARY without a
	// passing validation on record — the MLOPS-01a gate.
	ErrNotValidated = errors.New("registry: model has no passing validation (MLOPS-01a gate)")
	// ErrUnknownModel is returned when validating/promoting an unregistered id.
	ErrUnknownModel = errors.New("registry: unknown model")
)

type entry struct {
	meta       Metadata
	role       Role
	validation Validation
	model      Model
}

// Registry is the process-local model registry. Concurrent-safe.
type Registry struct {
	mu     sync.RWMutex
	load   ModelLoader
	byID   map[string]*entry
	onLoad func(modelID string, err error)
}

// New builds a registry. load rematerializes live models (nil ⇒ models are held
// as their Metadata with a nil live Model — useful in tests / metadata-only
// nodes).
func New(load ModelLoader) *Registry {
	return &Registry{load: load, byID: map[string]*entry{}}
}

// Register records (or replaces) a model version at the given role. Promoting to
// PRIMARY requires a passing validation already on record for this id
// (ErrNotValidated otherwise) — the gate that stops an unvalidated model from
// serving. Registering as CANDIDATE has no gate.
func (r *Registry) Register(meta Metadata, role Role) error {
	if meta.ModelID == "" {
		return errors.New("registry: empty model id")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.byID[meta.ModelID]
	if e == nil {
		e = &entry{}
		r.byID[meta.ModelID] = e
	}
	if role == RolePrimary && !e.validation.Passed {
		return fmt.Errorf("%w: %s", ErrNotValidated, meta.ModelID)
	}
	// ONE PRIMARY PER FEATURE CONTRACT, AND PROMOTION DEMOTES THE INCUMBENT
	// (#112).
	//
	// Without this, two models can be PRIMARY for the same contract and
	// PrimaryFor resolves by MAP ITERATION ORDER — so two replicas holding
	// identical state score with different models, and the same replica may
	// disagree with itself between calls. A registry whose answer depends on
	// iteration order is worse than no registry: it looks authoritative.
	//
	// Demote rather than refuse, because promotion is what this operation IS. A
	// refusal would make every model swap a two-step dance whose intermediate
	// state has no primary at all — and on the coordinated log, where events are
	// replayed in order, that gap becomes a window in which peers serve nothing.
	if role == RolePrimary && meta.FeatureSetRef != "" {
		for id, other := range r.byID {
			if id != meta.ModelID && other.role == RolePrimary &&
				other.meta.FeatureSetRef == meta.FeatureSetRef {
				other.role = RoleCandidate
			}
		}
	}
	e.meta = meta
	e.role = role
	e.model = r.materialize(meta)
	return nil
}

// RecordValidation stores the gate outcome for a model id. The id must already be
// registered (validation is about a known candidate). Must precede a PRIMARY
// Register — the ordering the coordinated log preserves by keying on model_id.
func (r *Registry) RecordValidation(modelID string, v Validation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.byID[modelID]
	if e == nil {
		return fmt.Errorf("%w: %s", ErrUnknownModel, modelID)
	}
	e.validation = v
	return nil
}

// Get returns the live model and its role for id.
func (r *Registry) Get(modelID string) (Model, Role, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e := r.byID[modelID]
	if e == nil {
		return nil, RoleUnspecified, false
	}
	return e.model, e.role, true
}

// Primary returns the model id currently serving as PRIMARY for the given
// version family, or ("", false) if none. Since ids are version-qualified, this
// reports whether a specific id is primary.
func (r *Registry) IsPrimary(modelID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e := r.byID[modelID]
	return e != nil && e.role == RolePrimary
}

// PrimaryFor returns the model serving as PRIMARY for a feature contract, and
// the metadata that says which model it is.
//
// RESOLUTION BY CONTRACT, NEVER BY NAME (#112). A caller holding a feature vector
// asks which model may score THIS contract; it never names a model, so it cannot
// be handed one that was retrained against a different one.
//
// AN EMPTY featureSetRef MATCHES NOTHING, deliberately. It is the value a model
// registered before this field existed carries, and treating it as a wildcard
// would make exactly the substitution this exists to prevent — every legacy
// model silently eligible for every contract.
//
// ok=false is a real and common answer: no model has been promoted for this
// contract yet. It is not an error, and a caller must not read it as "score it
// anyway".
func (r *Registry) PrimaryFor(featureSetRef string) (Model, Metadata, bool) {
	if featureSetRef == "" {
		return nil, Metadata{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.byID {
		if e.role == RolePrimary && e.meta.FeatureSetRef == featureSetRef {
			return e.model, e.meta, true
		}
	}
	return nil, Metadata{}, false
}

// FeatureSets lists the contracts that have a PRIMARY model, sorted — the
// posture read a composition root exposes so "no model serves this contract" is
// visible rather than inferred from silence.
func (r *Registry) FeatureSets() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := map[string]bool{}
	for _, e := range r.byID {
		if e.role == RolePrimary && e.meta.FeatureSetRef != "" {
			seen[e.meta.FeatureSetRef] = true
		}
	}
	out := make([]string, 0, len(seen))
	for ref := range seen {
		out = append(out, ref)
	}
	sort.Strings(out)
	return out
}

// Models lists the registered model ids, sorted — a stable snapshot for gauges.
func (r *Registry) Models() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byID))
	for id := range r.byID {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (r *Registry) materialize(meta Metadata) Model {
	if r.load == nil {
		return nil
	}
	m, err := r.load(meta)
	if err != nil {
		if r.onLoad != nil {
			r.onLoad(meta.ModelID, err)
		}
		return nil
	}
	return m
}
