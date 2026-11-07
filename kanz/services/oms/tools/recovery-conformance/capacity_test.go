package recoveryconformance

import (
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/services/oms/internal/order"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"strings"
	"testing"
)

// This is an explicit bounded-domain difference, not a Go rollback-parity
// claim. Go can commit a >1MiB recovered FACT before transport refusal. Rust
// refuses this record before its financial transaction, even though the full
// venue history is a valid response inside the recovery-only4MiB receive bound.
func TestRustRecoveryOversizedSingleFactRefusal(t *testing.T) {
	for _, owner := range []string{"go", "rust"} {
		t.Run(owner, func(t *testing.T) {
			f := newFixture(t)
			st := initial()
			store := order.NewPostgres(f.pool)
			if store.Create(f.ctx, st, nil) != nil {
				t.Fatal("seed canonical order failed")
			}
			view := history(st)
			view.Fills = view.Fills[:1]
			view.ExecutedQuantity = &commonpb.Decimal{Coefficient: 1}
			fill := view.Fills[0]
			unknown := append([]byte(nil), fill.ProtoReflect().GetUnknown()...)
			unknown = protowire.AppendBytes(protowire.AppendTag(unknown, 1001, protowire.BytesType), []byte(strings.Repeat("x", (1<<20)+1024)))
			fill.ProtoReflect().SetUnknown(unknown)
			if proto.Size(fill) <= 1<<20 || proto.Size(view) >= 4<<20 {
				t.Fatal("single-record bound fixture does not discriminate")
			}
			v := &venue{tenant: f.tenant, orderID: st.OrderId, description: &venuepb.DescribeResponse{Mic: "MOCK", Account: "fixture-account", AccountVerified: true, ExchangeAccountId: "fixture-exchange-account", SupportedOrderTypes: []orderpb.OrderType{orderpb.OrderType_ORDER_TYPE_LIMIT}}, history: view}
			tr := startTransport(t, v)
			env := &envelopepb.Envelope{EventId: "observation", TenantId: f.tenant, EventType: execution.SubjectStateHealed, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, CorrelationId: "fixture-correlation"}
			payload := wire(t, &orderpb.StateHealed{OrderId: st.OrderId, Venue: st.Venue, State: st})
			if owner == "rust" {
				w := f.start(t, tr)
				r := w.call(map[string]any{"method": "Observation", "envelope": wire(t, env), "payload": payload, "at": clock(), "timeout_ms": 30000})
				if r.Status != "error" || r.Error != "Capacity" || !r.SessionsClosed {
					t.Fatal("oversized emitted record was not explicitly refused")
				}
				w.close(tr)
			} else {
				err := f.service(t, tr).HandleRecovery(lineage(f.ctx, env), env, payload)
				// Broker configuration is observed by ensureBroker, not inferred from the
				// repository. A handler error here is not evidence of transaction rollback.
				t.Logf("canonical Go oversized-record handler category=%s", goErrorCategory(err))
			}
			got, version, err := store.Load(f.ctx, st.OrderId)
			if err != nil {
				t.Fatal("durable order unavailable")
			}
			c, err := store.RecoveryCase(f.ctx, env.EventId)
			if err != nil {
				t.Fatal("durable case unavailable")
			}
			if owner == "rust" {
				if version != 0 || !proto.Equal(got, st) || c.Checkpoint != 0 {
					t.Fatal("bounded refusal changed financial state")
				}
				for _, query := range []string{"SELECT count(*) FROM order_fills", "SELECT count(*) FROM order_executions", "SELECT count(*) FROM execution_fee_revisions", "SELECT count(*) FROM execution_recovery_targets", "SELECT count(*) FROM execution_recovery_acks", "SELECT count(*) FROM outbox WHERE event_type='order.order.execution_recovered'"} {
					var n int
					if f.pool.QueryRow(f.ctx, query).Scan(&n) != nil || n != 0 {
						t.Fatal("oversized-record refusal partially committed")
					}
				}
			} else {
				if c.Checkpoint != 1 || version != 1 || dec.Cmp(got.FilledQuantity, &commonpb.Decimal{Coefficient: 1}) != 0 {
					t.Fatalf("Go bounded-domain comparison did not reach committed record: version=%d checkpoint=%d", version, c.Checkpoint)
				}
				var records int
				if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM outbox WHERE event_type='order.order.execution_recovered' AND octet_length(payload)>1048576").Scan(&records) != nil || records != 1 {
					t.Fatal("Go oversized durable FACT absent")
				}
			}
			if v.queries.Load() != 1 || v.writes.Load() != 0 {
				t.Fatal("bounded response did not exercise actual history-only path")
			}
		})
	}
}
