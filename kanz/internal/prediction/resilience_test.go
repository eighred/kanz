package prediction

// PRED-13 — degraded-mode fallback + sync timeout / circuit-breaker
// tests. Complements the PRED-07 sync_client_test.go (external
// package, happy-path + no-cache fallbacks) with two things that need
// the internal package:
//
//   - the cache-HIT × trigger-reason matrix: the trigger reason
//     (inference_timeout / circuit_open) is only surfaced when there is
//     a cached value to serve, so these assert the reason mapping the
//     no-cache tests cannot reach.
//   - deterministic circuit-breaker timing via the injected `now` clock
//     (unexported field) — no time.Sleep, so the failed-probe-resets-
//     cooldown and cooldown-boundary behaviours are tested exactly.
//     Same time-injection approach as RISK-13's Detector.now.

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	inferencepb "github.com/kanz-eng/kanz-schemas-go/inference/v1"
)

// stubClient satisfies inferencepb.InferenceServiceClient with a
// configurable response / error / delay. Separate from the external
// test's fakeStub (different package).
type stubClient struct {
	resp  *inferencepb.PredictionEnvelope
	err   error
	delay time.Duration
	calls int
}

func (s *stubClient) Predict(ctx context.Context, _ *inferencepb.FeatureVector, _ ...grpc.CallOption) (*inferencepb.PredictionEnvelope, error) {
	s.calls++
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.resp, s.err
}

func fvFor(subject string) FeatureVector {
	return FeatureVector{
		SubjectID:     SubjectID(subject),
		FeatureSetRef: "equity-momentum:7",
		Values:        map[FeatureName]FeatureValue{"return_5d": Scalar(0.025)},
		AsOf:          time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func normalPred(subject string, value float64) *inferencepb.PredictionEnvelope {
	return &inferencepb.PredictionEnvelope{
		SubjectId:  subject,
		Model:      "vol-forecast@1.4.2",
		Value:      value,
		Confidence: 0.9,
		Mode:       inferencepb.PredictionMode_PREDICTION_MODE_NORMAL,
		AsOf:       timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	}
}

// --- Cache-hit × trigger-reason matrix --------------------------------

// On the cache-HIT fallback path the served envelope carries the live
// trigger reason (not no_cached_prediction): a timeout reuses the
// cached value AND reports inference_timeout. This is the path the
// no-cache tests in sync_client_test.go cannot exercise.
func TestPredict_TimeoutWithCacheHitReportsTimeoutReason(t *testing.T) {
	stub := &stubClient{resp: normalPred("AAPL", 1.23)}
	opts := DefaultSyncClientOptions()
	opts.Timeout = 5 * time.Millisecond
	client := NewSyncClientWithStub(stub, opts)

	// Prime the cache with a successful NORMAL response (no delay, so it
	// beats the 5ms timeout).
	if _, err := client.Predict(context.Background(), fvFor("AAPL")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Now make the call exceed the deadline.
	stub.delay = 50 * time.Millisecond
	stub.resp = nil

	pred, err := client.Predict(context.Background(), fvFor("AAPL"))
	if pred.Mode != inferencepb.PredictionMode_PREDICTION_MODE_DEGRADED {
		t.Errorf("Mode=%v want DEGRADED", pred.Mode)
	}
	if pred.DegradedReason != ReasonInferenceTimeout {
		t.Errorf("DegradedReason=%q want inference_timeout (err=%v)", pred.DegradedReason, err)
	}
	if pred.Value != 1.23 {
		t.Errorf("Value=%v want cached 1.23", pred.Value)
	}
}

// On the cache-HIT fallback path an open circuit reports circuit_open
// while still serving the cached value (and never hitting the wire).
func TestPredict_OpenCircuitWithCacheHitReportsCircuitOpen(t *testing.T) {
	stub := &stubClient{resp: normalPred("AAPL", 2.5)}
	opts := DefaultSyncClientOptions()
	opts.BreakerThreshold = 2
	opts.BreakerCooldown = 1 * time.Hour // stays open
	client := NewSyncClientWithStub(stub, opts)

	// Prime cache with a NORMAL success (also resets the breaker).
	if _, err := client.Predict(context.Background(), fvFor("AAPL")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Trip the breaker: threshold consecutive failures.
	stub.resp = nil
	stub.err = errors.New("down")
	for i := 0; i < opts.BreakerThreshold; i++ {
		_, _ = client.Predict(context.Background(), fvFor("AAPL")) // driving load; failures are the point
	}
	callsBeforeOpen := stub.calls

	pred, _ := client.Predict(context.Background(), fvFor("AAPL"))
	if stub.calls != callsBeforeOpen {
		t.Errorf("stub hit while circuit open: calls=%d want %d", stub.calls, callsBeforeOpen)
	}
	if pred.DegradedReason != ReasonCircuitOpen {
		t.Errorf("DegradedReason=%q want circuit_open", pred.DegradedReason)
	}
	if pred.Value != 2.5 {
		t.Errorf("Value=%v want cached 2.5", pred.Value)
	}
	// Trust-laundering guard: a cached NORMAL must be re-served as
	// DEGRADED, never NORMAL (PRED-02 §1).
	if pred.Mode != inferencepb.PredictionMode_PREDICTION_MODE_DEGRADED {
		t.Errorf("Mode=%v want DEGRADED (cached NORMAL must not be served as NORMAL)", pred.Mode)
	}
}

// --- Feature-translation failure --------------------------------------

// A FeatureValue with an unspecified Kind fails translation before the
// wire call — the client must return a DEGRADED envelope and NOT hit
// the stub, mirroring the empty-SubjectID validation path.
func TestPredict_FeatureTranslationErrorReturnsDegradedNoStubCall(t *testing.T) {
	stub := &stubClient{resp: normalPred("AAPL", 1.0)}
	client := NewSyncClientWithStub(stub, DefaultSyncClientOptions())

	fv := fvFor("AAPL")
	fv.Values["broken"] = FeatureValue{} // Kind == unspecified → translate error

	pred, err := client.Predict(context.Background(), fv)
	if err == nil {
		t.Error("expected translation error")
	}
	if pred == nil || pred.Mode != inferencepb.PredictionMode_PREDICTION_MODE_DEGRADED {
		t.Fatal("envelope must be populated as DEGRADED on translation failure")
	}
	if stub.calls != 0 {
		t.Errorf("stub called despite client-side translation failure: calls=%d", stub.calls)
	}
}

// --- AsOf propagation on no-cache fallback ----------------------------

// The no-cache degraded envelope carries the request's as_of so a
// consumer can reason about freshness even on the synthesized fallback.
func TestPredict_NoCacheFallbackPropagatesAsOf(t *testing.T) {
	stub := &stubClient{err: errors.New("down")}
	client := NewSyncClientWithStub(stub, DefaultSyncClientOptions())

	fv := fvFor("AAPL")
	pred, _ := client.Predict(context.Background(), fv)
	if pred.AsOf == nil {
		t.Fatal("AsOf is nil on fallback envelope")
	}
	if !pred.AsOf.AsTime().Equal(fv.AsOf) {
		t.Errorf("AsOf=%v want %v (request as_of propagated)", pred.AsOf.AsTime(), fv.AsOf)
	}
	if pred.SubjectId != "AAPL" {
		t.Errorf("SubjectId=%q want AAPL", pred.SubjectId)
	}
}

// --- Circuit breaker: deterministic timing ----------------------------

// A failed half-open probe must re-open the breaker AND reset the
// cooldown window, so the next probe waits another full cooldown — the
// behaviour documented in CircuitBreaker but not reachable without
// controlling the clock.
func TestCircuitBreaker_FailedProbeResetsCooldown(t *testing.T) {
	b := NewCircuitBreaker(2, 10*time.Second)
	now := time.Unix(0, 0)
	b.now = func() time.Time { return now }

	// Open the breaker at t=0.
	b.RecordFailure()
	b.RecordFailure()
	if b.State() != "open" {
		t.Fatalf("state=%q want open", b.State())
	}

	// t=11s: past cooldown → half-open, one probe admitted.
	now = time.Unix(11, 0)
	if !b.Allow() {
		t.Fatal("probe not admitted after cooldown")
	}
	// Probe fails → re-open, cooldown window resets to t=11s.
	b.RecordFailure()

	// t=12s: only 1s since the reset (< 10s cooldown) → still open.
	now = time.Unix(12, 0)
	if b.Allow() {
		t.Error("breaker admitted a call 1s after a failed probe (cooldown not reset)")
	}
	// t=22s: 11s since the failed probe → cooldown elapsed, probe again.
	now = time.Unix(22, 0)
	if !b.Allow() {
		t.Error("breaker not half-open after the reset cooldown elapsed")
	}
}

// The cooldown boundary is exclusive: at EXACTLY cooldown the breaker
// is still open; only strictly past it does a probe get admitted.
// Mirrors RISK-11's "boundary is above the budget" discipline.
func TestCircuitBreaker_CooldownBoundaryIsExclusive(t *testing.T) {
	b := NewCircuitBreaker(1, 10*time.Second)
	now := time.Unix(100, 0)
	b.now = func() time.Time { return now }

	b.RecordFailure() // opens at t=100
	now = time.Unix(110, 0)
	if b.State() != "open" || b.Allow() {
		t.Errorf("at exactly cooldown: state=%q allow=%v want open/false", b.State(), b.Allow())
	}
	now = time.Unix(110, 1) // 1ns past
	if b.State() != "half_open" || !b.Allow() {
		t.Errorf("just past cooldown: state=%q allow=%v want half_open/true", b.State(), b.Allow())
	}
}

// A success below the threshold resets the consecutive-failure count,
// so interrupted failures never accumulate to open the circuit.
func TestCircuitBreaker_SuccessResetsConsecutiveFailures(t *testing.T) {
	b := NewCircuitBreaker(3, 1*time.Hour)
	b.RecordFailure()
	b.RecordFailure() // 2 < 3 → still closed
	b.RecordSuccess() // resets the count
	b.RecordFailure()
	b.RecordFailure() // 2 again, not 4 → still closed
	if b.State() != "closed" {
		t.Errorf("state=%q want closed (success must reset the consecutive count)", b.State())
	}
}
