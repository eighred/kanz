package recoveryconformance

import (
	"github.com/eighred/kanz/internal/execution"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/services/oms/internal/order"
	"google.golang.org/protobuf/proto"
	"strings"
	"testing"
)

func TestRustRecoveryMalformedPostedJournal(t *testing.T) {
	for _, owner := range []string{"go", "rust"} {
		t.Run(owner, func(t *testing.T) {
			f := newFixture(t)
			st := initial()
			store := order.NewPostgres(f.pool)
			if store.Create(f.ctx, st, nil) != nil {
				t.Fatal("seed order failed")
			}
			bad := proto.Clone(history(st).Fills[0]).(*orderpb.Fill)
			bad.FillId = strings.Repeat("x", 65536)
			bad.Venue = ""
			bad.VenueExecutionId = ""
			raw := wire(t, bad)
			// A deliberately corrupt legacy body under a short indexed row identity:
			// no oversized B-tree tuple or disabled canonical constraint is involved.
			if _, err := f.pool.Exec(f.ctx, "INSERT INTO order_fills(order_id,fill_id) VALUES($1,'legacy-row')", st.OrderId); err != nil {
				t.Fatal("seed legacy claim failed")
			}
			if _, err := f.pool.Exec(f.ctx, "INSERT INTO order_executions(order_id,fill_id,venue,venue_account_id,instrument_id,venue_execution_id,fill) VALUES($1,'legacy-row','',$2,$3,'',$4)", st.OrderId, st.VenueAccountId, st.InstrumentId, raw); err != nil {
				t.Fatal("seed malformed journal failed")
			}
			v := &venue{tenant: f.tenant, orderID: st.OrderId, description: &venuepb.DescribeResponse{Mic: "MOCK", Account: "fixture-account", AccountVerified: true, ExchangeAccountId: "fixture-exchange-account", SupportedOrderTypes: []orderpb.OrderType{orderpb.OrderType_ORDER_TYPE_LIMIT}}, history: history(st)}
			tr := startTransport(t, v)
			env := &envelopepb.Envelope{EventId: "observation", TenantId: f.tenant, EventType: execution.SubjectStateHealed, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, CorrelationId: "fixture-correlation"}
			payload := wire(t, &orderpb.StateHealed{OrderId: st.OrderId, Venue: st.Venue, State: st})
			if !f.invoke(t, owner, tr, "Observation", env, payload) {
				t.Fatal("posted-history refusal did not acknowledge durable blocked state")
			}
			c, err := store.RecoveryCase(f.ctx, env.EventId)
			if err != nil || c.Status != "blocked" || c.Checkpoint != 0 {
				t.Fatal("malformed posted identity was not blocked")
			}
			got, version, err := store.Load(f.ctx, st.OrderId)
			if err != nil || version != 0 || !proto.Equal(st, got) {
				t.Fatal("malformed posted identity altered order")
			}
			for _, table := range []string{"order_fills", "order_executions"} {
				var n int
				if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM "+table).Scan(&n) != nil || n != 1 {
					t.Fatal("posted refusal changed legacy inventory")
				}
			}
			for _, table := range []string{"execution_recovery_targets", "execution_recovery_acks", "execution_fee_revisions"} {
				var n int
				if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM "+table).Scan(&n) != nil || n != 0 {
					t.Fatal("posted refusal wrote financial evidence")
				}
			}
			var persisted []byte
			if f.pool.QueryRow(f.ctx, "SELECT fill FROM order_executions WHERE fill_id='legacy-row'").Scan(&persisted) != nil || string(persisted) != string(raw) {
				t.Fatal("posted refusal rewrote historical bytes")
			}
		})
	}
}
