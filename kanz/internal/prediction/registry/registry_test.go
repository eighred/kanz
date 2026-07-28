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
