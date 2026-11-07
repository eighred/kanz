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

func TestRustRecoveryDurableTailRollback(t *testing.T) {
	for _, phase := range []string{"outbox-insert", "deferred-commit"} {
		t.Run(phase, func(t *testing.T) {
			for _, owner := range []string{"go", "rust"} {
				f := newFixture(t)
				st := initial()
				store := order.NewPostgres(f.pool)
				if store.Create(f.ctx, st, nil) != nil {
					t.Fatal("seed canonical order failed")
				}
				v := &venue{tenant: f.tenant, orderID: st.OrderId, description: &venuepb.DescribeResponse{Mic: "MOCK", Account: "fixture-account", AccountVerified: true, ExchangeAccountId: "fixture-exchange-account", SupportedOrderTypes: []orderpb.OrderType{orderpb.OrderType_ORDER_TYPE_LIMIT}}, history: history(st)}
				tr := startTransport(t, v)
				env := &envelopepb.Envelope{EventId: "observation", TenantId: f.tenant, EventType: execution.SubjectStateHealed, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, CorrelationId: "fixture-correlation"}
				payload := wire(t, &orderpb.StateHealed{OrderId: st.OrderId, Venue: st.Venue, State: st})
				// Only the disposable schema is modified. The status record that freezes
				// observed history is allowed; the financial transaction's required FACT
				// fails either at INSERT or at COMMIT, preserving both distinct boundaries.
				sql := `CREATE FUNCTION refuse_recovery_fact() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    IF NEW.event_type='order.order.execution_recovered' THEN RAISE EXCEPTION 'synthetic recovery refusal'; END IF;
    RETURN NEW; END $$;`
				if _, err := f.pool.Exec(f.ctx, sql); err != nil {
					t.Fatal("install disposable refusal function failed")
				}
				trigger := `CREATE TRIGGER refuse_recovery_fact BEFORE INSERT ON outbox FOR EACH ROW EXECUTE FUNCTION refuse_recovery_fact()`
				if phase == "deferred-commit" {
					trigger = `CREATE CONSTRAINT TRIGGER refuse_recovery_fact AFTER INSERT ON outbox DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION refuse_recovery_fact()`
				}
				if _, err := f.pool.Exec(f.ctx, trigger); err != nil {
					t.Fatal("install disposable refusal trigger failed")
				}
				if owner == "go" {
					if f.service(t, tr).HandleRecovery(lineage(f.ctx, env), env, payload) == nil {
						t.Fatal("canonical Go financial transaction did not refuse")
					}
				} else {
					w := f.start(t, tr)
					r := w.call(map[string]any{"method": "Observation", "envelope": wire(t, env), "payload": payload, "at": clock(), "timeout_ms": 30000})
					want := "Database"
					if phase == "deferred-commit" {
						want = "CommitUnconfirmed"
					}
					if r.Status != "error" || r.Error != want || !r.SessionsClosed {
						t.Fatal("Rust transaction failure classification differs")
					}
					w.close(tr)
				}
				got, version, err := store.Load(f.ctx, st.OrderId)
				if err != nil || version != 0 || !proto.Equal(got, st) {
					t.Fatal("failed transaction changed order")
				}
				c, err := store.RecoveryCase(f.ctx, env.EventId)
				if err != nil || c.Status != "investigating" || c.Checkpoint != 0 {
					t.Fatal("failed transaction advanced frozen checkpoint")
				}
				for _, query := range []string{"SELECT count(*) FROM order_fills", "SELECT count(*) FROM order_executions", "SELECT count(*) FROM execution_fee_revisions", "SELECT count(*) FROM execution_recovery_targets", "SELECT count(*) FROM execution_recovery_acks", "SELECT count(*) FROM outbox WHERE event_type='order.order.execution_recovered'"} {
					var n int
					if f.pool.QueryRow(f.ctx, query).Scan(&n) != nil || n != 0 {
						t.Fatal("financial owner survived transaction rollback")
					}
				}
				var frozen int
				if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM execution_recovery_history").Scan(&frozen) != nil || frozen != 1 {
					t.Fatal("separately committed frozen evidence was lost")
				}
				if _, err := f.pool.Exec(f.ctx, "DROP TRIGGER refuse_recovery_fact ON outbox"); err != nil {
					t.Fatal("remove owned fixture trigger failed")
				}
				receive := f.recovered(t, 2)
				if !f.invoke(t, owner, tr, "Observation", env, payload) {
					t.Fatal("restart failed to reclaim rolled-back execution")
				}
				if len(receive()) != 2 || v.queries.Load() != 1 || v.writes.Load() != 0 {
					t.Fatal("rollback retry re-queried, traded or lost facts")
				}
				c, err = store.RecoveryCase(f.ctx, env.EventId)
				if err != nil || c.Checkpoint != 2 {
					t.Fatal("retry did not commit complete history")
				}
			}
		})
	}
}
