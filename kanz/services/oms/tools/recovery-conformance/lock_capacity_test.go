package recoveryconformance

import (
	"errors"
	"github.com/eighred/kanz/internal/execution"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/services/oms/internal/order"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"os"
	"testing"
	"time"
)

func TestRustRecoveryDefaultLockCapacityRefusal(t *testing.T) {
	binary(t)
	defaultURL := os.Getenv("TEST_POSTGRES_DEFAULT_LOCK_URL")
	if defaultURL == "" {
		t.Skip("separate default-lock-capacity PostgreSQL unavailable")
	}
	primary, err := pgxpool.ParseConfig(os.Getenv("TEST_POSTGRES_URL"))
	if err != nil {
		t.Fatal("primary PostgreSQL configuration invalid")
	}
	secondary, err := pgxpool.ParseConfig(defaultURL)
	if err != nil || secondary.ConnConfig.Port == primary.ConnConfig.Port {
		t.Fatal("separate disposable PostgreSQL instance required")
	}
	for _, owner := range []string{"go", "rust"} {
		t.Run(owner, func(t *testing.T) {
			f := newFixtureAt(t, 180*time.Second, defaultURL)
			var locks, connections, prepared int
			if f.pool.QueryRow(f.ctx, "SELECT current_setting('max_locks_per_transaction')::int,current_setting('max_connections')::int,current_setting('max_prepared_transactions')::int").Scan(&locks, &connections, &prepared) != nil || locks != 64 || connections != 100 || prepared != 0 {
				t.Fatal("default-capacity control settings differ")
			}
			t.Logf("actual default PostgreSQL lock settings: max_locks_per_transaction=%d max_connections=%d max_prepared_transactions=%d", locks, connections, prepared)
			st, view := largeHistory(8000)
			store := order.NewPostgres(f.pool)
			if store.Create(f.ctx, st, nil) != nil {
				t.Fatal("seed order failed")
			}
			v := &venue{tenant: f.tenant, orderID: st.OrderId, description: &venuepb.DescribeResponse{Mic: "MOCK", Account: "fixture-account", AccountVerified: true, ExchangeAccountId: "fixture-exchange-account", SupportedOrderTypes: []orderpb.OrderType{orderpb.OrderType_ORDER_TYPE_LIMIT}}, history: view}
			tr := startTransport(t, v)
			env := &envelopepb.Envelope{EventId: "observation", TenantId: f.tenant, EventType: execution.SubjectStateHealed, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, CorrelationId: "fixture-correlation"}
			payload := wire(t, &orderpb.StateHealed{OrderId: st.OrderId, Venue: st.Venue, State: st})
			if owner == "go" {
				err := f.service(t, tr).HandleRecovery(lineage(f.ctx, env), env, payload)
				var databaseError *pgconn.PgError
				if !errors.As(err, &databaseError) || databaseError.Code != "53200" {
					t.Fatalf("Go default-capacity control did not establish PostgreSQL resource exhaustion: %s", goErrorCategory(err))
				}
				t.Log("canonical Go default-capacity SQLSTATE=53200")
			} else {
				w := f.start(t, tr)
				r := w.call(map[string]any{"method": "Observation", "envelope": wire(t, env), "payload": payload, "at": clock(), "timeout_ms": 30000})
				if r.Status != "error" || r.Error != "Database" || !r.SessionsClosed {
					t.Fatalf("default-capacity outcome: status=%s error=%s sessions_closed=%t", r.Status, r.Error, r.SessionsClosed)
				}
				w.close(tr)
			}
			got, version, err := store.Load(f.ctx, st.OrderId)
			if err != nil || version != 0 || !proto.Equal(got, st) {
				t.Fatal("lock exhaustion altered order")
			}
			c, err := store.RecoveryCase(f.ctx, env.EventId)
			if err != nil || c.Checkpoint != 0 || c.Status == "corrected" {
				t.Fatal("lock exhaustion advanced recovery checkpoint")
			}
			for _, query := range []string{"SELECT count(*) FROM order_fills", "SELECT count(*) FROM order_executions", "SELECT count(*) FROM execution_fee_revisions", "SELECT count(*) FROM execution_recovery_targets", "SELECT count(*) FROM execution_recovery_acks", "SELECT count(*) FROM outbox WHERE event_type='order.order.execution_recovered'"} {
				var n int
				if f.pool.QueryRow(f.ctx, query).Scan(&n) != nil || n != 0 {
					t.Fatal("lock exhaustion partially committed financial owners")
				}
			}
			if v.queries.Load() != 1 || v.writes.Load() != 0 {
				t.Fatal("lock control missed actual history or attempted trading")
			}
		})
	}
}
