package app_test

// THE LOOP, OVER THE REAL BUS (AI-M1).
//
// The Go half computes features and publishes them. The Python half scores them and
// publishes the prediction. Both halves were written to the same contract — the subject
// `inference.feature.computed`, the FeatureVector, the PredictionEnvelope — and neither had
// ever spoken to the other. Nothing served inference.v1.InferenceService and nothing called
// it; internal/prediction had zero importers outside its own tests.
//
// This drives the whole thing black-box across the language boundary: publish ONE
// FeatureVector as the risk engine now does, and assert a REAL prediction comes back on the
// bus, carrying a confidence and an explanation.
//
// Run it with NATS up and the Python inference service on the same bus:
//
//	KANZ_INFERENCE_MODELS=models.json \
//	KANZ_INFERENCE_NATS_URL=nats://localhost:4222 python -m kanz_inference &
//
//	TEST_NATS_URL=nats://localhost:4222 TEST_INFERENCE_ON_BUS=1 \
//	  go test -run TestIntegration_TheModelScoresTheEnginesFeatures ./services/risk-engine/internal/app/

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	inferencepb "github.com/kanz-eng/kanz-schemas-go/inference/v1"
	"google.golang.org/protobuf/proto"

	"github.com/kanz-eng/kanz/internal/prediction"
	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/services/risk-engine/internal/app"
)

// SubjectPredictionScored is what the Python publisher emits (kanz_inference/publish.py).
const SubjectPredictionScored = "inference.prediction.scored"

func TestIntegration_TheModelScoresTheEnginesFeatures(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL to run the cross-language inference loop")
	}
	// This test needs TWO things: a live NATS *and* the Python inference service consuming
	// from it. Gate on BOTH — a test that gates on half its preconditions does not skip when
	// the other half is missing, it FAILS, and that is how a real-bus test ends up disabled.
	if os.Getenv("TEST_INFERENCE_ON_BUS") == "" {
		t.Skip("set TEST_INFERENCE_ON_BUS=1 with `python -m kanz_inference` on TEST_NATS_URL")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "ai-m1-it"})
	if err != nil {
		t.Fatalf("dial NATS: %v", err)
	}
	defer client.Close()

	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}

	var (
		mu   sync.Mutex
		got  *inferencepb.PredictionEnvelope
		seen = make(chan struct{})
		once sync.Once
	)
	go func() {
		_ = consumer.Subscribe(ctx, SubjectPredictionScored, "ai-m1-it",
			func(_ context.Context, _ *envelopepb.Envelope, payload []byte) error {
				var p inferencepb.PredictionEnvelope
				if err := proto.Unmarshal(payload, &p); err != nil {
					return nil
				}
				if p.GetSubjectId() != "fund-alpha" {
					return nil
				}
				mu.Lock()
				got = &p
				mu.Unlock()
				once.Do(func() { close(seen) })
				return nil
			})
	}()
	time.Sleep(500 * time.Millisecond) // let the subscription land before we publish

	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source: "ai-m1-it", ProducerVersion: "test", Tenant: "acme",
	})
	if err != nil {
		t.Fatalf("producer: %v", err)
	}
	features, err := prediction.NewPublisher(producer)
	if err != nil {
		t.Fatalf("feature publisher: %v", err)
	}

	// EXACTLY what the risk engine now publishes on every recompute.
	fv := prediction.FeatureVector{
		SubjectID:     "fund-alpha",
		FeatureSetRef: app.FeatureSetPortfolioRisk,
		AsOf:          time.Now().UTC(),
		Values: map[prediction.FeatureName]prediction.FeatureValue{
			"GrossExposure": prediction.Scalar(5.0),
			"VaR99":         prediction.Scalar(1.0),
		},
	}
	if err := features.PublishFeatures(ctx, fv); err != nil {
		t.Fatalf("publish features: %v — is `inference.>` bound to the INFERENCE stream?", err)
	}

	select {
	case <-seen:
	case <-time.After(20 * time.Second):
		t.Fatal("NO PREDICTION CAME BACK. The engine published a FeatureVector and nothing scored it " +
			"— which is exactly the state AI-M1 exists to end: a fully-specified AI contract with no " +
			"model behind it. Is `python -m kanz_inference` running on this bus with a PRIMARY model?")
	}

	mu.Lock()
	p := got
	mu.Unlock()

	if p.GetModel() == "" {
		t.Error("the prediction does not say which model made it — it cannot be audited")
	}
	if p.GetMode() != inferencepb.PredictionMode_PREDICTION_MODE_NORMAL {
		t.Errorf("mode = %v (%s), want NORMAL", p.GetMode(), p.GetDegradedReason())
	}
	if p.GetConfidence() <= 0 || p.GetConfidence() > 1 {
		t.Errorf("confidence = %v, want (0,1] — a prediction that cannot say how much it trusts "+
			"itself must be DEGRADED, not confident", p.GetConfidence())
	}
	// THE XAI PAYLOAD, produced by the real permutation-Shapley explainer across the wire.
	// A risk desk that cannot attribute a number cannot sign off on it.
	if len(p.GetExplanation()) == 0 {
		t.Fatal("the prediction carried NO explanation — the explainer exists and was not connected")
	}
	if _, ok := p.GetExplanation()["GrossExposure"]; !ok {
		t.Errorf("explanation does not attribute GrossExposure: %v", p.GetExplanation())
	}
	t.Logf("scored by %s: value=%.4f confidence=%.4f explanation=%v",
		p.GetModel(), p.GetValue(), p.GetConfidence(), p.GetExplanation())
}
