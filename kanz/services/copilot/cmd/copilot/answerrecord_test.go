package main

// THE ENVELOPE, NOT THE ENCODER (#245, #971).
//
// Tier-B: a REAL bus.Producer over a fake bus.Client. The fake sits at the
// TRANSPORT level and receives wire bytes, so stamping, bus.Validate and framing
// all really run and only the socket is mocked. An Event-level double proves
// nothing here, because it is precisely Validate that it skips — the shape
// pkg/bus/producer.go records failing on the first real broker.
//
// WHAT IT CATCHES HERE SPECIFICALLY. This record is published with a PER-RECORD
// tenant rather than a producer fallback, because the copilot serves many tenants
// from one process and its producer is deliberately built with no
// ProducerConfig.Tenant. If the tenant did not reach the envelope, bus.Validate
// would refuse every answer record — the copilot would answer normally and record
// nothing, which is the pre-#971 state with the added cost of looking wired.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"

	"github.com/eighred/kanz/pkg/bus"
)

type answerCaptureClient struct {
	mu   sync.Mutex
	sent []bus.Message
}

func (c *answerCaptureClient) Publish(_ context.Context, m bus.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, m)
	return nil
}
func (c *answerCaptureClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *answerCaptureClient) Close() error { return nil }

func (c *answerCaptureClient) messages() []bus.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]bus.Message(nil), c.sent...)
}

func answerRig(t *testing.T) (*busAnswerRecorder, *answerCaptureClient) {
	t.Helper()
	cc := &answerCaptureClient{}
	// The composition root's own config: NO Tenant, because the tenant is stamped
	// per record from the asking principal.
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{Source: "copilot", ProducerVersion: "test"})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	return &busAnswerRecorder{producer: prod}, cc
}

func sampleRecord() *observationpb.AgentAnswerRecord {
	return &observationpb.AgentAnswerRecord{
		AnswerId:         "ans-1",
		Agent:            "copilot",
		PrincipalSubject: "alice@desk",
		TenantId:         "acme",
		Models:           []string{"opus-x/18"},
		Outcome:          observationpb.AgentAnswerOutcome_AGENT_ANSWER_OUTCOME_ANSWERED,
		Grounded:         true,
		Turns:            1,
		AnsweredAt:       timestamppb.New(time.Date(2026, 9, 2, 15, 0, 0, 0, time.UTC)),
	}
}

// THE ENVELOPE A REAL PRODUCER STAMPS MUST PASS bus.Validate.
func TestAnswerRecordEnvelopeValidates(t *testing.T) {
	rec, cc := answerRig(t)
	if err := rec.RecordAnswer(context.Background(), sampleRecord()); err != nil {
		t.Fatalf("RecordAnswer: %v", err)
	}
	msgs := cc.messages()
	if len(msgs) != 1 {
		t.Fatalf("captured %d messages, want 1", len(msgs))
	}
	env, payload, err := bus.Unframe(msgs[0].Body)
	if err != nil {
		t.Fatalf("unframe: %v", err)
	}
	if err := bus.Validate(env); err != nil {
		t.Fatalf("bus.Validate refused the envelope a real producer stamped: %v", err)
	}
	if env.GetTenantId() != "acme" {
		t.Fatalf("tenant_id = %q, want acme — it comes from the RECORD, not the deployment, because "+
			"one process serves many tenants", env.GetTenantId())
	}
	if env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_OBSERVATION {
		t.Fatalf("event_class = %v, want OBSERVATION", env.GetEventClass())
	}
	if got := msgs[0].Subject; got != SubjectAgentAnswer {
		t.Fatalf("subject = %q, want %q", got, SubjectAgentAnswer)
	}
	if got := env.GetPartitionKey(); got != "alice@desk" {
		t.Fatalf("partition_key = %q, want the principal — the granularity a reader reconstructs at", got)
	}
	var back observationpb.AgentAnswerRecord
	if err := proto.Unmarshal(payload, &back); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if back.GetAnswerId() != "ans-1" || back.GetModels()[0] != "opus-x/18" {
		t.Fatalf("payload lost fields: %+v", &back)
	}
	// Derived from the payload, not retyped: a consumer routing on the schema ref
	// must decode the type it names.
	if want := string(back.ProtoReflect().Descriptor().FullName()) + ":1"; env.GetPayloadSchemaRef() != want {
		t.Fatalf("PayloadSchemaRef = %q does not describe the payload (%q)", env.GetPayloadSchemaRef(), want)
	}
}

// AN UNTENANTED RECORD IS REFUSED, NEVER PUBLISHED UNDER A FALLBACK. The
// principal is authenticated before Ask runs, so an empty tenant means the record
// was built wrong — and guessing a tenant for an audit record is how a trail
// stops being evidence. Under a fallback it would file one analyst's retrieval
// manifest in another tenant's audit trail.
func TestAnUntenantedAnswerRecordIsRefused(t *testing.T) {
	rec, cc := answerRig(t)
	r := sampleRecord()
	r.TenantId = ""
	err := rec.RecordAnswer(context.Background(), r)
	if err == nil {
		t.Fatal("an untenanted answer record was published")
	}
	// THE MESSAGE IS WHY THIS CHECK EXISTS AT ALL, and asserting it is what makes
	// the check testable.
	//
	// FOUND BY MUTATION: removing the explicit check left this test green, because
	// the producer has no fallback tenant and bus.Validate refuses the envelope
	// anyway. That redundancy is real defence in depth — but it meant nothing
	// distinguished the two layers, so deleting the outer one was invisible.
	//
	// What the outer check adds is the CONSEQUENCE. bus.Validate says "tenant_id
	// required", which is a wire-level complaint; an operator reading it has no
	// idea that the alternative was filing one analyst's retrieval manifest in
	// another tenant's audit trail. The refusal that names the harm is the one
	// somebody can act on.
	if !strings.Contains(err.Error(), "audit trail") {
		t.Fatalf("error = %v — the refusal must name what it prevents (one analyst's retrieval "+
			"manifest reaching another tenant's audit trail), not merely that a field is missing. "+
			"bus.Validate already says the latter", err)
	}
	if len(cc.messages()) != 0 {
		t.Fatalf("%d messages reached the transport despite the refusal", len(cc.messages()))
	}
}

func TestANilAnswerRecordIsRefused(t *testing.T) {
	rec, cc := answerRig(t)
	if err := rec.RecordAnswer(context.Background(), nil); err == nil {
		t.Fatal("a nil record was published")
	}
	if len(cc.messages()) != 0 {
		t.Fatalf("%d messages reached the transport", len(cc.messages()))
	}
}
