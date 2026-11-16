package recoveryconformance

import (
	"github.com/eighred/kanz/internal/execution"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/services/oms/internal/order"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestRustRecoveryExplicitZeroHistory(t *testing.T) {
	for _, owner := range []string{"go", "rust"} {
		t.Run(owner, func(t *testing.T) {
			f := newFixture(t)
			st := initial()
			store := order.NewPostgres(f.pool)
			if store.Create(f.ctx, st, nil) != nil {
				t.Fatal("seed order failed")
			}
			view := &venuepb.QueryOrderResponse{State: venuepb.OrderViewState_ORDER_VIEW_STATE_CANCELLED, ExecutedQuantity: &commonpb.Decimal{}}
			v := &venue{tenant: f.tenant, orderID: st.OrderId, description: &venuepb.DescribeResponse{Mic: "MOCK", Account: "fixture-account", AccountVerified: true, ExchangeAccountId: "fixture-exchange-account", SupportedOrderTypes: []orderpb.OrderType{orderpb.OrderType_ORDER_TYPE_LIMIT}}, history: view}
			tr := startTransport(t, v)
			env := &envelopepb.Envelope{EventId: "observation", TenantId: f.tenant, EventType: execution.SubjectStateHealed, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, CorrelationId: "fixture-correlation"}
			payload := wire(t, &orderpb.StateHealed{OrderId: st.OrderId, Venue: st.Venue, State: st})
			if !f.invoke(t, owner, tr, "Observation", env, payload) {
				t.Fatal("explicit zero history refused")
			}
			got, version, err := store.Load(f.ctx, st.OrderId)
			if err != nil || version != 1 || !proto.Equal(got, st) {
				t.Fatal("zero history altered financial state or missed CAS")
			}
			c, err := store.RecoveryCase(f.ctx, env.EventId)
			if err != nil || c.Status != "corrected" || c.Checkpoint != 0 {
				t.Fatal("zero history not corrected without ACKs")
			}
			for _, table := range []string{"order_fills", "order_executions", "execution_recovery_targets", "execution_recovery_acks", "execution_fee_revisions"} {
				var n int
				if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM "+table).Scan(&n) != nil || n != 0 {
					t.Fatal("zero history fabricated execution")
				}
			}
			var wrong int
			if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM outbox WHERE event_type<>'order.order.recovery_recorded'").Scan(&wrong) != nil || wrong != 0 {
				t.Fatal("zero history emitted non-status FACT")
			}
			frozen, _, err := store.RecoveryHistory(f.ctx, env.EventId)
			if err != nil || frozen.ExecutedQuantity == nil || frozen.ExecutedQuantity.Coefficient != 0 || len(frozen.Fills) != 0 {
				t.Fatal("explicit zero lost presence")
			}
			if !f.invoke(t, owner, tr, "Observation", env, payload) || v.queries.Load() != 1 || v.writes.Load() != 0 {
				t.Fatal("zero replay re-queried or traded")
			}
		})
	}
}
