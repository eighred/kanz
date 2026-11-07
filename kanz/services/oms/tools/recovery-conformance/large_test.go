package recoveryconformance

import (
	"fmt"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/fillfact"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/order"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"testing"
	"time"
)

func largeHistory(count int) (*orderpb.OrderState, *venuepb.QueryOrderResponse) {
	st := initial()
	st.OrderedQuantity.Coefficient = int64(count + 1)
	seed := history(st).Fills[0]
	view := &venuepb.QueryOrderResponse{State: venuepb.OrderViewState_ORDER_VIEW_STATE_CANCELLED, ExecutedQuantity: &commonpb.Decimal{Coefficient: int64(count)}}
	for i := range count {
		fill := proto.Clone(seed).(*orderpb.Fill)
		fill.FillId = fmt.Sprintf("execution-%05d", i)
		fill.VenueExecutionId = fmt.Sprintf("remote-%05d", i)
		fill.Price.Coefficient = int64(100 + i%2)
		unknown := append([]byte(nil), fill.ProtoReflect().GetUnknown()...)
		fill.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(unknown, 1001, protowire.BytesType), make([]byte, 96)))
		view.Fills = append(view.Fills, fill)
	}
	return st, view
}

func TestRustRecoveryComplete8000History(t *testing.T) {
	for _, owner := range []string{"go", "rust"} {
		t.Run(owner, func(t *testing.T) {
			f := newFixtureWithin(t, 15*time.Minute)
			st, view := largeHistory(8000)
			if n := proto.Size(view); n <= 1<<20 || n >= 4<<20 {
				t.Fatal("8000 fixture must cross standard response bound but fit recovery budget")
			}
			// These are actual server settings, not a fixture assertion that the shared
			// lock table has unlimited capacity. The successful single transaction below
			// is the decisive qualification; no alias locks or commits are batched away.
			var locks, connections, prepared int
			if f.pool.QueryRow(f.ctx, "SELECT current_setting('max_locks_per_transaction')::int,current_setting('max_connections')::int,current_setting('max_prepared_transactions')::int").Scan(&locks, &connections, &prepared) != nil {
				t.Fatal("actual PostgreSQL lock settings unavailable")
			}
			t.Logf("actual PostgreSQL lock settings: max_locks_per_transaction=%d max_connections=%d max_prepared_transactions=%d", locks, connections, prepared)
			if locks <= 0 || connections <= 0 || prepared < 0 {
				t.Fatal("invalid PostgreSQL lock capacity settings")
			}
			store := order.NewPostgres(f.pool)
			if store.Create(f.ctx, st, nil) != nil {
				t.Fatal("seed canonical order failed")
			}
			v := &venue{tenant: f.tenant, orderID: st.OrderId, description: &venuepb.DescribeResponse{Mic: "MOCK", Account: "fixture-account", AccountVerified: true, ExchangeAccountId: "fixture-exchange-account", SupportedOrderTypes: []orderpb.OrderType{orderpb.OrderType_ORDER_TYPE_LIMIT}}, history: view}
			tr := startTransport(t, v)
			env := &envelopepb.Envelope{EventId: "observation", TenantId: f.tenant, EventType: execution.SubjectStateHealed, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, CorrelationId: "fixture-correlation"}
			payload := wire(t, &orderpb.StateHealed{OrderId: st.OrderId, Venue: st.Venue, State: st})
			receive := f.recoveredWithin(t, 8000, 10*time.Minute)
			var w *worker
			var svc *order.Service
			if owner == "rust" {
				w = f.startWithin(t, tr, 15*time.Minute)
			} else {
				svc = f.service(t, tr)
			}
			observe := func() bool {
				if w == nil {
					err := svc.HandleRecovery(lineage(f.ctx, env), env, payload)
					if err != nil && !isFullBatchStall(err, st.OrderId) {
						t.Fatalf("large-history Go failed before publication drain: category=%s", goErrorCategory(err))
					}
					return err == nil
				}
				r := w.call(map[string]any{"method": "Observation", "envelope": wire(t, env), "payload": payload, "at": clock(), "timeout_ms": 30000})
				if !r.SessionsClosed || (r.Status != "ok" && (r.Status != "error" || r.Error != "PublicationUnconfirmed")) {
					t.Fatalf("large-history owner failed before bounded publication drain: status=%s error=%s sessions_closed=%t", r.Status, r.Error, r.SessionsClosed)
				}
				return r.Status == "ok"
			}
			// A full relay batch is not empty. Retry this exact committed observation
			// until every pending FACT is acknowledged; never re-query venue history.
			finished := false
			for range 128 {
				success := observe()
				var pending int
				if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM outbox WHERE published_at IS NULL").Scan(&pending) != nil {
					t.Fatal("pending outbox read failed")
				}
				if pending == 0 {
					if !success {
						t.Fatal("empty outbox still reported unconfirmed publication")
					}
					finished = true
					break
				}
				if success {
					t.Fatal("handler claimed publication with pending required facts")
				}
			}
			if !finished {
				t.Fatal("bounded large-history drain incomplete")
			}
			records := receive()
			seen := make(map[string]bool, 8000)
			originals := make(map[string]*orderpb.Fill, 8000)
			for _, fill := range view.Fills {
				originals[fill.FillId] = fill
			}
			for _, record := range records {
				original := originals[record.Fill.FillId]
				candidate := proto.Clone(record.Fill).(*orderpb.Fill)
				candidate.Recovery = nil
				if original == nil || seen[candidate.FillId] || !proto.Equal(candidate, original) {
					t.Fatal("large-history FACT inventory or raw unknowns differ")
				}
				seen[candidate.FillId] = true
			}
			got, version, err := store.Load(f.ctx, st.OrderId)
			// Independent exact arithmetic:4000 executions each at100 and101 =>100.5;
			// qty8000 leaves1. Terminal cancellation remains preserved.
			if err != nil || version != 1 || got.Status != orderpb.OrderStatus_ORDER_STATUS_CANCELLED || dec.Cmp(got.FilledQuantity, &commonpb.Decimal{Coefficient: 8000}) != 0 || dec.Cmp(got.LeavesQuantity, &commonpb.Decimal{Coefficient: 1}) != 0 || dec.Cmp(got.AverageFillPrice, &commonpb.Decimal{Coefficient: 1005, Exponent: -1}) != 0 {
				t.Fatal("large-history financial state differs")
			}
			for _, record := range records {
				if !proto.Equal(record.State, got) {
					t.Fatal("large-history FACT state differs")
				}
			}
			for _, table := range []string{"order_fills", "order_executions", "execution_recovery_targets"} {
				var n int
				if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM "+table).Scan(&n) != nil || n != 8000 {
					t.Fatal("large-history durable inventory incomplete")
				}
			}
			c, err := store.RecoveryCase(f.ctx, env.EventId)
			if err != nil || c.Checkpoint != 8000 || c.Status != "investigating" {
				t.Fatal("large-history checkpoint differs")
			}
			for index, record := range records {
				for _, subject := range []string{fillfact.RecoveryPositionApplied, fillfact.RecoveryLedgerApplied} {
					ack, err := fillfact.RecoveryAck(bus.WithTenantID(f.ctx, f.tenant), subject, record.Fill, instant)
					if err != nil {
						t.Fatal(err)
					}
					ackEnv := &envelopepb.Envelope{EventId: "ack", TenantId: f.tenant, EventType: subject, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT}
					if w == nil {
						if svc.HandleRecoveryAck(lineage(f.ctx, ackEnv), ackEnv, ack.Payload) != nil {
							t.Fatal("Go large-history ACK failed")
						}
					} else {
						r := w.call(map[string]any{"method": "Ack", "envelope": wire(t, ackEnv), "payload": ack.Payload, "at": clock(), "timeout_ms": 30000})
						if r.Status != "ok" || !r.SessionsClosed {
							t.Fatal("Rust large-history ACK failed")
						}
					}
					if index == 7999 {
						current, err := store.RecoveryCase(f.ctx, env.EventId)
						expected := "investigating"
						if subject == fillfact.RecoveryLedgerApplied {
							expected = "corrected"
						}
						if err != nil || current.Status != expected {
							t.Fatal("large-history completion did not require both books for all8000executions")
						}
					}
				}
			}
			var ackCount int
			if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM execution_recovery_acks").Scan(&ackCount) != nil || ackCount != 16000 {
				t.Fatal("large-history ACK inventory incomplete")
			}
			if v.queries.Load() != 1 || v.writes.Load() != 0 {
				t.Fatal("large-history retries queried or traded")
			}
			if w != nil {
				w.peakMemory(t)
				w.close(tr)
			}
		})
	}
}

func TestRustRecovery8001HistoryRefusal(t *testing.T) {
	for _, owner := range []string{"go", "rust"} {
		t.Run(owner, func(t *testing.T) {
			f := newFixture(t)
			st, view := largeHistory(8001)
			if proto.Size(view) >= 4<<20 {
				t.Fatal("row-limit fixture accidentally hits byte limit")
			}
			store := order.NewPostgres(f.pool)
			if store.Create(f.ctx, st, nil) != nil {
				t.Fatal("seed order failed")
			}
			v := &venue{tenant: f.tenant, orderID: st.OrderId, description: &venuepb.DescribeResponse{Mic: "MOCK", Account: "fixture-account", AccountVerified: true, ExchangeAccountId: "fixture-exchange-account", SupportedOrderTypes: []orderpb.OrderType{orderpb.OrderType_ORDER_TYPE_LIMIT}}, history: view}
			tr := startTransport(t, v)
			env := &envelopepb.Envelope{EventId: "observation", TenantId: f.tenant, EventType: execution.SubjectStateHealed, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, CorrelationId: "fixture-correlation"}
			payload := wire(t, &orderpb.StateHealed{OrderId: st.OrderId, Venue: st.Venue, State: st})
			if !f.invoke(t, owner, tr, "Observation", env, payload) {
				t.Fatal("durable history refusal did not acknowledge blocked lifecycle")
			}
			c, err := store.RecoveryCase(f.ctx, env.EventId)
			if err != nil || c.Status != "blocked" || c.Checkpoint != 0 {
				t.Fatal("oversized history was not durably blocked")
			}
			got, version, err := store.Load(f.ctx, st.OrderId)
			if err != nil || version != 0 || !proto.Equal(st, got) {
				t.Fatal("row refusal mutated order")
			}
			for _, table := range []string{"order_fills", "order_executions", "execution_recovery_targets", "execution_recovery_acks"} {
				var n int
				if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM "+table).Scan(&n) != nil || n != 0 {
					t.Fatal("row refusal committed partial history")
				}
			}
		})
	}
}
