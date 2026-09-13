// THE ENVELOPE THIS CLI PUBLISHES, PINNED AGAINST A REAL PRODUCER, and the
// validation that must happen before it (#1010).
//
// run() dials a broker before it constructs anything, so without this file every
// field of the envelope would be unreachable without one — the shape
// pkg/bus/producer.go records in past tense and the shape that put a tenant-less
// producer into production in services/accounting.
//
// WHY IT MATTERS MORE HERE THAN ON AN APPEND-ONLY SUBJECT, and more than on
// kanz-household's. The model subject is COMPACTED to the last message per model,
// so a bad publish does not sit beside the good history — it REPLACES the target
// allocation, and unlike a valuation, which is wrong about one household, a model
// is wrong about EVERY household on that risk profile at once.
//
// TIER-B: a REAL bus.Producer over a FAKE bus.Client. The Producer stamps
// event_id / publish_time / producer_sequence and runs Validate, so a double at
// the Event level would remove exactly the thing under test.
package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	wealthpb "github.com/eighred/kanz/kanz-schemas-go/wealth/v1"

	"github.com/eighred/kanz/internal/wealth"
	"github.com/eighred/kanz/pkg/bus"
)

// captureClient is a bus.Client that records the framed wire bytes — the Tier-B
// helper from cmd/kanz-household/publish_test.go and internal/risk/publish.
type captureClient struct{ sent []bus.Message }

func (c *captureClient) Publish(_ context.Context, m bus.Message) error {
	c.sent = append(c.sent, m)
	return nil
}
func (c *captureClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *captureClient) Close() error { return nil }

func newTestProducer(t *testing.T, tenant string) (*bus.Producer, *captureClient) {
	t.Helper()
	cc := &captureClient{}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source:          "kanz-model",
		ProducerVersion: "1",
		Tenant:          tenant,
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	return prod, cc
}

func model() *wealthpb.ModelPortfolio {
	return &wealthpb.ModelPortfolio{
		ModelId:             "growth-2026",
		Name:                "Growth",
		RiskProfile:         wealthpb.RiskProfile_RISK_PROFILE_GROWTH,
		ExactTargetWeights:  map[string]string{"BTC-USD": "0.6", "ETH-USD": "0.4"},
		ExactDriftTolerance: "0.05", ArithmeticVersion: 2,
		RecordedBy: "operator:akif",
		Reason:     "IC review 2026-09",
	}
}

// THE ONE THAT MATTERS. Every field asserted here is one Validate rejects when it
// is wrong or absent, so this is the test that would fail if this CLI carried the
// accounting cash producer's defect.
func TestModelEventPublishesAValidFactEnvelope(t *testing.T) {
	prod, cc := newTestProducer(t, "eighred")

	if err := prod.Publish(context.Background(), modelEvent(options{tenant: "eighred"}, model())); err != nil {
		t.Fatalf("Publish: %v — this CLI's envelope does not survive bus.Validate, which is the "+
			"failure an operator would meet the first time they used it", err)
	}
	if len(cc.sent) != 1 {
		t.Fatalf("published %d messages, want 1", len(cc.sent))
	}

	env, _, err := bus.Unframe(cc.sent[0].Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if err := bus.Validate(env); err != nil {
		t.Errorf("the emitted envelope fails Validate: %v", err)
	}
	if got := env.GetTenantId(); got != "eighred" {
		t.Errorf("tenant_id = %q, want eighred — a missing tenant is what the bus rejects first", got)
	}
	if got := env.GetEventClass(); got != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Errorf("event_class = %v, want FACT", got)
	}
	if got := env.GetDomain(); got != wealth.Domain {
		t.Errorf("domain = %q, want %q", got, wealth.Domain)
	}
	if got := env.GetPayloadSchemaRef(); got != "wealth.v1.ModelPortfolio:1" {
		t.Errorf("payload_schema_ref = %q", got)
	}
	// FACT rule (pkg/bus/validate.go): idempotency_key == event_id.
	if env.GetIdempotencyKey() != env.GetEventId() {
		t.Errorf("idempotency_key %q != event_id %q — the FACT rule Validate enforces",
			env.GetIdempotencyKey(), env.GetEventId())
	}
}

// THE SUBJECT AND THE EVENT TYPE ARE DIFFERENT STRINGS HERE, deliberately, the
// same split kanz-household makes: the subject is tenant-prefixed (where the
// message lands); the type is not (what the message is).
func TestModelSubjectIsTenantScopedAndTypeIsNot(t *testing.T) {
	prod, cc := newTestProducer(t, "eighred")
	if err := prod.Publish(context.Background(), modelEvent(options{tenant: "eighred"}, model())); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	wantSubject := wealth.SubjectModelFor("eighred", "growth-2026")
	if got := cc.sent[0].Subject; got != wantSubject {
		t.Errorf("subject = %q, want %q — a model on the wrong subject becomes another tenant's "+
			"target allocation", got, wantSubject)
	}
	if !strings.Contains(wantSubject, "eighred") {
		t.Fatalf("this test asserts tenant scoping but %q does not contain the tenant — the subject "+
			"helper changed shape and the assertion above no longer means what it says", wantSubject)
	}
	env, _, err := bus.Unframe(cc.sent[0].Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if got := env.GetEventType(); got != wealth.EventTypeModelPublished {
		t.Errorf("event_type = %q, want the un-prefixed %q", got, wealth.EventTypeModelPublished)
	}
	if env.GetEventType() == cc.sent[0].Subject {
		t.Error("event_type and subject are now the same string. On this publisher they must differ: " +
			"the subject carries the tenant and the model id, the type does not")
	}
}

// THE COMPACTION KEY. This subject keeps only the last message per model, so the
// partition key decides WHOSE target allocation this message overwrites.
func TestModelPartitionKeyIsTheModelID(t *testing.T) {
	prod, cc := newTestProducer(t, "eighred")
	if err := prod.Publish(context.Background(), modelEvent(options{tenant: "eighred"}, model())); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := string(cc.sent[0].Key); got != "growth-2026" {
		t.Errorf("partition key = %q, want the model id. This subject is COMPACTED to the last "+
			"message per model, so a wrong key replaces a DIFFERENT model's target allocation and "+
			"every household on that profile is measured against the replacement", got)
	}
}

// A TENANT-LESS RUN IS REFUSED BY THE BUS rather than published blank.
func TestModelEventWithNoTenantIsRefused(t *testing.T) {
	prod, cc := newTestProducer(t, "")

	err := prod.Publish(context.Background(), modelEvent(options{tenant: ""}, model()))
	if err == nil {
		t.Fatal("a tenant-less model portfolio was published. On the live spine bus.Validate " +
			"refuses it, so this CLI would report success while the FACT never left")
	}
	if !strings.Contains(err.Error(), "tenant") {
		t.Errorf("the refusal must name the tenant; got %v", err)
	}
	if len(cc.sent) != 0 {
		t.Errorf("a refused envelope still reached the transport (%d message(s))", len(cc.sent))
	}
}

// VALIDATION HAPPENS BEFORE THE NETWORK, and it is the SAME rule the consumer
// applies. Each case here is a model that would be admitted to nothing: it DLQs at
// the wealth service's catalogue and leaves the risk profile it was meant to serve
// with no target allocation, while this tool printed PUBLISHED.
//
// Written as a table over loadModel — the function that reads the operator's file
// — rather than over Validate directly, because the failure being pinned is "this
// CLI published it", not "the domain rule exists".
func TestLoadModelRefusesAModelTheCatalogueWouldNotAdmit(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "no drift tolerance",
			body:    `{"arithmeticVersion":2,"modelId":"m1","riskProfile":"RISK_PROFILE_GROWTH","exactTargetWeights":{"BTC-USD":"1.0"}}`,
			wantErr: "drift_tolerance",
		},
		{
			name:    "tolerance wider than the whole book",
			body:    `{"arithmeticVersion":2,"modelId":"m1","riskProfile":"RISK_PROFILE_GROWTH","exactDriftTolerance":"1.5","exactTargetWeights":{"BTC-USD":"1.0"}}`,
			wantErr: "drift_tolerance",
		},
		{
			name:    "unspecified risk profile",
			body:    `{"arithmeticVersion":2,"modelId":"m1","exactDriftTolerance":"0.05","exactTargetWeights":{"BTC-USD":"1.0"}}`,
			wantErr: "risk_profile",
		},
		{
			name:    "weights do not sum to 1",
			body:    `{"arithmeticVersion":2,"modelId":"m1","riskProfile":"RISK_PROFILE_GROWTH","exactDriftTolerance":"0.05","exactTargetWeights":{"BTC-USD":"0.6","ETH-USD":"0.3"}}`,
			wantErr: "sum to",
		},
		{
			name:    "no targets at all",
			body:    `{"arithmeticVersion":2,"modelId":"m1","riskProfile":"RISK_PROFILE_GROWTH","exactDriftTolerance":"0.05"}`,
			wantErr: "target_weights",
		},
		{
			name:    "no model id",
			body:    `{"arithmeticVersion":2,"riskProfile":"RISK_PROFILE_GROWTH","exactDriftTolerance":"0.05","exactTargetWeights":{"BTC-USD":"1.0"}}`,
			wantErr: "model_id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "model.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			_, err := loadModel(path, "operator:akif", "IC review")
			if err == nil {
				t.Fatalf("loadModel accepted a model the wealth catalogue refuses. It would be "+
					"published to a COMPACTED subject, DLQ at every consumer on every boot, and "+
					"leave the risk profile with no target allocation — while this CLI printed "+
					"PUBLISHED. body=%s", tc.body)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("refusal does not name %q, so an operator cannot tell what to fix: %v", tc.wantErr, err)
			}
		})
	}
}

// PROVENANCE IS STAMPED ON THE PAYLOAD, not just printed. This subject is
// compacted, so the model this one replaced is gone and -by/-reason are the only
// record of who changed the firm's target allocation and why.
func TestLoadModelStampsProvenanceOntoThePayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model.json")
	body := `{"arithmeticVersion":2,"modelId":"m1","riskProfile":"RISK_PROFILE_BALANCED","exactDriftTolerance":"0.03","exactTargetWeights":{"BTC-USD":"0.5","ETH-USD":"0.5"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	mp, err := loadModel(path, "operator:akif", "IC review 2026-09")
	if err != nil {
		t.Fatalf("loadModel: %v", err)
	}
	if mp.GetRecordedBy() != "operator:akif" || mp.GetReason() != "IC review 2026-09" {
		t.Errorf("recorded_by/reason = %q/%q — the file carried neither, so if this CLI only "+
			"printed them the published model would be attributable to nobody and the model it "+
			"replaced is already gone", mp.GetRecordedBy(), mp.GetReason())
	}
}
