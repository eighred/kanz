package recoveryconformance

import (
	"github.com/eighred/kanz/internal/execution"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/services/oms/internal/order"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestRustRecoveryAdmissionRefusals(t *testing.T) {
	for _, name := range []string{"foreign-tenant", "missing-tenant", "command-not-fact", "wrong-subject", "empty-payload", "malformed-payload", "oversized-observation"} {
		t.Run(name, func(t *testing.T) {
			for _, owner := range []string{"go", "rust"} {
				f := newFixture(t)
				st := initial()
				store := order.NewPostgres(f.pool)
				if store.Create(f.ctx, st, nil) != nil {
					t.Fatal("seed canonical order failed")
				}
				v := &venue{tenant: f.tenant, orderID: st.OrderId, description: &venuepb.DescribeResponse{Mic: "MOCK", Account: "fixture-account", AccountVerified: true, ExchangeAccountId: "fixture-exchange-account", SupportedOrderTypes: []orderpb.OrderType{orderpb.OrderType_ORDER_TYPE_LIMIT}}, history: history(st)}
				tr := startTransport(t, v)
				env := &envelopepb.Envelope{EventId: "refused-observation", TenantId: f.tenant, EventType: execution.SubjectStateHealed, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT}
				payload := wire(t, &orderpb.StateHealed{OrderId: st.OrderId, Venue: st.Venue, State: st})
				switch name {
				case "foreign-tenant":
					env.TenantId = "other-tenant"
				case "missing-tenant":
					env.TenantId = ""
				case "command-not-fact":
					env.EventClass = envelopepb.EventClass_EVENT_CLASS_COMMAND
				case "wrong-subject":
					env.EventType = "order.order.filled"
				case "empty-payload":
					payload = []byte{}
				case "malformed-payload":
					payload = []byte{255}
				case "oversized-observation":
					payload = make([]byte, (4<<20)+1)
				}
				if f.invoke(t, owner, tr, "Observation", env, payload) {
					t.Fatal("invalid recovery trigger accepted")
				}
				for _, query := range []string{"SELECT count(*) FROM execution_recovery_cases", "SELECT count(*) FROM execution_account_mappings", "SELECT count(*) FROM order_fills", "SELECT count(*) FROM outbox"} {
					var n int
					if f.pool.QueryRow(f.ctx, query).Scan(&n) != nil || n != 0 {
						t.Fatal("refused trigger wrote durable evidence")
					}
				}
				got, version, err := store.Load(f.ctx, st.OrderId)
				if err != nil || version != 0 || !proto.Equal(got, st) {
					t.Fatal("refused trigger changed order")
				}
				if v.queries.Load() != 0 || v.writes.Load() != 0 {
					t.Fatal("refused trigger contacted execution history or traded")
				}
			}
		})
	}
}
