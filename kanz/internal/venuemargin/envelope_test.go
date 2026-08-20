package venuemargin

import (
	"context"
	"errors"
	"testing"

	collateralpb "github.com/eighred/kanz/kanz-schemas-go/collateral/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/pkg/bus"
)

// A FAKE PUBLISHER DOES NOT RUN bus.Validate, SO A SUITE BUILT ON ONE IS NOT A
// BROKER PROOF (test/arch/publisher_validation_test.go). Everything else in this
// package publishes through capturePub, which accepts whatever it is handed; a
// missing domain, an unstamped tenant or a COMMAND-shaped idempotency key would
// pass every one of those tests and fail on the first real broker — with the
// symptom being that margin observations silently stop, which reads exactly like
// an unlevered account.
//
// So this one goes through a real bus.Producer over a fake bus.Client and
// asserts the unframed envelope against bus.Validate.

type captureClient struct{ sent []bus.Message }

func (c *captureClient) Publish(_ context.Context, m bus.Message) error {
	c.sent = append(c.sent, m)
	return nil
}
func (c *captureClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *captureClient) Close() error { return nil }

func TestReportedEnvelopePassesBusValidate(t *testing.T) {
	cc := &captureClient{}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source:          "venue-okx/test",
		ProducerVersion: "okx-1.0.0",
		Tenant:          "acme",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	r := NewReporter(ReporterConfig{
		Source: &fakeSource{obs: full()}, Pub: prod,
		Venue: "OKX", Account: "acct-1", Tenant: "acme",
	})
	if err := r.Report(context.Background()); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(cc.sent) != 1 {
		t.Fatalf("messages = %d, want 1", len(cc.sent))
	}
	msg := cc.sent[0]
	if msg.Subject != Subject {
		t.Errorf("subject = %q, want %q", msg.Subject, Subject)
	}
	if string(msg.Key) != "acct-1" {
		t.Errorf("partition key = %q, want the venue account — the unit the exchange liquidates per", msg.Key)
	}

	env, payload, err := bus.Unframe(msg.Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if err := bus.Validate(env); err != nil {
		t.Fatalf("the margin envelope a real broker would receive fails Validate: %v", err)
	}
	if env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Errorf("event class = %v, want FACT", env.GetEventClass())
	}
	if env.GetDomain() != "accounting" {
		t.Errorf("domain = %q, want accounting — the spine this subject lives on", env.GetDomain())
	}
	if env.GetTenantId() != "acme" {
		t.Errorf("tenant = %q, want acme; an untenanted publish is refused by the broker", env.GetTenantId())
	}
	// FACT contract: idempotency_key == event_id.
	if env.GetIdempotencyKey() != env.GetEventId() {
		t.Errorf("idempotency key = %q, want event_id %q", env.GetIdempotencyKey(), env.GetEventId())
	}
	if !env.GetEventTime().AsTime().Equal(observed) {
		t.Errorf("envelope event_time = %s, want the VENUE's observation time %s",
			env.GetEventTime().AsTime(), observed)
	}

	var got collateralpb.VenueMarginState
	if err := proto.Unmarshal(payload, &got); err != nil {
		t.Fatalf("payload does not unmarshal as a VenueMarginState: %v", err)
	}
	if got.GetVenueAccountId() != "acct-1" {
		t.Errorf("payload account = %q, want acct-1", got.GetVenueAccountId())
	}
}
