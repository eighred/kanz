package authbus_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/authbus"
	"github.com/eighred/kanz/pkg/bus"
)

var recClock = time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)

// captureClient records every Publish (mirrors the integrity/RISK-10 test seam).
type captureClient struct {
	mu   sync.Mutex
	sent []bus.Message
}

func (c *captureClient) Publish(_ context.Context, m bus.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, m)
	return nil
}
func (c *captureClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *captureClient) Close() error { return nil }

func (c *captureClient) messages() []bus.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]bus.Message(nil), c.sent...)
}

func newRecorder(t *testing.T, opts ...authbus.Option) (*authbus.BusRecorder, *captureClient) {
	t.Helper()
	cc := &captureClient{}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source:          "copilot/test",
		ProducerVersion: "copilot-1.0.0",
		Tenant:          "acme",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	opts = append([]authbus.Option{authbus.WithClock(func() time.Time { return recClock })}, opts...)
	rec, err := authbus.NewBusRecorder(prod, opts...)
	if err != nil {
		t.Fatalf("NewBusRecorder: %v", err)
	}
	return rec, cc
}

func decisionLog(allow bool) *observationpb.DecisionLog {
	req := auth.Request{
		Principal: &auth.Principal{Subject: "u1", Tenant: "acme"},
		Action:    auth.ActionRiskRead,
		Resource:  auth.Resource{Type: "portfolio", ID: "p1", Tenant: "acme"},
	}
	d := auth.Decision{Allow: allow, Reason: "policy grant"}
	return auth.BuildDecisionLog(auth.DefaultAuthzDecider, req, d)
}

func TestBusRecorderPublishesObservationOnAuthzSubject(t *testing.T) {
	rec, cc := newRecorder(t)
	if err := rec.Record(context.Background(), decisionLog(true)); err != nil {
		t.Fatalf("Record: %v", err)
	}
	rec.Close() // drains the async worker

	msgs := cc.messages()
	if len(msgs) != 1 {
		t.Fatalf("captured %d messages want 1", len(msgs))
	}
	var frame envelopepb.EventFrame
	if err := proto.Unmarshal(msgs[0].Body, &frame); err != nil {
		t.Fatalf("frame unmarshal: %v", err)
	}
	if err := bus.Validate(frame.Envelope); err != nil {
		t.Fatalf("envelope invalid: %v", err)
	}
	env := frame.Envelope
	if env.EventType != auth.AuthzDecisionEventType {
		t.Errorf("event_type = %q want %q", env.EventType, auth.AuthzDecisionEventType)
	}
	if env.Domain != auth.AuthzDecisionDomain {
		t.Errorf("domain = %q want %q", env.Domain, auth.AuthzDecisionDomain)
	}
	if env.EventClass != envelopepb.EventClass_EVENT_CLASS_OBSERVATION {
		t.Errorf("class = %v want OBSERVATION", env.EventClass)
	}
	// OBSERVATION idempotency_key defaults to event_id (bus stamps it); partition
	// by the deciding principal so a subject's audit trail stays ordered.
	if env.PartitionKey != "u1" {
		t.Errorf("partition_key = %q want u1 (principal subject)", env.PartitionKey)
	}

	var entry observationpb.DecisionLog
	if err := proto.Unmarshal(frame.Payload, &entry); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if entry.GetAttributes()["decision"] != "allow" {
		t.Errorf("decision attr = %q want allow", entry.GetAttributes()["decision"])
	}
	if entry.GetDecider() != auth.DefaultAuthzDecider {
		t.Errorf("decider = %q want %q", entry.GetDecider(), auth.DefaultAuthzDecider)
	}
}

// A deny is published just like an allow — the audit trail is symmetric.
func TestBusRecorderPublishesDeny(t *testing.T) {
	rec, cc := newRecorder(t)
	_ = rec.Record(context.Background(), decisionLog(false))
	rec.Close()

	msgs := cc.messages()
	if len(msgs) != 1 {
		t.Fatalf("captured %d want 1", len(msgs))
	}
	var frame envelopepb.EventFrame
	_ = proto.Unmarshal(msgs[0].Body, &frame)
	var entry observationpb.DecisionLog
	_ = proto.Unmarshal(frame.Payload, &entry)
	if entry.GetAttributes()["decision"] != "deny" {
		t.Errorf("decision attr = %q want deny", entry.GetAttributes()["decision"])
	}
}

// Record is non-blocking: a full buffer drops rather than stalls, and the drop
// is counted (never a silent loss).
func TestBusRecorderOverflowDropsNotBlocks(t *testing.T) {
	// Queue size 1, and hold the worker by blocking the client so nothing drains.
	release := make(chan struct{})
	cc := &blockingClient{release: release}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{Source: "copilot/test", ProducerVersion: "v1", Tenant: "acme"})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	var dropped int
	rec, err := authbus.NewBusRecorder(prod,
		authbus.WithClock(func() time.Time { return recClock }),
		authbus.WithQueueSize(1),
		authbus.WithOverflowHandler(func(*observationpb.DecisionLog) { dropped++ }))
	if err != nil {
		t.Fatalf("NewBusRecorder: %v", err)
	}

	// First record is taken by the worker (which then blocks in Publish); the
	// next fills the size-1 buffer; every further record overflows and drops.
	for i := 0; i < 12; i++ {
		if err := rec.Record(context.Background(), decisionLog(true)); err != nil {
			t.Fatalf("Record must never error: %v", err)
		}
	}
	if rec.Dropped() == 0 {
		t.Error("expected overflow drops with the worker blocked")
	}
	if dropped == 0 {
		t.Error("overflow handler not invoked")
	}
	close(release) // let the worker drain so Close returns
	rec.Close()
}

func TestNewBusRecorderNilProducer(t *testing.T) {
	if _, err := authbus.NewBusRecorder(nil); err == nil {
		t.Fatal("nil producer should error (use SlogRecorder without a bus)")
	}
}

// blockingClient blocks in Publish until release is closed, pinning the worker.
type blockingClient struct {
	release chan struct{}
	mu      sync.Mutex
	sent    int
}

func (c *blockingClient) Publish(_ context.Context, _ bus.Message) error {
	<-c.release
	c.mu.Lock()
	c.sent++
	c.mu.Unlock()
	return nil
}
func (c *blockingClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *blockingClient) Close() error { return nil }

// THE DECISION CARRIES ITS OWN TENANT, NOT THE DEPLOYMENT'S.
//
// The `newRecorder` producer is configured with Tenant "acme" as a fallback, so a
// test that only ever decided for acme could not tell the two apart. This one
// decides for a DIFFERENT tenant: if the envelope comes back "acme", the
// producer's fallback won and every customer's access decisions would be
// attributed to whatever the deployment was configured with — a value that is
// valid, not theirs, and undetectable downstream.
func TestBusRecorderStampsTheDecidingPrincipalsTenant(t *testing.T) {
	rec, cc := newRecorder(t)
	req := auth.Request{
		Principal: &auth.Principal{Subject: "u2", Tenant: "globex"},
		Action:    auth.ActionRiskRead,
		Resource:  auth.Resource{Type: "portfolio", ID: "p9", Tenant: "globex"},
	}
	entry := auth.BuildDecisionLog(auth.DefaultAuthzDecider, req, auth.Decision{Allow: true, Reason: "grant"})
	_ = rec.Record(context.Background(), entry)
	rec.Close()

	msgs := cc.messages()
	if len(msgs) != 1 {
		t.Fatalf("captured %d want 1", len(msgs))
	}
	var frame envelopepb.EventFrame
	if err := proto.Unmarshal(msgs[0].Body, &frame); err != nil {
		t.Fatalf("frame unmarshal: %v", err)
	}
	if got := frame.Envelope.GetTenantId(); got != "globex" {
		t.Fatalf("envelope tenant_id = %q, want globex.\n\n"+
			"The producer's fallback (acme) won over the deciding principal's tenant. In a "+
			"multi-tenant deployment that attributes every customer's access decisions to the "+
			"platform's own tenant, and nothing downstream can tell they are misfiled.", got)
	}
}

// A DECISION WITH NO PRINCIPAL TENANT USES THE FALLBACK — and without one, fails
// LOUDLY rather than being stamped with a tenant nobody chose.
func TestBusRecorderFallbackTenantAppliesOnlyWhenThereIsNone(t *testing.T) {
	anonymous := auth.BuildDecisionLog("gateway",
		auth.Request{Action: "read", Resource: auth.Resource{Type: "route", ID: "/v1/x"}},
		auth.Decision{Allow: false, Reason: "no principal"})
	if _, ok := anonymous.GetAttributes()["principal.tenant"]; ok {
		t.Fatal("fixture carries a principal.tenant — this test would not exercise the fallback")
	}

	t.Run("fallback configured", func(t *testing.T) {
		cc := &captureClient{}
		// No producer-level Tenant: this is the api-gateway shape, where a producer
		// fallback would also apply to order COMMANDs on the same producer.
		prod, err := bus.NewProducer(cc, bus.ProducerConfig{Source: "gw/test", ProducerVersion: "v1"})
		if err != nil {
			t.Fatalf("NewProducer: %v", err)
		}
		rec, err := authbus.NewBusRecorder(prod, authbus.WithFallbackTenant("__system__"))
		if err != nil {
			t.Fatalf("NewBusRecorder: %v", err)
		}
		_ = rec.Record(context.Background(), anonymous)
		rec.Close()

		msgs := cc.messages()
		if len(msgs) != 1 {
			t.Fatalf("captured %d want 1 — the publish failed, so the fallback did not apply", len(msgs))
		}
		var frame envelopepb.EventFrame
		_ = proto.Unmarshal(msgs[0].Body, &frame)
		if got := frame.Envelope.GetTenantId(); got != "__system__" {
			t.Errorf("envelope tenant_id = %q, want __system__", got)
		}
	})

	t.Run("no fallback fails loudly", func(t *testing.T) {
		cc := &captureClient{}
		prod, err := bus.NewProducer(cc, bus.ProducerConfig{Source: "gw/test", ProducerVersion: "v1"})
		if err != nil {
			t.Fatalf("NewProducer: %v", err)
		}
		var publishErr error
		rec, err := authbus.NewBusRecorder(prod,
			authbus.WithErrorHandler(func(e error) { publishErr = e }))
		if err != nil {
			t.Fatalf("NewBusRecorder: %v", err)
		}
		_ = rec.Record(context.Background(), anonymous)
		rec.Close()

		if publishErr == nil {
			t.Fatal("a decision with no principal tenant and no fallback PUBLISHED.\n\n" +
				"It must be rejected by bus.Validate so the error handler counts it and the " +
				"deployment learns it never configured one. Silently stamping some default " +
				"would file those decisions under a tenant nobody chose.")
		}
		if len(cc.messages()) != 0 {
			t.Errorf("an invalid envelope reached the client: %d messages", len(cc.messages()))
		}
	})
}
