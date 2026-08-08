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
// It is the remaining third of the AI-M1 bridge. services/risk-engine's
// internal/app/features.go records the original state — "internal/prediction
// shipped a feature publisher, a resilient inference client and a model registry
// — and had ZERO importers outside its own tests" — and the first two are now
// wired at risk-engine's composition root. This is the piece that is not.
//
// DELETING IT WAS PROPOSED AND REJECTED, so the argument is recorded here rather
// than re-litigated:
//
//   - It is not an orphan. Removing it would freeze a documented three-part
//     design permanently at two thirds and turn "wire it" into "rebuild it".
//   - Its job is a correctness property. It resolves a model by feature_set_ref
//     and NEVER by name, which is what stops a model scoring a feature set it
//     was not trained on — the same drift features.go warns about directly.
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
	Extra   map[string]string
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
