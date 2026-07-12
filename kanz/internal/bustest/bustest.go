// Package bustest supports integration tests that drive the REAL event spine.
//
// It exists because of a collision that only appears once CI provisions the real
// topology (EXEC-M9): an integration test on a PRODUCTION subject cannot create
// its own stream for that subject, because two JetStream streams may not claim
// overlapping subjects — and the production stream is already there. The test
// would fail with "subjects overlap with an existing stream", against the very
// spine it exists to prove itself against.
//
// The resolution is not to give such a test a private subject. A test that proves
// `order.order.submit` reaches the OMS must publish to `order.order.submit`; the
// EXEC-M7a bug was precisely that no stream was bound to it, and a test on a
// made-up subject would have been green through all of it. So: bind to the real
// stream when the real topology is there, and fall back to a scratch stream when
// it is not (a developer's bare broker).
package bustest

import (
	"context"
	"fmt"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
)

// EnsureSubjects makes sure every subject is carried by SOME stream, and returns
// nothing to clean up when the production topology already carries them.
//
// If the subjects are already bound (CI bootstraps infra/nats/bootstrap-job.yaml,
// the same script production applies), the test runs against those streams — which
// is the point: it is then proving the real spine, including that the subject is
// bound at all. Otherwise it creates a scratch stream named `name` and registers
// its deletion.
func EnsureSubjects(t *testing.T, ctx context.Context, js jetstream.JetStream, name string, subjects []string) {
	t.Helper()
	if len(subjects) == 0 {
		t.Fatal("bustest: no subjects")
	}

	bound := 0
	for _, s := range subjects {
		if _, err := js.StreamNameBySubject(ctx, s); err == nil {
			bound++
		}
	}
	switch bound {
	case len(subjects):
		return // the real topology carries them all
	case 0:
		// A bare broker: provide a scratch stream so the test can still run.
	default:
		// Some bound, some not. Do NOT paper over it — a half-bound topology is the
		// exact production defect these tests exist to catch (a service whose events
		// silently go nowhere because one subject was forgotten).
		t.Fatalf("bustest: %d of %d subjects are bound to a stream — the topology is incomplete, "+
			"and events on the unbound ones would be hard publish failures in production. Subjects: %v",
			bound, len(subjects), subjects)
	}

	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      name,
		Subjects:  subjects,
		Storage:   jetstream.MemoryStorage,
		Retention: jetstream.LimitsPolicy,
	}); err != nil {
		t.Fatalf("bustest: create scratch stream %s: %v", name, err)
	}
	t.Cleanup(func() {
		// context.Background: the test's ctx is usually already cancelled by now, and
		// the stream must still go — a leaked stream poisons the next run.
		_ = js.DeleteStream(context.Background(), name)
	})
}

// StreamFor returns the stream carrying subject, for tests that need to inspect it.
func StreamFor(ctx context.Context, js jetstream.JetStream, subject string) (string, error) {
	name, err := js.StreamNameBySubject(ctx, subject)
	if err != nil {
		return "", fmt.Errorf("bustest: no stream carries %q: %w", subject, err)
	}
	return name, nil
}
