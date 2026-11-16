package recoveryconformance

import (
	"errors"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/services/oms/internal/order"
	"google.golang.org/protobuf/proto"
	"testing"
)

// Preserve immutable mapping/history rows. A legitimate order CAS changes the
// current portfolio after the history was frozen; recovery must recheck binding.
func TestRustRecoveryFrozenMappingAttribution(t *testing.T) {
	for _, owner := range []string{"go", "rust"} {
		t.Run(owner, func(t *testing.T) {
			f := prepareFee(t, false)
			store := order.NewPostgres(f.pool)
			current, version, err := store.Load(f.ctx, f.state.OrderId)
			if err != nil {
				t.Fatal("load order failed")
			}
			current.PortfolioId = "different-fixture-portfolio"
			if store.Save(f.ctx, current, version, nil, "") != nil {
				t.Fatal("canonical order attribution CAS failed")
			}
			before, err := store.RecoveryCase(f.ctx, f.env.EventId)
			if err != nil {
				t.Fatal("case unavailable")
			}
			var beforeOutbox int
			if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM outbox").Scan(&beforeOutbox) != nil {
				t.Fatal("outbox count failed")
			}
			v := &venue{tenant: f.tenant, orderID: current.OrderId, description: &venuepb.DescribeResponse{Mic: "MOCK", Account: "fixture-account", AccountVerified: true, ExchangeAccountId: "fixture-exchange-account", SupportedOrderTypes: []orderpb.OrderType{orderpb.OrderType_ORDER_TYPE_LIMIT}}, history: &venuepb.QueryOrderResponse{}}
			tr := startTransport(t, v)
			if owner == "go" {
				err := f.service(t, tr).HandleRecovery(lineage(f.ctx, f.env), f.env, f.payload)
				if !errors.Is(err, order.ErrRecoveryEvidenceConflict) {
					t.Fatalf("mapping refusal differs: %s", goErrorCategory(err))
				}
			} else {
				w := f.start(t, tr)
				result := w.call(map[string]any{"method": "Observation", "envelope": wire(t, f.env), "payload": f.payload, "at": clock(), "timeout_ms": 30000})
				if result.Status != "error" || result.Error != "RecoveryEvidenceConflict" || !result.SessionsClosed {
					t.Fatalf("mapping refusal: status=%s error=%s sessions_closed=%t", result.Status, result.Error, result.SessionsClosed)
				}
				w.close(tr)
			}
			got, nextVersion, err := store.Load(f.ctx, current.OrderId)
			if err != nil || nextVersion != version+1 || !proto.Equal(got, current) {
				t.Fatal("mapping refusal changed order")
			}
			after, err := store.RecoveryCase(f.ctx, before.ID)
			if err != nil || after.Status != before.Status || after.Version != before.Version || after.Checkpoint != before.Checkpoint {
				t.Fatal("mapping refusal acknowledged or blocked case")
			}
			var afterOutbox int
			if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM outbox").Scan(&afterOutbox) != nil || afterOutbox != beforeOutbox {
				t.Fatal("mapping refusal emitted FACT")
			}
			for _, table := range []string{"execution_recovery_targets", "execution_recovery_acks", "execution_fee_revisions"} {
				var n int
				if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM "+table).Scan(&n) != nil || n != 0 {
					t.Fatal("mapping refusal changed financial owners")
				}
			}
			if v.queries.Load() != 0 || v.writes.Load() != 0 {
				t.Fatal("frozen mapping refusal queried or traded")
			}
		})
	}
}
