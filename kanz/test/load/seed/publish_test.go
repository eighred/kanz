// THE SEEDER'S ENVELOPE, DRIVEN THROUGH A REAL PRODUCER (#245).
//
// The last entry on #245's list. Same reasoning as test/load/ingest: a seeder
// whose envelopes the broker refuses does not report a failed seed — it reports
// a seeded environment that is empty, and every measurement taken against it is
// of nothing.
//
// THIS ONE IS THE ONLY STATE_SNAPSHOT PUBLISHER IN THE MODULE. Every other
// publisher covered by #245 emits FACTs, COMMANDs or OBSERVATIONs, so the
// snapshot arm of bus.Validate was exercised nowhere at all until this file.
// That makes the lowest-ranked entry on the list the only cover for one whole
// event class — worth noting, because "lowest value" was about the tool, not
// about what testing it happens to reach.
//
// Tier-B: a real bus.Producer over a fake bus.Client.
package main

import (
	"context"
	"errors"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

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

var seedAt = time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

func realSeedProducer(t *testing.T, tenant string) (*bus.Producer, *captureClient) {
	t.Helper()
	cc := &captureClient{}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source:          "load-seed/seed",
		ProducerVersion: "dev",
		Tenant:          tenant,
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	return prod, cc
}

// THE SNAPSHOT ENVELOPE IS LEGAL — and this is the only place STATE_SNAPSHOT
// reaches bus.Validate anywhere in the module.
func TestSeededSnapshotIsAValidEnvelope(t *testing.T) {
	prod, cc := realSeedProducer(t, "acme")

	if err := prod.Publish(context.Background(), snapshotEvent("pf-1", seedAt)); err != nil {
		t.Fatalf("Publish: %v — the seeder's envelope does not survive bus.Validate, so a 'seeded' "+
			"environment would be empty and every measurement against it meaningless", err)
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
	if got := env.GetEventClass(); got != envelopepb.EventClass_EVENT_CLASS_STATE_SNAPSHOT {
		t.Errorf("event_class = %v, want STATE_SNAPSHOT — a snapshot folded as a FACT would be "+
			"applied on top of the book rather than replacing it", got)
	}
	if got := env.GetTenantId(); got != "acme" {
		t.Errorf("tenant_id = %q, want acme — snapshotEvent sets none, so it comes from ProducerConfig", got)
	}
	if got := env.GetPayloadSchemaRef(); got != payloadSchemaRef {
		t.Errorf("payload_schema_ref = %q, want %q", got, payloadSchemaRef)
	}
	if got := string(cc.sent[0].Key); got != "pf-1" {
		t.Errorf("partition key = %q, want the portfolio", got)
	}
	if got := env.GetEventTime().AsTime(); !got.Equal(seedAt) {
		t.Errorf("event_time = %s, want %s", got, seedAt)
	}
}

// THE SEEDED BOOK IS NON-DEGENERATE. buildSnapshot's comment says it exists so
// Exposure/Measures return real numbers rather than a zero set — an empty
// snapshot is a perfectly legal envelope, so Validate cannot catch this and a
// run against it would measure nothing while looking healthy.
func TestSeededSnapshotCarriesPositions(t *testing.T) {
	prod, cc := realSeedProducer(t, "acme")
	if err := prod.Publish(context.Background(), snapshotEvent("pf-1", seedAt)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	snap := buildSnapshot("pf-1", seedAt)
	if len(snap.GetPositions()) == 0 {
		t.Fatal("the seeded snapshot carries no positions. It is still a LEGAL envelope, so nothing " +
			"would refuse it — the load run would simply measure an empty book and report success")
	}
	if len(cc.sent) != 1 {
		t.Fatalf("published %d messages, want 1", len(cc.sent))
	}
}

// WITH NO TENANT ANYWHERE the publish is refused, rather than seeding nothing
// and reporting success.
func TestASeededSnapshotWithNoTenantIsRefused(t *testing.T) {
	prod, cc := realSeedProducer(t, "")

	if err := prod.Publish(context.Background(), snapshotEvent("pf-1", seedAt)); err == nil {
		t.Fatal("a tenant-less snapshot was published. On the live spine bus.Validate refuses it, so " +
			"the seeder would exit 0 having seeded nothing")
	}
	if len(cc.sent) != 0 {
		t.Errorf("a refused envelope still reached the transport (%d message(s))", len(cc.sent))
	}
}
