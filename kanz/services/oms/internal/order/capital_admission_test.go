package order

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/outbox"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/services/oms/internal/capital"
)

func TestPostgresFundedAdmissionCompetitionAndRedelivery(t *testing.T) {
	for _, sameID := range []bool{false, true} {
		t.Run(fmt.Sprint("same_id=", sameID), func(t *testing.T) {
			pool := newPool(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			now := time.Now().UTC()
			if err := capital.Apply(ctx, pool, capital.CashEvent{PortfolioID: "fund", Currency: "USD", Revision: 1, Total: "250", Complete: true, ObservedAt: now}); err != nil {
				t.Fatal(err)
			}
			debits := []*commonpb.Money{{CurrencyCode: "USD", Amount: d(200, 0)}}
			results := make(chan error, 12)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := range 12 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					id := fmt.Sprintf("order-%d", i)
					if sameID {
						id = "same"
					}
					st := &orderpb.OrderState{OrderId: id, PortfolioId: "fund", Status: orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW}
					fact, err := NewEmitter(&fakeBus{}).AcceptedFact(testCtx(), st)
					if err != nil {
						results <- err
						return
					}
					<-start
					results <- NewPostgres(pool).CreateFunded(ctx, st, []outbox.Record{fact}, debits, now, time.Minute)
				}()
			}
			close(start)
			wg.Wait()
			close(results)
			wins := 0
			for err := range results {
				if err == nil {
					wins++
					continue
				}
				want := capital.ErrInsufficient
				if sameID {
					want = ErrExists
				}
				if !errors.Is(err, want) {
					t.Fatalf("unexpected refusal: %v", err)
				}
			}
			if wins != 1 {
				t.Fatalf("admitted %d orders", wins)
			}
			for _, table := range []string{"orders", "capital_commitments", "capital_members", "outbox"} {
				var count int
				if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 1 {
					t.Fatalf("%s count=%d err=%v", table, count, err)
				}
			}
			var reserved string
			if err := pool.QueryRow(ctx, `SELECT reserved FROM capital_balances`).Scan(&reserved); err != nil || reserved != "200" {
				t.Fatalf("reserved=%s err=%v", reserved, err)
			}
			if sameID {
				st := &orderpb.OrderState{OrderId: "same", PortfolioId: "fund", Status: orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW}
				if err := NewPostgres(pool).CreateFunded(ctx, st, nil, debits, now.Add(time.Hour), time.Minute); !errors.Is(err, ErrExists) {
					t.Fatalf("stale redelivery: %v", err)
				}
			}
		})
	}
}

func TestPostgresFundedAdmissionOutboxFailureRollsBackCash(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := capital.Apply(ctx, pool, capital.CashEvent{PortfolioID: "fund", Currency: "USD", Revision: 1, Total: "250", Complete: true, ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	// Inject a database write failure after the reservation and order INSERT.
	if _, err := pool.Exec(ctx, `ALTER TABLE outbox ADD CONSTRAINT refuse_funded_test CHECK (false)`); err != nil {
		t.Fatal(err)
	}
	st := &orderpb.OrderState{OrderId: "rollback", PortfolioId: "fund", Status: orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW}
	fact, err := NewEmitter(&fakeBus{}).AcceptedFact(testCtx(), st)
	if err != nil {
		t.Fatal(err)
	}
	err = NewPostgres(pool).CreateFunded(ctx, st, []outbox.Record{fact}, []*commonpb.Money{{CurrencyCode: "USD", Amount: d(200, 0)}}, now, time.Minute)
	if err == nil {
		t.Fatal("failed outbox admitted order")
	}
	for _, table := range []string{"orders", "capital_commitments", "capital_members", "outbox"} {
		var count int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s count=%d err=%v", table, count, err)
		}
	}
	var reserved string
	if err := pool.QueryRow(ctx, `SELECT reserved FROM capital_balances`).Scan(&reserved); err != nil || reserved != "0" {
		t.Fatalf("reserved=%s err=%v", reserved, err)
	}
}
