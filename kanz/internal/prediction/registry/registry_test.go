package registry_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/eighred/kanz/internal/prediction/registry"
)

// memLog is an in-memory platform.model log: an ordered append list with a
// replay from offset 0 (the seam the concrete NATS/Kafka binding implements).
type memLog struct {
	mu     sync.Mutex
	events []registry.Event
}

func (l *memLog) Publish(_ context.Context, e registry.Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
	return nil
}

func (l *memLog) Replay(_ context.Context, apply func(registry.Event) error) error {
	l.mu.Lock()
	snap := append([]registry.Event(nil), l.events...)
	l.mu.Unlock()
	for _, e := range snap {
		if err := apply(e); err != nil {
			return err
		}
	}
	return nil
}

func (l *memLog) drainTo(t *testing.T, c *registry.CoordinatedRegistry) {
	t.Helper()
	l.mu.Lock()
	snap := append([]registry.Event(nil), l.events...)
	l.mu.Unlock()
	for _, e := range snap {
		if err := c.Apply(e); err != nil {
			t.Fatalf("apply: %v", err)
		}
	}
}

func meta(id string) registry.Metadata { return registry.Metadata{ModelID: id, Version: "1"} }

// The MLOPS-01a gate: a model cannot be promoted to PRIMARY without a passing
// validation on record.
func TestPrimaryRequiresValidation(t *testing.T) {
	r := registry.New(nil)
	if err := r.Register(meta("m1"), registry.RoleCandidate); err != nil {
		t.Fatalf("candidate register: %v", err)
	}
	if err := r.Register(meta("m1"), registry.RolePrimary); !errors.Is(err, registry.ErrNotValidated) {
		t.Fatalf("primary without validation = %v want ErrNotValidated", err)
	}
	if err := r.RecordValidation("m1", registry.Validation{Passed: true, Report: "rep-1"}); err != nil {
		t.Fatalf("record validation: %v", err)
	}
	if err := r.Register(meta("m1"), registry.RolePrimary); err != nil {
		t.Fatalf("primary after validation: %v", err)
	}
	if !r.IsPrimary("m1") {
		t.Error("m1 should be primary after a passing validation")
	}
}

func TestModelLoaderMaterializes(t *testing.T) {
	r := registry.New(func(m registry.Metadata) (registry.Model, error) {
		return "model:" + m.ModelID, nil
	})
	_ = r.Register(meta("m1"), registry.RoleCandidate)
	model, role, ok := r.Get("m1")
	if !ok || role != registry.RoleCandidate || model != "model:m1" {
		t.Fatalf("Get = (%v,%v,%v) want (model:m1, candidate, true)", model, role, ok)
	}
}

// Two coordinated replicas over one shared log converge: a promotion on A
// (validate → primary, in order) becomes visible on B after it drains the log.
func TestCoordinatedConvergence(t *testing.T) {
	log := &memLog{}
	ctx := context.Background()
	a := registry.NewCoordinated(registry.New(nil), log, "pod-a")
	b := registry.NewCoordinated(registry.New(nil), log, "pod-b")

	// Register the candidate, validate, then promote — the keyed order the log
	// preserves so the gate holds on every replica.
	if err := a.Register(ctx, meta("m1"), registry.RoleCandidate); err != nil {
		t.Fatalf("A candidate: %v", err)
	}
	if err := a.RecordValidation(ctx, "m1", registry.Validation{Passed: true}); err != nil {
		t.Fatalf("A validate: %v", err)
	}
	if err := a.Register(ctx, meta("m1"), registry.RolePrimary); err != nil {
		t.Fatalf("A promote: %v", err)
	}

	// B hasn't seen anything yet.
	if _, _, ok := b.Get("m1"); ok {
		t.Fatal("B should not know m1 before draining the log")
	}
	log.drainTo(t, b)
	if !b.IsPrimary("m1") {
		t.Error("B should see m1 as primary after draining the shared log")
	}
}

// A replica skips applying its OWN live echoes (loop prevention) but Rebuild
// replays everything (including its own prior writes) from offset 0.
func TestOriginSkipButRebuildAll(t *testing.T) {
	log := &memLog{}
	ctx := context.Background()
	a := registry.NewCoordinated(registry.New(nil), log, "pod-a")

	_ = a.Register(ctx, meta("m1"), registry.RoleCandidate)
	_ = a.RecordValidation(ctx, "m1", registry.Validation{Passed: true})
	_ = a.Register(ctx, meta("m1"), registry.RolePrimary)

	// Applying A's own events is a no-op (already applied locally) — no gate
	// double-trip, state unchanged.
	log.drainTo(t, a)
	if !a.IsPrimary("m1") {
		t.Error("A state should be intact after skipping its own echoes")
	}

	// A fresh replica with the SAME origin (a restart) rebuilds from offset 0,
	// applying every event including its predecessor's own-origin writes.
	restarted := registry.NewCoordinated(registry.New(nil), log, "pod-a")
	if err := restarted.Rebuild(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if !restarted.IsPrimary("m1") {
		t.Error("restarted replica should rebuild m1 as primary from the log")
	}
}

// ===== RESOLUTION BY FEATURE CONTRACT, NEVER BY NAME (#112) =====
//
// This is the property that saved this package from deletion, asserted in three
// places — this package's doc, risk-engine's features.go, and the
// dark-capability exemption — while being IMPLEMENTED IN NONE. registry.Metadata had no
// FeatureSetRef and Get resolved by model id.
//
// The failure it prevents is silent: ask for a model by NAME and you get
// whatever is registered under that name, which may have been retrained against
// a different feature contract since the caller was written. A well-formed
// vector, a confident score, and a model reading columns that mean something
// else.

func validated(t *testing.T, r *registry.Registry, id, contract string) {
	t.Helper()
	if err := r.Register(registry.Metadata{ModelID: id, FeatureSetRef: contract}, registry.RoleCandidate); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
	if err := r.RecordValidation(id, registry.Validation{Passed: true}); err != nil {
		t.Fatalf("validate %s: %v", id, err)
	}
	if err := r.Register(registry.Metadata{ModelID: id, FeatureSetRef: contract}, registry.RolePrimary); err != nil {
		t.Fatalf("promote %s: %v", id, err)
	}
}

func TestPrimaryFor_ResolvesByContractNotByName(t *testing.T) {
	r := registry.New(nil)
	validated(t, r, "risk-v3", "portfolio-risk:1")
	validated(t, r, "flow-v1", "order-flow:2")

	_, meta, ok := r.PrimaryFor("portfolio-risk:1")
	if !ok {
		t.Fatal("no primary for a contract that has one")
	}
	if meta.ModelID != "risk-v3" {
		t.Fatalf("resolved %q for portfolio-risk:1, want risk-v3 — a caller that asked by "+
			"CONTRACT was handed a model trained on a different one", meta.ModelID)
	}
	// AND THE MODEL AGREES IT SERVES THAT CONTRACT. Checking only the id would
	// pass on a lookup that ignores the contract and happens to return the right
	// model — which is exactly what map iteration order would do half the time.
	if meta.FeatureSetRef != "portfolio-risk:1" {
		t.Fatalf("resolved a model declaring %q for a request for portfolio-risk:1",
			meta.FeatureSetRef)
	}
}

// A CONTRACT WITH NO PRIMARY ANSWERS "no", and that is a real answer rather than
// an error. A caller must not read it as "score it anyway".
func TestPrimaryFor_AnUnservedContractIsNotAnError(t *testing.T) {
	r := registry.New(nil)
	validated(t, r, "risk-v3", "portfolio-risk:1")

	if _, _, ok := r.PrimaryFor("order-flow:2"); ok {
		t.Fatal("a contract nobody has promoted a model for reported a primary")
	}
}

// A CANDIDATE IS NOT A PRIMARY. The MLOPS-01a gate exists so an unvalidated
// model cannot serve; resolving to one by contract would route around it.
func TestPrimaryFor_ACandidateNeverServes(t *testing.T) {
	r := registry.New(nil)
	if err := r.Register(registry.Metadata{ModelID: "risk-v4", FeatureSetRef: "portfolio-risk:1"}, registry.RoleCandidate); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := r.PrimaryFor("portfolio-risk:1"); ok {
		t.Fatal("a CANDIDATE was resolved as the serving model — the MLOPS-01a gate is routed around")
	}
}

// AN EMPTY CONTRACT MATCHES NOTHING, in both directions.
//
// It is the value a model registered before this field existed carries. Treating
// it as a wildcard would make every legacy model silently eligible for every
// contract — precisely the substitution this resolution exists to prevent.
func TestPrimaryFor_AnEmptyContractIsNotAWildcard(t *testing.T) {
	r := registry.New(nil)
	validated(t, r, "legacy", "") // declares no contract

	if _, _, ok := r.PrimaryFor(""); ok {
		t.Error("asking for the empty contract resolved a model")
	}
	if _, _, ok := r.PrimaryFor("portfolio-risk:1"); ok {
		t.Error("a model declaring NO contract was resolved for a real one — every legacy model " +
			"would be eligible for every feature set")
	}
}

// PROMOTION DEMOTES THE INCUMBENT, AND THE ANSWER IS DETERMINISTIC.
//
// Without the demotion two models are PRIMARY for one contract and PrimaryFor
// resolves by MAP ITERATION ORDER — so two replicas holding identical state
// score with different models, and the same replica may disagree with itself
// between calls. A registry whose answer depends on iteration order is worse
// than none: it looks authoritative.
func TestRegister_PromotionDemotesTheIncumbentForThatContract(t *testing.T) {
	r := registry.New(nil)
	validated(t, r, "risk-v3", "portfolio-risk:1")
	validated(t, r, "risk-v4", "portfolio-risk:1")

	if r.IsPrimary("risk-v3") {
		t.Error("the old model is still PRIMARY for a contract another model was promoted for")
	}
	if !r.IsPrimary("risk-v4") {
		t.Error("the promoted model is not PRIMARY")
	}
	// Asked repeatedly: a map-order answer would eventually differ.
	for i := range 50 {
		_, meta, ok := r.PrimaryFor("portfolio-risk:1")
		if !ok || meta.ModelID != "risk-v4" {
			t.Fatalf("call %d resolved %q (ok=%v), want risk-v4 every time — the answer depends "+
				"on map iteration order", i, meta.ModelID, ok)
		}
	}
}

// A DIFFERENT CONTRACT IS UNAFFECTED. The demotion must not reach across
// contracts, or promoting a flow model would silently unserve the risk one.
func TestRegister_PromotionDoesNotDemoteAnotherContractsPrimary(t *testing.T) {
	r := registry.New(nil)
	validated(t, r, "risk-v3", "portfolio-risk:1")
	validated(t, r, "flow-v1", "order-flow:2")

	if !r.IsPrimary("risk-v3") {
		t.Fatal("promoting a model for order-flow:2 demoted the primary for portfolio-risk:1")
	}
	if got := r.FeatureSets(); len(got) != 2 {
		t.Errorf("FeatureSets() = %v, want both contracts served", got)
	}
}
