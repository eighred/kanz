// THE LOAD GENERATOR'S ENVELOPE, DRIVEN THROUGH A REAL PRODUCER (#245).
//
// This is the last category on #245's list and the one it ranks lowest, for a
// stated reason worth repeating: "a load tool that cannot publish measures
// nothing". A generator whose envelopes the broker refuses does not report a
// failed load test — it reports a load test that found no problems, because no
// load ever arrived. That is a worse outcome than a red run.
//
// It was LISTED rather than excluded by path, deliberately, so that anything
// which moves into test/ does not silently escape the guard. Clearing it the
// same way as everything else keeps that true.
//
// Tier-B: a real bus.Producer over a fake bus.Client.
package main

import (
	"context"
	"errors"
	"math/rand"
	"testing"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/internal/platform/subject"
	"github.com/eighred/kanz/pkg/bus"
)

// captureClient records the framed wire bytes — the Tier-B helper from
// internal/risk/publish and services/accounting/internal/cashmove.
type captureClient struct{ sent []bus.Message }

func (c *captureClient) Publish(_ context.Context, m bus.Message) error {
	c.sent = append(c.sent, m)
	return nil
}
func (c *captureClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *captureClient) Close() error { return nil }

func realLoadProducer(t *testing.T, tenant string) (*bus.Producer, *captureClient) {
	t.Helper()
	cc := &captureClient{}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source:          "load-ingest/ingest",
		ProducerVersion: "dev",
		Tenant:          tenant,
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	return prod, cc
}

// THE GENERATED EVENT IS A LEGAL ENVELOPE. If it is not, a load run publishes
// nothing and reports success.
func TestGeneratedTickIsAValidEnvelope(t *testing.T) {
	prod, cc := realLoadProducer(t, "acme")
	rng := rand.New(rand.NewSource(1))

	if err := prod.Publish(context.Background(), tick("acme", "pf-1", "BTC-USD", rng)); err != nil {
		t.Fatalf("Publish: %v — the load generator's envelope does not survive bus.Validate, so a run "+
			"would produce no load at all and report no problems", err)
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
	if got := env.GetEventClass(); got != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Errorf("event_class = %v, want FACT", got)
	}
	if got := env.GetTenantId(); got != "acme" {
		t.Errorf("tenant_id = %q, want acme — tick() sets none, so it comes from ProducerConfig", got)
	}
	if got := env.GetPayloadSchemaRef(); got != positionSchemaRef {
		t.Errorf("payload_schema_ref = %q, want %q", got, positionSchemaRef)
	}
	if got := string(cc.sent[0].Key); got != "pf-1" {
		t.Errorf("partition key = %q, want the portfolio — per-aggregate ordering (RISK-05) and the "+
			"shard fan-out (PARITY-05a) both key on it", got)
	}
}

// THE SUBJECT IS PER-HOLDING (EXEC-M20), AND THAT IS NOT COSMETIC. The flat
// subject is no longer carried by any stream, and a JetStream publish to an
// unbound subject is a HARD ERROR — so a generator still using it would not
// degrade, it would fail outright. tick()'s own comment says so; this asserts it.
func TestGeneratedTickUsesThePerHoldingSubject(t *testing.T) {
	prod, cc := realLoadProducer(t, "acme")
	rng := rand.New(rand.NewSource(1))

	if err := prod.Publish(context.Background(), tick("acme", "pf-1", "BTC-USD", rng)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	want := subject.PositionFor("acme", "pf-1", "BTC-USD")
	if got := cc.sent[0].Subject; got != want {
		t.Errorf("subject = %q, want %q — a flat subject is bound to no stream, so the publish is a "+
			"hard error rather than a degraded one", got, want)
	}
}

// WITH NO TENANT ANYWHERE the publish is refused, rather than producing a load
// run that silently generates nothing.
func TestATickWithNoTenantIsRefused(t *testing.T) {
	prod, cc := realLoadProducer(t, "")
	rng := rand.New(rand.NewSource(1))

	if err := prod.Publish(context.Background(), tick("", "pf-1", "BTC-USD", rng)); err == nil {
		t.Fatal("a tenant-less tick was published. On the live spine bus.Validate refuses it, so the " +
			"load run would generate no load and report no failures")
	}
	if len(cc.sent) != 0 {
		t.Errorf("a refused envelope still reached the transport (%d message(s))", len(cc.sent))
	}
}
