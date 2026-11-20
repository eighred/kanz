package storeconformance

import (
	"context"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/order"
	"google.golang.org/protobuf/proto"
)

func (f *fixture) dispatchRequest(child *orderpb.OrderState) map[string]any {
	next := proto.Clone(child).(*orderpb.OrderState)
	next.Status = orderpb.OrderStatus_ORDER_STATUS_ROUTED
	req := f.request(next, 0, nil)
	fact, err := order.NewEmitter(nil).RoutedFact(bus.WithTenantID(f.ctx, f.tenant), child.OrderId, "XSIM", "", at)
	if err != nil {
		f.t.Fatal(err)
	}
	req["records"] = []map[string]any{factJSON(fact)}
	return req
}

func TestRustScheduledChild(t *testing.T) {
	for _, condition := range []string{"missing", "other-tenant", "other-portfolio", "terminal", "quarantine", "cancelling"} {
		t.Run(condition, func(t *testing.T) {
			f := newFixture(t)
			child := f.initial()
			child.ParentOrderId = "parent"
			parent := f.initial()
			parent.OrderId = child.ParentOrderId
			parent.Status = orderpb.OrderStatus_ORDER_STATUS_WORKING_SCHEDULED
			pool := f.pool
			visible := 1
			switch condition {
			case "other-tenant":
				pool = f.open("tenant-b")
				visible = 0
			case "other-portfolio":
				parent.PortfolioId = "other-portfolio"
			case "terminal":
				parent.Status = orderpb.OrderStatus_ORDER_STATUS_CANCELLED
			case "quarantine":
				parent.Quarantine = &orderpb.OrderQuarantine{}
			case "cancelling":
				parent.CancellationRequest = &orderpb.OrderCancellationRequest{}
			case "missing":
				visible = 0
			}
			if condition != "missing" {
				if err := order.NewPostgres(pool).Create(f.ctx, parent, nil); err != nil {
					t.Fatal(err)
				}
			}
			expect(t, run(t, f.createRequest(child)), "ParentStopped")
			expect(t, run(t, f.dispatchRequest(child)), "ParentStopped")
			expect(t, run(t, f.request(child, 0, nil)), "Conflict")
			f.rowCounts(visible, 0)
		})
	}
	t.Run("working", func(t *testing.T) {
		f := newFixture(t)
		child := f.initial()
		child.ParentOrderId = "parent"
		f.createQueryParent(child.ParentOrderId, child.PortfolioId)
		self := f.initial()
		self.ParentOrderId, self.OrderId = "parent", "parent"
		expect(t, run(t, f.dispatchRequest(self)), "ParentStopped")
		expect(t, run(t, f.createRequest(child)), "committed")
		// Dispatch must check the same parent that admitted this child.
		f.createQueryParent("other", child.PortfolioId)
		for _, id := range []string{"other", ""} {
			changed := proto.Clone(child).(*orderpb.OrderState)
			changed.ParentOrderId = id
			expect(t, run(t, f.dispatchRequest(changed)), "Conflict")
		}
		expect(t, run(t, f.dispatchRequest(child)), "committed")
		child.Status = orderpb.OrderStatus_ORDER_STATUS_ROUTED
		f.assertLoad(child, 1)
		f.rowCounts(3, 2)
	})
	for _, operation := range []string{"create", "dispatch"} {
		for _, outcome := range []string{"commit", "rollback"} {
			t.Run(operation+"-cancel-"+outcome, func(t *testing.T) {
				f := newFixture(t)
				child := f.initial()
				child.ParentOrderId = "parent"
				parent := f.createQueryParent(child.ParentOrderId, child.PortfolioId)
				req := f.createRequest(child)
				orders, facts := 1, 0
				if operation == "dispatch" {
					expect(t, run(t, req), "committed")
					orders, facts = 2, 1
					req = f.dispatchRequest(child)
				}
				tx, err := f.pool.Begin(f.ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(context.Background())
				parent.CancellationRequest = &orderpb.OrderCancellationRequest{}
				if _, err = tx.Exec(f.ctx, "UPDATE orders SET state=$1, version=version+1 WHERE order_id=$2", wire(t, parent), parent.OrderId); err != nil {
					t.Fatal(err)
				}
				done := make(chan string, 1)
				go func() { defer close(done); done <- run(t, req) }()
				defer func() { _ = tx.Rollback(context.Background()); <-done }()
				// Use the lock owner's session as observer. Only the Rust
				// transaction is competing for this private schema's parent.
				deadline := time.Now().Add(6 * time.Second)
				for {
					var waiting bool
					err = tx.QueryRow(f.ctx, "SELECT EXISTS (SELECT FROM pg_locks WHERE NOT granted AND pg_backend_pid() = ANY(pg_blocking_pids(pid)))").Scan(&waiting)
					if err != nil {
						t.Fatal(err)
					}
					if waiting {
						break
					}
					select {
					case result := <-done:
						t.Fatalf("Rust %s completed before parent lock release: %s", operation, result)
					default:
					}
					if time.Now().After(deadline) {
						t.Fatal("no PostgreSQL waiter on cancellation transaction")
					}
					time.Sleep(10 * time.Millisecond)
				}
				if operation == "dispatch" {
					var id string
					if err = tx.QueryRow(f.ctx, "SELECT order_id FROM orders WHERE order_id=$1 FOR UPDATE NOWAIT", child.OrderId).Scan(&id); err != nil {
						t.Fatalf("dispatch locked child before parent: %v", err)
					}
				}
				if outcome == "rollback" {
					if err = tx.Rollback(f.ctx); err != nil {
						t.Fatal(err)
					}
					expect(t, <-done, "committed")
					f.rowCounts(2, facts+1)
					return
				}
				if err = tx.Commit(f.ctx); err != nil {
					t.Fatal(err)
				}
				expect(t, <-done, "ParentStopped")
				f.rowCounts(orders, facts)
				if operation == "dispatch" {
					f.assertLoad(child, 0)
					expect(t, run(t, f.createRequest(child)), "Exists")
					child.Status = orderpb.OrderStatus_ORDER_STATUS_CANCELLED
					expect(t, run(t, f.request(child, 0, nil)), "committed")
					f.assertLoad(child, 1)
					f.rowCounts(orders, facts)
				}
			})
		}
	}
}
