package prediction_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	inferencepb "github.com/kanz-eng/kanz-schemas-go/inference/v1"

	"github.com/kanz-eng/kanz/internal/prediction"
)

// fakeStub satisfies inferencepb.InferenceServiceClient. Tests
// configure response/err and (optionally) a delay to simulate
// slow inference.
type fakeStub struct {
	response *inferencepb.PredictionEnvelope
	err      error
	delay    time.Duration
	calls    int
}

func (f *fakeStub) Predict(ctx context.Context, fv *inferencepb.FeatureVector, opts ...grpc.CallOption) (*inferencepb.PredictionEnvelope, error) {
	f.calls++
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.response, f.err
}

func goodFV() prediction.FeatureVector {
	return prediction.FeatureVector{
		SubjectID:     "AAPL",
		FeatureSetRef: "equity-momentum:7",
		Values: map[prediction.FeatureName]prediction.FeatureValue{
			"return_5d": prediction.Scalar(0.025),
		},
		AsOf: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func normalPrediction(subject string, value float64) *inferencepb.PredictionEnvelope {
	return &inferencepb.PredictionEnvelope{
		SubjectId:  subject,
		Model:      "vol-forecast@1.4.2",
		Value:      value,
		Confidence: 0.9,
		Mode:       inferencepb.PredictionMode_PREDICTION_MODE_NORMAL,
		AsOf:       timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	}
}

// --- Predict happy path ----------------------------------------------

func TestPredict_NormalPathReturnsServerResponse(t *testing.T) {
	stub := &fakeStub{response: normalPrediction("AAPL", 0.73)}
	client := prediction.NewSyncClientWithStub(stub, prediction.DefaultSyncClientOptions())
	defer client.Close()

	pred, err := client.Predict(context.Background(), goodFV())
	if err != nil {
		t.Fatalf("err=%v want nil on normal path", err)
	}
	if pred.Mode != inferencepb.PredictionMode_PREDICTION_MODE_NORMAL {
		t.Errorf("Mode=%v want NORMAL", pred.Mode)
	}
	if pred.Value != 0.73 {
		t.Errorf("Value=%v want 0.73", pred.Value)
	}
	if stub.calls != 1 {
		t.Errorf("calls=%d want 1", stub.calls)
	}
}

func TestPredict_CachesOnlyNormalResponses(t *testing.T) {
	// PRED-02 §1 — DEGRADED responses must NOT seed the cache.
	stub := &fakeStub{response: &inferencepb.PredictionEnvelope{
		SubjectId: "AAPL",
		Mode:      inferencepb.PredictionMode_PREDICTION_MODE_DEGRADED,
	}}
	client := prediction.NewSyncClientWithStub(stub, prediction.DefaultSyncClientOptions())

	_, _ = client.Predict(context.Background(), goodFV())

	// Trigger fallback via stub returning error; fallback should
	// HIT cache only if cache was populated. With DEGRADED-cache
	// rejection in effect, no entry → no_cached_prediction.
	stub.response = nil
	stub.err = errors.New("server gone")
	pred, _ := client.Predict(context.Background(), goodFV())
	if pred.DegradedReason != prediction.ReasonNoCachedPrediction {
		t.Errorf("DegradedReason=%q want no_cached_prediction (DEGRADED responses must not seed cache)", pred.DegradedReason)
	}
}

// --- Transport error fallback ----------------------------------------

func TestPredict_TransportErrorNoCacheReturnsNoCachedPrediction(t *testing.T) {
	stub := &fakeStub{err: errors.New("connection refused")}
	client := prediction.NewSyncClientWithStub(stub, prediction.DefaultSyncClientOptions())

	pred, err := client.Predict(context.Background(), goodFV())

	// PRED-02 §1: envelope is ALWAYS populated, even on error.
	if pred == nil {
		t.Fatal("envelope is nil — must be populated even on failure")
	}
	if pred.Mode != inferencepb.PredictionMode_PREDICTION_MODE_DEGRADED {
		t.Errorf("Mode=%v want DEGRADED", pred.Mode)
	}
	// Cache miss → no_cached_prediction (contract: the degraded_reason
	// field reports the fallback outcome, not the trigger; the trigger
	// is observable via err). The cache-hit path reports the trigger
	// reason — see the resilience matrix tests.
	if pred.DegradedReason != prediction.ReasonNoCachedPrediction {
		t.Errorf("DegradedReason=%q want no_cached_prediction", pred.DegradedReason)
	}
	// err non-nil = observable fault; caller can log/metric the trigger.
	if err == nil {
		t.Error("err is nil — transport failures should be observable")
	}
}

func TestPredict_CacheHitOnFallbackReusesCachedValue(t *testing.T) {
	// Step 1: prime cache with a successful NORMAL call.
	stub := &fakeStub{response: normalPrediction("AAPL", 1.23)}
	client := prediction.NewSyncClientWithStub(stub, prediction.DefaultSyncClientOptions())
	if _, err := client.Predict(context.Background(), goodFV()); err != nil {
		t.Fatalf("seed call: %v", err)
	}
	// Step 2: server fails; fallback should reuse cached 1.23.
	stub.response = nil
	stub.err = errors.New("server down")
	pred, _ := client.Predict(context.Background(), goodFV())
	if pred.Mode != inferencepb.PredictionMode_PREDICTION_MODE_DEGRADED {
		t.Errorf("Mode=%v want DEGRADED", pred.Mode)
	}
	if pred.Value != 1.23 {
		t.Errorf("Value=%v want 1.23 (cached value preserved)", pred.Value)
	}
	if pred.DegradedReason != prediction.ReasonInferenceUnavailable {
		t.Errorf("DegradedReason=%q want inference_unavailable", pred.DegradedReason)
	}
}

// --- Timeout fallback ------------------------------------------------

func TestPredict_TimeoutNoCacheReturnsDegraded(t *testing.T) {
	stub := &fakeStub{
		response: normalPrediction("AAPL", 1.0),
		delay:    50 * time.Millisecond,
	}
	opts := prediction.DefaultSyncClientOptions()
	opts.Timeout = 5 * time.Millisecond // shorter than delay
	client := prediction.NewSyncClientWithStub(stub, opts)

	pred, err := client.Predict(context.Background(), goodFV())
	if pred.Mode != inferencepb.PredictionMode_PREDICTION_MODE_DEGRADED {
		t.Errorf("Mode=%v want DEGRADED", pred.Mode)
	}
	// Cache miss → no_cached_prediction; err carries the deadline so the
	// timeout trigger is observable. The timeout→inference_timeout reason
	// mapping is asserted on the cache-hit path (resilience matrix).
	if pred.DegradedReason != prediction.ReasonNoCachedPrediction {
		t.Errorf("DegradedReason=%q want no_cached_prediction, err=%v", pred.DegradedReason, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err=%v want context.DeadlineExceeded", err)
	}
}

// --- Circuit breaker -------------------------------------------------

func TestPredict_CircuitOpensAfterThresholdFailures(t *testing.T) {
	stub := &fakeStub{err: errors.New("down")}
	opts := prediction.DefaultSyncClientOptions()
	opts.BreakerThreshold = 3
	opts.BreakerCooldown = 1 * time.Hour // long enough to stay open
	client := prediction.NewSyncClientWithStub(stub, opts)

	// First N failures attempt the call; subsequent attempts
	// short-circuit and never hit the stub.
	for i := 0; i < opts.BreakerThreshold; i++ {
		_, _ = client.Predict(context.Background(), goodFV()) // driving the breaker; the error is expected
	}
	stubCallsBeforeOpen := stub.calls

	// One more call — circuit is open, stub.calls must NOT
	// increment.
	pred, _ := client.Predict(context.Background(), goodFV())
	if stub.calls != stubCallsBeforeOpen {
		t.Errorf("stub.calls=%d want %d (open circuit must short-circuit)", stub.calls, stubCallsBeforeOpen)
	}
	// Cache was never primed (every call failed), so the open-circuit
	// fallback misses the cache → no_cached_prediction. The circuit_open
	// reason surfaces only when there is a cached value to serve — see
	// the resilience matrix test.
	if pred.DegradedReason != prediction.ReasonNoCachedPrediction {
		t.Errorf("DegradedReason=%q want no_cached_prediction", pred.DegradedReason)
	}
}

func TestPredict_CircuitClosesAfterSuccessfulProbe(t *testing.T) {
	stub := &fakeStub{err: errors.New("down")}
	opts := prediction.DefaultSyncClientOptions()
	opts.BreakerThreshold = 2
	opts.BreakerCooldown = 5 * time.Millisecond
	client := prediction.NewSyncClientWithStub(stub, opts)

	// Trip the breaker.
	for i := 0; i < opts.BreakerThreshold; i++ {
		_, _ = client.Predict(context.Background(), goodFV()) // driving the breaker; the error is expected
	}
	// Wait past cooldown.
	time.Sleep(10 * time.Millisecond)

	// Server recovers — probe call succeeds, breaker closes.
	stub.err = nil
	stub.response = normalPrediction("AAPL", 1.0)
	pred, err := client.Predict(context.Background(), goodFV())
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if pred.Mode != inferencepb.PredictionMode_PREDICTION_MODE_NORMAL {
		t.Errorf("Mode=%v want NORMAL after recovery", pred.Mode)
	}
}

// --- Validation ------------------------------------------------------

func TestPredict_EmptySubjectIDReturnsDegradedAndError(t *testing.T) {
	stub := &fakeStub{response: normalPrediction("AAPL", 1.0)}
	client := prediction.NewSyncClientWithStub(stub, prediction.DefaultSyncClientOptions())

	fv := goodFV()
	fv.SubjectID = ""
	pred, err := client.Predict(context.Background(), fv)
	if err == nil {
		t.Error("expected error for empty SubjectID")
	}
	if pred == nil || pred.Mode != inferencepb.PredictionMode_PREDICTION_MODE_DEGRADED {
		t.Error("envelope must still be populated as DEGRADED on validation failure")
	}
	if stub.calls != 0 {
		t.Error("stub called despite client-side validation failure")
	}
}

// --- PredictionCache -------------------------------------------------

func TestPredictionCache_OverwriteReplacesPrevious(t *testing.T) {
	c := prediction.NewPredictionCache()
	c.Store("AAPL", normalPrediction("AAPL", 1.0))
	c.Store("AAPL", normalPrediction("AAPL", 2.0))
	got, ok := c.Lookup("AAPL")
	if !ok || got.Value != 2.0 {
		t.Errorf("Lookup got value=%v ok=%v want 2.0", got.Value, ok)
	}
}

func TestPredictionCache_NilStoreIsNoOp(t *testing.T) {
	c := prediction.NewPredictionCache()
	c.Store("AAPL", nil)
	c.Store("", normalPrediction("", 1.0))
	if _, ok := c.Lookup("AAPL"); ok {
		t.Error("nil-prediction store wrote phantom entry")
	}
	if _, ok := c.Lookup(""); ok {
		t.Error("empty-subject-id store wrote phantom entry")
	}
}

// --- CircuitBreaker --------------------------------------------------

func TestCircuitBreaker_StateTransitions(t *testing.T) {
	b := prediction.NewCircuitBreaker(2, 100*time.Millisecond)
	if got := b.State(); got != "closed" {
		t.Errorf("initial=%q want closed", got)
	}
	b.RecordFailure()
	if got := b.State(); got != "closed" {
		t.Errorf("after 1 failure=%q want closed (below threshold)", got)
	}
	b.RecordFailure()
	if got := b.State(); got != "open" {
		t.Errorf("after threshold failures=%q want open", got)
	}
	// Cool down.
	time.Sleep(110 * time.Millisecond)
	if got := b.State(); got != "half_open" {
		t.Errorf("after cooldown=%q want half_open", got)
	}
	b.RecordSuccess()
	if got := b.State(); got != "closed" {
		t.Errorf("after probe success=%q want closed", got)
	}
}
