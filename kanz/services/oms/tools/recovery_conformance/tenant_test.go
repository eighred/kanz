package recoveryconformance

import (
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/fillfact"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/order"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestRustRecoveryTenantIsolation(t *testing.T) {
	for _, owner := range []string{"go", "rust"} {
		t.Run(owner, func(t *testing.T) {
			a := newFixture(t)
			b := &fixture{t: t, ctx: a.ctx, schema: a.schema, tenant: "recovery-tenant-b", postgresURL: a.postgresURL}
			b.pool = b.open(b.tenant)
			var first *orderpb.ExecutionRecovered
			var firstState *orderpb.OrderState
			var firstVersion int64
			for index, f := range []*fixture{a, b} {
				st := initial()
				store := order.NewPostgres(f.pool)
				if store.Create(f.ctx, st, nil) != nil {
					t.Fatal("same-ID tenant order creation failed")
				}
				view := history(st)
				if index == 1 {
					for _, fill := range view.Fills {
						fill.Price.Coefficient += 100
					}
				}
				v := &venue{tenant: f.tenant, orderID: st.OrderId, description: &venuepb.DescribeResponse{Mic: "MOCK", Account: "fixture-account", AccountVerified: true, ExchangeAccountId: "fixture-exchange-account", SupportedOrderTypes: []orderpb.OrderType{orderpb.OrderType_ORDER_TYPE_LIMIT}}, history: view}
				tr := startTransport(t, v)
				env := &envelopepb.Envelope{EventId: "observation", TenantId: f.tenant, EventType: execution.SubjectStateHealed, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, CorrelationId: "fixture-correlation"}
				payload := wire(t, &orderpb.StateHealed{OrderId: st.OrderId, Venue: st.Venue, State: st})
				if index == 1 {
					foreign := proto.Clone(env).(*envelopepb.Envelope)
					foreign.TenantId = a.tenant
					if f.invoke(t, owner, tr, "Observation", foreign, payload) {
						t.Fatal("foreign recovery envelope accepted")
					}
					var n int
					if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM execution_recovery_cases").Scan(&n) != nil || n != 0 {
						t.Fatal("foreign envelope created tenant B recovery state")
					}
				}
				receive := f.recovered(t, 2)
				if !f.invoke(t, owner, tr, "Observation", env, payload) {
					t.Fatal("tenant positive recovery failed")
				}
				records := receive()
				if index == 0 {
					first = records[0]
					var err error
					firstState, firstVersion, err = store.Load(f.ctx, st.OrderId)
					if err != nil {
						t.Fatal(err)
					}
				} else {
					// This qualifies target binding, not authentication by a relabelled
					// envelope: the caller still owns authenticated tenant attribution.
					ack, err := fillfact.RecoveryAck(bus.WithTenantID(f.ctx, a.tenant), fillfact.RecoveryPositionApplied, first.Fill, instant)
					if err != nil {
						t.Fatal(err)
					}
					ackEnv := &envelopepb.Envelope{EventId: "crossed-ack", TenantId: f.tenant, EventType: fillfact.RecoveryPositionApplied, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT}
					if f.invoke(t, owner, tr, "Ack", ackEnv, ack.Payload) {
						t.Fatal("crossed tenant-local execution digest accepted")
					}
					var n int
					if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM execution_recovery_acks").Scan(&n) != nil || n != 0 {
						t.Fatal("crossed ACK changed tenant B acknowledgement state")
					}
				}
				f.acknowledge(t, owner, tr, records)
				var rows, foreign int
				if f.pool.QueryRow(f.ctx, "SELECT count(*),count(*) FILTER(WHERE tenant_id<>$1 OR envelope_tenant_id<>$1) FROM outbox", f.tenant).Scan(&rows, &foreign) != nil || rows == 0 || foreign != 0 {
					t.Fatal("tenant-scoped outbox attribution differs")
				}
			}
			final, version, err := order.NewPostgres(a.pool).Load(a.ctx, initial().OrderId)
			if err != nil || version != firstVersion || !proto.Equal(final, firstState) {
				t.Fatal("tenant B same-ID writes altered tenant A order")
			}
			for _, f := range []*fixture{a, b} {
				c, err := order.NewPostgres(f.pool).RecoveryCase(f.ctx, "observation")
				if err != nil || c.Status != "corrected" || c.Checkpoint != 2 {
					t.Fatal("independent same-ID tenant case missing")
				}
			}
		})
	}
}
