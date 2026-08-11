package publish

import (
	"context"
	"testing"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	optimizationpb "github.com/eighred/kanz/kanz-schemas-go/optimization/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/pkg/bus"
)

type fakeBus struct{ events []bus.Event }

func (f *fakeBus) Publish(_ context.Context, e bus.Event) error {
	f.events = append(f.events, e)
	return nil
}

type count struct{ n int }

func (c *count) Inc() { c.n++ }

// A DISARMED MATERIALIZER CANNOT PUBLISH, even if something calls Publish
// directly. It is handed to the server for its recording half only; this is the
// last line of defence if that ever changes, and it REFUSES rather than
// silently succeeding — a no-op here is a rebalance the caller believes went out.
func TestADisarmedMaterializerRefusesToPublish(t *testing.T) {
	m := NewRecorder(&fakeBus{}, &count{}).ForTenant("acme")

	if err := m.Publish(context.Background(), &orderpb.SubmitOrder{OrderId: "o1"}); err == nil {
		t.Fatal("a disarmed materializer published a command. The default posture is that the " +
			"optimizer proposes and a human approves; this is that reversed silently.")
	}
}

// A MATERIALIZER WITH NO TENANT PUBLISHES NOTHING. An unscoped one would emit a
// command the broker cannot attribute — and the failure this prevents is worse
// than rejection: a capital command booked against the wrong tenant is wrong
// forever and nothing downstream can tell.
func TestAnUnscopedMaterializerRefusesToPublish(t *testing.T) {
	m := NewMaterializer(&fakeBus{}, &count{}, &count{}) // never ForTenant'd

	if err := m.Publish(context.Background(), &orderpb.SubmitOrder{OrderId: "o1"}); err == nil {
		t.Fatal("published a command with no tenant")
	}
}

// ForTenant COPIES. One materializer is shared by every request; stamping the
// tenant onto the shared value would let two concurrent callers from different
// tenants publish under each other's.
func TestForTenantDoesNotMutateTheShared(t *testing.T) {
	shared := NewMaterializer(&fakeBus{}, &count{}, &count{})
	a := shared.ForTenant("acme")
	b := shared.ForTenant("globex")

	if a.tenant != "acme" || b.tenant != "globex" {
		t.Fatalf("tenants = %q / %q, want acme / globex", a.tenant, b.tenant)
	}
	if shared.tenant != "" {
		t.Errorf("the SHARED materializer acquired tenant %q — two callers now race over one field, "+
			"and the loser's rebalance publishes under the winner's tenant", shared.tenant)
	}
}

// The command envelope must match the api-gateway's order surface: a COMMAND,
// tenant-routed subject, and an idempotency key so a re-materialization dedups
// at the OMS rather than double-trading.
func TestTheCommandEnvelopeIsACommandAndIsIdempotent(t *testing.T) {
	fb := &fakeBus{}
	m := NewMaterializer(fb, &count{}, &count{}).ForTenant("acme")

	if err := m.Publish(context.Background(), &orderpb.SubmitOrder{OrderId: "PF:BTC-USDT:rebal"}); err != nil {
		t.Fatal(err)
	}
	if len(fb.events) != 1 {
		t.Fatalf("events = %d, want 1", len(fb.events))
	}
	e := fb.events[0]
	if e.EventClass != envelopepb.EventClass_EVENT_CLASS_COMMAND {
		t.Errorf("event class = %v, want COMMAND — a FACT here would bypass the OMS's command path", e.EventClass)
	}
	if e.EventType != "order.order.submit" {
		t.Errorf("event type = %q, want order.order.submit (the OMS's own subject: one door into admission)", e.EventType)
	}
	if e.TenantID != "acme" {
		t.Errorf("tenant = %q, want acme", e.TenantID)
	}
	if e.IdempotencyKey != "PF:BTC-USDT:rebal" {
		t.Errorf("idempotency key = %q, want the order id — without it a re-materialized proposal "+
			"double-trades instead of deduping", e.IdempotencyKey)
	}
}

// The FACT is a FACT, not a command: it records what happened and instructs
// nobody.
func TestTheMaterializedRecordIsAFact(t *testing.T) {
	fb := &fakeBus{}
	m := NewMaterializer(fb, &count{}, &count{}).ForTenant("acme")

	err := m.Record(context.Background(), &optimizationpb.ProposalMaterialized{PortfolioId: "PF", Issuer: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	e := fb.events[0]
	if e.EventClass != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Errorf("event class = %v, want FACT", e.EventClass)
	}
	if e.PartitionKey != "PF" {
		t.Errorf("partition key = %q, want the portfolio id", e.PartitionKey)
	}
}

// Published commands are counted, so "this deployment trades on its own
// recommendation" is a number on a dashboard rather than a property of a config
// file nobody re-reads.
func TestPublishedCommandsAreCounted(t *testing.T) {
	published := &count{}
	m := NewMaterializer(&fakeBus{}, published, &count{}).ForTenant("acme")

	for i := 0; i < 3; i++ {
		if err := m.Publish(context.Background(), &orderpb.SubmitOrder{OrderId: "o"}); err != nil {
			t.Fatal(err)
		}
	}
	if published.n != 3 {
		t.Errorf("counter = %d, want 3", published.n)
	}
}
