package recoveryconformance

import (
	"errors"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
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

func TestRustRecoveryDefaultLockCapacityOutcome(t *testing.T) {
	recoveryLockCapacity(t, "TEST_POSTGRES_DEFAULT_LOCK_URL", 64, false)
}
func TestRustRecoveryConstrainedLockCapacityRefusal(t *testing.T) {
	recoveryLockCapacity(t, "TEST_POSTGRES_CONSTRAINED_LOCK_URL", 10, true)
}

// PostgreSQL sizes a shared lock table; max_locks_per_transaction is not a
// per-transaction ceiling. Default capacity may complete or actually exhaust.
// The independently constrained server must demonstrate the owning SQLSTATE.
func recoveryLockCapacity(t *testing.T, variable string, expectedLocks int, mustRefuse bool) {
	binary(t)
	defaultURL := os.Getenv(variable)
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
	if mustRefuse {
		normal, err := pgxpool.ParseConfig(os.Getenv("TEST_POSTGRES_DEFAULT_LOCK_URL"))
		if err != nil || normal.ConnConfig.Port == secondary.ConnConfig.Port {
			t.Fatal("constrained and default instances must be independent")
		}
	}
	for _, owner := range []string{"go", "rust"} {
		t.Run(owner, func(t *testing.T) {
			f := newFixtureAt(t, 180*time.Second, defaultURL)
			var locks, connections, prepared int
			if f.pool.QueryRow(f.ctx, "SELECT current_setting('max_locks_per_transaction')::int,current_setting('max_connections')::int,current_setting('max_prepared_transactions')::int").Scan(&locks, &connections, &prepared) != nil || locks != expectedLocks || connections != 100 || prepared != 0 {
				t.Fatal("default-capacity control settings differ")
			}
			label := "default"
			if mustRefuse {
				label = "constrained"
			}
			t.Logf("actual %s PostgreSQL lock settings: max_locks_per_transaction=%d max_connections=%d max_prepared_transactions=%d", label, locks, connections, prepared)
			t.Logf("actual scoped PostgreSQL pool max_connections=%d", f.pool.Config().MaxConns)
			if f.pool.Config().MaxConns >= int32(connections) {
				t.Fatal("fixture pool can exhaust server connections before lock proof")
			}
			var activeConnections int
			if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database()").Scan(&activeConnections) != nil || activeConnections >= connections {
				t.Fatal("server connection headroom unavailable before lock proof")
			}
			t.Logf("actual PostgreSQL existing connections=%d max_connections=%d", activeConnections, connections)
			st, view := largeHistory(8000)
			store := order.NewPostgres(f.pool)
			if store.Create(f.ctx, st, nil) != nil {
				t.Fatal("seed order failed")
			}
			v := &venue{tenant: f.tenant, orderID: st.OrderId, description: &venuepb.DescribeResponse{Mic: "MOCK", Account: "fixture-account", AccountVerified: true, ExchangeAccountId: "fixture-exchange-account", SupportedOrderTypes: []orderpb.OrderType{orderpb.OrderType_ORDER_TYPE_LIMIT}}, history: view}
			tr := startTransport(t, v)
			env := &envelopepb.Envelope{EventId: "observation", TenantId: f.tenant, EventType: execution.SubjectStateHealed, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, CorrelationId: "fixture-correlation"}
			payload := wire(t, &orderpb.StateHealed{OrderId: st.OrderId, Venue: st.Venue, State: st})
			exhausted := false
			unconfirmed := false
			if owner == "go" {
				err := f.service(t, tr).HandleRecovery(lineage(f.ctx, env), env, payload)
				var databaseError *pgconn.PgError
				exhausted = errors.As(err, &databaseError) && databaseError.Code == "53200"
				unconfirmed = isFullBatchStall(err, st.OrderId)
				if !exhausted && err != nil && !unconfirmed {
					t.Fatalf("Go lock-capacity outcome is neither commit nor owning resource exhaustion: %s", goErrorCategory(err))
				}
				if exhausted {
					t.Logf("canonical Go %s-capacity SQLSTATE=53200", label)
				}
			} else {
				w := f.start(t, tr)
				r := w.call(map[string]any{"method": "Observation", "envelope": wire(t, env), "payload": payload, "at": clock(), "timeout_ms": 30000})
				exhausted = r.Status == "error" && r.Error == "DatabaseMemory"
				unconfirmed = r.Status == "error" && r.Error == "PublicationUnconfirmed"
				if !r.SessionsClosed || (!exhausted && !unconfirmed && r.Status != "ok") {
					t.Logf("unexpected lock-capacity outcome: status=%s error=%s sessions_closed=%t", r.Status, r.Error, r.SessionsClosed)
					// Keep the pre-close witness even if cleanup fails. Neither
					// observation admits the unexpected command outcome.
					d := observeLockOutcome(f, st, env.EventId)
					t.Log(d.summary("before_close"))
					w.close(tr)
					t.Log(observeLockOutcome(f, st, env.EventId).summary("after_joined_close"))
					t.Fatalf("lock-capacity outcome: status=%s error=%s sessions_closed=%t", r.Status, r.Error, r.SessionsClosed)
				}
				if exhausted {
					t.Logf("actual Rust %s-capacity SQLSTATE=53200", label)
				}
				w.close(tr)
			}
			if mustRefuse && !exhausted {
				t.Fatal("constrained server did not establish actual PostgreSQL resource exhaustion")
			}
			var usable int
			if f.pool.QueryRow(f.ctx, "SELECT 1").Scan(&usable) != nil || usable != 1 {
				t.Fatal("scoped pool unusable after lock-capacity outcome")
			}
			got, version, err := store.Load(f.ctx, st.OrderId)
			if err != nil {
				t.Fatal("order unavailable after lock-capacity outcome")
			}
			c, err := store.RecoveryCase(f.ctx, env.EventId)
			if err != nil {
				t.Fatal("recovery case unavailable after lock-capacity outcome")
			}
			if exhausted {
				if version != 0 || !proto.Equal(got, st) || c.Checkpoint != 0 || c.Status == "corrected" {
					t.Fatal("resource exhaustion advanced financial state")
				}
				for _, query := range []string{"SELECT count(*) FROM order_fills", "SELECT count(*) FROM order_executions", "SELECT count(*) FROM execution_fee_revisions", "SELECT count(*) FROM execution_recovery_targets", "SELECT count(*) FROM execution_recovery_acks", "SELECT count(*) FROM outbox WHERE event_type='order.order.execution_recovered'"} {
					var n int
					if f.pool.QueryRow(f.ctx, query).Scan(&n) != nil || n != 0 {
						t.Fatal("resource exhaustion partially committed financial owners")
					}
				}
			} else {
				if version != 1 || c.Checkpoint != 8000 || c.Status != "investigating" || got.Status != orderpb.OrderStatus_ORDER_STATUS_CANCELLED || dec.Cmp(got.FilledQuantity, &commonpb.Decimal{Coefficient: 8000}) != 0 || dec.Cmp(got.LeavesQuantity, &commonpb.Decimal{Coefficient: 1}) != 0 || dec.Cmp(got.AverageFillPrice, &commonpb.Decimal{Coefficient: 1005, Exponent: -1}) != 0 {
					t.Fatal("default capacity committed incorrect financial state")
				}
				for _, table := range []string{"order_fills", "order_executions", "execution_recovery_targets"} {
					var n int
					if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM "+table).Scan(&n) != nil || n != 8000 {
						t.Fatal("default capacity partial financial inventory")
					}
				}
				var records, pending, failed, acks, fees, expected int
				if f.pool.QueryRow(f.ctx, "SELECT count(*),count(*) FILTER(WHERE published_at IS NULL),count(*) FILTER(WHERE attempts<>0) FROM outbox WHERE event_type='order.order.execution_recovered'").Scan(&records, &pending, &failed) != nil || records != 8000 || failed != 0 {
					t.Fatal("default capacity recovered FACT inventory differs")
				}
				if unconfirmed && pending == 0 || !unconfirmed && pending != 0 {
					t.Fatal("publication outcome contradicts durable pending inventory")
				}
				if f.pool.QueryRow(f.ctx, "SELECT (SELECT count(*) FROM execution_recovery_acks),(SELECT count(*) FROM execution_fee_revisions),expected_ack_count FROM execution_recovery_cases WHERE case_id=$1", env.EventId).Scan(&acks, &fees, &expected) != nil || acks != 0 || fees != 0 || expected != 16000 {
					t.Fatal("default capacity fabricated book acknowledgements or fees")
				}
				t.Log("actual default capacity committed complete 8000-execution financial inventory")
			}
			if v.queries.Load() != 1 || v.writes.Load() != 0 {
				t.Fatal("lock control missed actual history or attempted trading")
			}
		})
	}
}
