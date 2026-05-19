package prediction_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	inferencepb "github.com/kanz-eng/kanz-schemas-go/inference/v1"

	"github.com/kanz-eng/kanz/internal/prediction"
	"github.com/kanz-eng/kanz/pkg/bus"
)

var baseTime = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// captureClient mirrors the helper in the other publish-side tests
// (risk/publish, EVT-21a contract). Records every Publish call.
type captureClient struct {
	sent []bus.Message
}

func (c *captureClient) Publish(_ context.Context, m bus.Message) error {
	c.sent = append(c.sent, m)
	return nil
}
func (c *captureClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *captureClient) Close() error { return nil }

func newPublisher(t *testing.T) (*prediction.Publisher, *captureClient) {
	t.Helper()
	cc := &captureClient{}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source:          "feature-svc/test",
		ProducerVersion: "feature-1.0.0",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	pub, err := prediction.NewPublisher(prod)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	return pub, cc
}

func mkFV(values map[prediction.FeatureName]prediction.FeatureValue) prediction.FeatureVector {
	return prediction.FeatureVector{
		SubjectID:     "AAPL",
		FeatureSetRef: "equity-momentum:7",
		Values:        values,
		AsOf:          baseTime,
	}
}

func TestNewPublisher_NilProducerRejected(t *testing.T) {
	if _, err := prediction.NewPublisher(nil); err == nil {
		t.Fatal("expected error for nil producer")
	}
}

func TestPublishFeatures_HappyPathEmitsValidEnvelope(t *testing.T) {
	pub, cc := newPublisher(t)
	fv := mkFV(map[prediction.FeatureName]prediction.FeatureValue{
		"return_5d":    prediction.Scalar(0.025),
		"sector":       prediction.Categorical("TECH"),
		"day_of_week":  prediction.Ordinal(2),
		"embedding_v1": prediction.Vector([]float64{0.1, 0.2, 0.3}),
	})
	if err := pub.PublishFeatures(context.Background(), fv); err != nil {
		t.Fatalf("PublishFeatures: %v", err)
	}
	if got := len(cc.sent); got != 1 {
		t.Fatalf("messages=%d want 1", got)
	}
	msg := cc.sent[0]
	if msg.Subject != prediction.EventTypeFeatureComputed {
		t.Errorf("Subject=%q want %q", msg.Subject, prediction.EventTypeFeatureComputed)
	}
	if string(msg.Key) != "AAPL" {
		t.Errorf("Key=%q want AAPL", msg.Key)
	}
	env, payloadBytes, err := bus.Unframe(msg.Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if err := bus.Validate(env); err != nil {
		t.Errorf("emitted envelope fails Validate: %v", err)
	}
	if env.EventClass != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Errorf("EventClass=%v want FACT", env.EventClass)
	}
	if env.Domain != "inference" {
		t.Errorf("Domain=%q want inference", env.Domain)
	}

	var got inferencepb.FeatureVector
	if err := proto.Unmarshal(payloadBytes, &got); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if got.SubjectId != "AAPL" || got.FeatureSetRef != "equity-momentum:7" {
		t.Errorf("payload mismatch: SubjectId=%q FeatureSetRef=%q", got.SubjectId, got.FeatureSetRef)
	}
	if len(got.Values) != 4 {
		t.Fatalf("Values len=%d want 4", len(got.Values))
	}
}

func TestPublishFeatures_AllFourKindsTranslate(t *testing.T) {
	pub, cc := newPublisher(t)
	fv := mkFV(map[prediction.FeatureName]prediction.FeatureValue{
		"sc": prediction.Scalar(1.5),
		"or": prediction.Ordinal(42),
		"ca": prediction.Categorical("BUY"),
		"vc": prediction.Vector([]float64{1, 2, 3, 4}),
	})
	if err := pub.PublishFeatures(context.Background(), fv); err != nil {
		t.Fatalf("PublishFeatures: %v", err)
	}
	_, payloadBytes, _ := bus.Unframe(cc.sent[0].Body)
	var got inferencepb.FeatureVector
	if err := proto.Unmarshal(payloadBytes, &got); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if sc, ok := got.Values["sc"].Value.(*inferencepb.FeatureValue_Scalar); !ok || sc.Scalar != 1.5 {
		t.Errorf("scalar mismatch: %v", got.Values["sc"])
	}
	if or, ok := got.Values["or"].Value.(*inferencepb.FeatureValue_Ordinal); !ok || or.Ordinal != 42 {
		t.Errorf("ordinal mismatch: %v", got.Values["or"])
	}
	if ca, ok := got.Values["ca"].Value.(*inferencepb.FeatureValue_Categorical); !ok || ca.Categorical != "BUY" {
		t.Errorf("categorical mismatch: %v", got.Values["ca"])
	}
	vc, ok := got.Values["vc"].Value.(*inferencepb.FeatureValue_Vector)
	if !ok || len(vc.Vector.Values) != 4 || vc.Vector.Values[3] != 4 {
		t.Errorf("vector mismatch: %v", got.Values["vc"])
	}
}

func TestPublishFeatures_MissingSubjectIDRejected(t *testing.T) {
	pub, _ := newPublisher(t)
	fv := mkFV(map[prediction.FeatureName]prediction.FeatureValue{"x": prediction.Scalar(1)})
	fv.SubjectID = ""
	if err := pub.PublishFeatures(context.Background(), fv); err == nil {
		t.Fatal("expected error for empty SubjectID")
	}
}

func TestPublishFeatures_MissingFeatureSetRefRejected(t *testing.T) {
	pub, _ := newPublisher(t)
	fv := mkFV(map[prediction.FeatureName]prediction.FeatureValue{"x": prediction.Scalar(1)})
	fv.FeatureSetRef = ""
	if err := pub.PublishFeatures(context.Background(), fv); err == nil {
		t.Fatal("expected error for empty FeatureSetRef")
	}
}

func TestPublishFeatures_EmptyValuesRejected(t *testing.T) {
	pub, _ := newPublisher(t)
	fv := mkFV(map[prediction.FeatureName]prediction.FeatureValue{})
	if err := pub.PublishFeatures(context.Background(), fv); err == nil {
		t.Fatal("expected error for empty Values")
	}
}

func TestPublishFeatures_ZeroAsOfRejected(t *testing.T) {
	pub, _ := newPublisher(t)
	fv := mkFV(map[prediction.FeatureName]prediction.FeatureValue{"x": prediction.Scalar(1)})
	fv.AsOf = time.Time{}
	if err := pub.PublishFeatures(context.Background(), fv); err == nil {
		t.Fatal("expected error for zero AsOf")
	}
}

func TestPublishFeatures_UnspecifiedKindRejected(t *testing.T) {
	pub, _ := newPublisher(t)
	fv := mkFV(map[prediction.FeatureName]prediction.FeatureValue{
		"bad": {Kind: prediction.ValueKindUnspecified},
	})
	if err := pub.PublishFeatures(context.Background(), fv); err == nil {
		t.Fatal("expected error for UNSPECIFIED kind")
	}
}

func TestVector_DefensiveCopy(t *testing.T) {
	// Vector() should defensive-copy so caller mutations don't
	// affect already-constructed FeatureValues.
	orig := []float64{1, 2, 3}
	v := prediction.Vector(orig)
	orig[0] = 99
	if v.Vector[0] == 99 {
		t.Error("Vector did not defensive-copy — caller mutation bled into FeatureValue")
	}
}

func TestPublishFeatures_PartitionKeyIsSubjectID(t *testing.T) {
	// Per-subject ordering: features for one subject must arrive
	// at the inference worker in order, so partition_key MUST be
	// SubjectID end-to-end.
	pub, cc := newPublisher(t)
	fv := mkFV(map[prediction.FeatureName]prediction.FeatureValue{"x": prediction.Scalar(1)})
	fv.SubjectID = "MSFT"
	if err := pub.PublishFeatures(context.Background(), fv); err != nil {
		t.Fatalf("PublishFeatures: %v", err)
	}
	if string(cc.sent[0].Key) != "MSFT" {
		t.Errorf("Key=%q want MSFT", cc.sent[0].Key)
	}
	env, _, _ := bus.Unframe(cc.sent[0].Body)
	if env.PartitionKey != "MSFT" {
		t.Errorf("envelope.PartitionKey=%q want MSFT", env.PartitionKey)
	}
}
