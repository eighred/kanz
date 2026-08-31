package prediction

// #895 — the degraded-fallback cache is bounded by a subject cardinality the
// CALLER states, and the bound must not be able to take away the answer the
// breaker exists to keep giving.
//
// These live in the internal package because the load-bearing assertions are
// about WHICH entry is dropped and how many remain, and the size of a cache is
// not part of its exported contract.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	inferencepb "github.com/eighred/kanz/kanz-schemas-go/inference/v1"
)

// newStubClient is the in-package construction helper. It states a subject cap
// explicitly for every test that does not care about the cap, which is the point
// of the cap having no default: no test can accidentally certify an unbounded
// client, because there is no longer such a thing to construct.
func newStubClient(t *testing.T, stub inferencepb.InferenceServiceClient, opts SyncClientOptions) *SyncClient {
	t.Helper()
	if opts.MaxCachedSubjects <= 0 {
		opts.MaxCachedSubjects = 64
	}
	c, err := NewSyncClientWithStub(stub, opts)
	if err != nil {
		t.Fatalf("NewSyncClientWithStub: %v", err)
	}
	return c
}

func normal(subject string, value float64) *inferencepb.PredictionEnvelope {
	return normalPred(subject, value)
}

func mustCache(t *testing.T, max int) *PredictionCache {
	t.Helper()
	c, err := NewPredictionCache(max)
	if err != nil {
		t.Fatalf("NewPredictionCache(%d): %v", max, err)
	}
	return c
}

// TestCacheRefusesToBeConstructedUnbounded is the "nothing configured" ≠
// "checked, and fine" half: the cap has no default, so an unset one is a
// refusal at construction rather than a map that grows until the pod dies.
func TestCacheRefusesToBeConstructedUnbounded(t *testing.T) {
	for _, max := range []int{0, -1} {
		if _, err := NewPredictionCache(max); !errors.Is(err, ErrMaxCachedSubjectsRequired) {
			t.Errorf("NewPredictionCache(%d) err=%v, want ErrMaxCachedSubjectsRequired", max, err)
		}
	}
	opts := DefaultSyncClientOptions()
	if opts.MaxCachedSubjects != 0 {
		t.Fatalf("DefaultSyncClientOptions must not invent a subject cap, got %d", opts.MaxCachedSubjects)
	}
	if _, err := NewSyncClientWithStub(&stubClient{}, opts); !errors.Is(err, ErrMaxCachedSubjectsRequired) {
		t.Errorf("NewSyncClientWithStub with the bare defaults err=%v, want ErrMaxCachedSubjectsRequired", err)
	}
	if _, err := NewSyncClient("passthrough:///inference:9000", opts); !errors.Is(err, ErrMaxCachedSubjectsRequired) {
		t.Errorf("NewSyncClient with the bare defaults err=%v, want ErrMaxCachedSubjectsRequired", err)
	}
}

// TestCachedSubjectsDoNotAccumulate PROVES the bound rather than asserting it:
// many more subjects than the cap, and the map stays at the cap. This is the
// shape orderview's TestTerminalOrdersDoNotAccumulate uses.
func TestCachedSubjectsDoNotAccumulate(t *testing.T) {
	const max = 16
	c := mustCache(t, max)
	for i := 0; i < 200*max; i++ {
		subject := fmt.Sprintf("PF-%d", i)
		c.Store(subject, normal(subject, float64(i)))
		if got := len(c.entries); got > max {
			t.Fatalf("after %d stores the cache holds %d subject(s), cap is %d", i+1, got, max)
		}
	}
	if got := len(c.entries); got != max {
		t.Fatalf("cache holds %d subject(s) after 3200 stores, want exactly the cap %d", got, max)
	}
}

// TestTheColdestSubjectIsTheOneDropped pins the eviction ORDER. A cap that drops
// an arbitrary subject would still bound the map and would still pass the count
// test above, which is why this one exists separately.
func TestTheColdestSubjectIsTheOneDropped(t *testing.T) {
	c := mustCache(t, 2)
	c.Store("A", normal("A", 1))
	c.Store("B", normal("B", 2))
	// A is read, so B is now the coldest even though it was stored second.
	if _, ok := c.Lookup("A"); !ok {
		t.Fatal("A must be cached before the read")
	}
	c.Store("C", normal("C", 3))

	if _, ok := c.Lookup("B"); ok {
		t.Error("B was the least recently used subject and must have been evicted")
	}
	if _, ok := c.Lookup("A"); !ok {
		t.Error("A was read more recently than B and must have survived")
	}
	if _, ok := c.Lookup("C"); !ok {
		t.Error("C was just stored and must be cached")
	}
}

// TestAnOpenBreakerEvictsNothing is THE safety test for this half.
//
// The cache exists so that a caller gets a last-known prediction while the
// inference service is unreachable. If a bound could drop an entry during that
// window it would take away the answer at exactly the moment the answer is the
// only one available — a degraded-but-answerable path turned unanswerable.
//
// The property that makes that impossible is structural: the map is written only
// by Store, Predict stores only a NORMAL response, and a NORMAL response cannot
// arrive while the transport is failing. The cache is deliberately sized to ONE
// subject so any eviction at all would be visible.
func TestAnOpenBreakerEvictsNothing(t *testing.T) {
	stub := &stubClient{resp: normal("PF-1", 42)}
	opts := DefaultSyncClientOptions()
	opts.BreakerThreshold = 2
	opts.BreakerCooldown = time.Hour // stays open for the rest of the test
	opts.MaxCachedSubjects = 1
	client, err := NewSyncClientWithStub(stub, opts)
	if err != nil {
		t.Fatalf("NewSyncClientWithStub: %v", err)
	}
	ctx := context.Background()
	fv := FeatureVector{SubjectID: "PF-1", Values: map[FeatureName]FeatureValue{"x": Scalar(1)}}

	if env, perr := client.Predict(ctx, fv); perr != nil || env.Mode != inferencepb.PredictionMode_PREDICTION_MODE_NORMAL {
		t.Fatalf("seeding call: mode=%v err=%v", env.GetMode(), perr)
	}

	// The service goes down and STAYS down. Other subjects keep being asked
	// about throughout — under an eviction rule that fired on reads, or on
	// degraded responses, PF-1's entry would not survive this loop.
	stub.err = errors.New("inference unreachable")
	for i := 0; i < 500; i++ {
		other := FeatureVector{SubjectID: SubjectID(fmt.Sprintf("PF-other-%d", i)), Values: fv.Values}
		if env, _ := client.Predict(ctx, other); env.DegradedReason == "" {
			t.Fatalf("call %d for an uncached subject must be degraded, got %+v", i, env)
		}
		env, _ := client.Predict(ctx, fv)
		if env.Mode != inferencepb.PredictionMode_PREDICTION_MODE_DEGRADED {
			t.Fatalf("call %d: mode=%v want DEGRADED", i, env.Mode)
		}
		if env.DegradedReason == ReasonNoCachedPrediction {
			t.Fatalf("call %d: the cached prediction for PF-1 was evicted during the outage — "+
				"the degraded path this cache exists for became unanswerable", i)
		}
		if env.Value != 42 {
			t.Fatalf("call %d: value=%v want the cached 42", i, env.Value)
		}
	}
	if got := len(client.cache.entries); got != 1 {
		t.Fatalf("cache holds %d subject(s) after 500 failing calls for 500 distinct subjects, want 1 — "+
			"a DEGRADED response must never seed the cache (PRED-02 §1)", got)
	}
}
