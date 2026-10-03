package order

import (
	"context"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/services/oms/internal/capital"
)

func TestPostgresConfirmedFundedCancelReleasesOnlyUnexecutedCash(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := capital.Apply(ctx, pool, capital.CashEvent{PortfolioID: "fund", Currency: "USD", Revision: 1, Total: "100", Complete: true, ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	store := NewPostgres(pool)
	st := &orderpb.OrderState{OrderId: "cancel", PortfolioId: "fund", Status: orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW}
	if err := store.CreateFunded(ctx, st, nil, []*commonpb.Money{{CurrencyCode: "USD", Amount: d(80, 0)}}, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	st.Status = orderpb.OrderStatus_ORDER_STATUS_CANCELLED
	if err := store.SaveFundedTerminal(ctx, st, 0, nil, now); err != nil {
		t.Fatal(err)
	}
	var reserved string
	if err := pool.QueryRow(ctx, `SELECT reserved FROM capital_balances WHERE portfolio_id='fund' AND currency='USD'`).Scan(&reserved); err != nil || reserved != "0" {
		t.Fatalf("confirmed cancellation reserved=%s err=%v", reserved, err)
	}
	if err := store.SaveFundedTerminal(ctx, st, 0, nil, now); err != ErrConflict {
		t.Fatalf("duplicate cancellation=%v", err)
	}
}

func TestPostgresConfirmedFundedRejectionAndExpiryReleaseCash(t *testing.T) {
	for _, status := range []orderpb.OrderStatus{
		orderpb.OrderStatus_ORDER_STATUS_REJECTED,
		orderpb.OrderStatus_ORDER_STATUS_EXPIRED,
	} {
		t.Run(status.String(), func(t *testing.T) {
			pool := newPool(t)
			ctx := context.Background()
			now := time.Now().UTC()
			if err := capital.Apply(ctx, pool, capital.CashEvent{PortfolioID: "fund", Currency: "USD", Revision: 1, Total: "100", Complete: true, ObservedAt: now}); err != nil {
				t.Fatal(err)
			}
			store := NewPostgres(pool)
			st := &orderpb.OrderState{OrderId: "terminal", PortfolioId: "fund", Status: orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW}
			if err := store.CreateFunded(ctx, st, nil, []*commonpb.Money{{CurrencyCode: "USD", Amount: d(80, 0)}}, now, time.Minute); err != nil {
				t.Fatal(err)
			}
			st.Status = status
			if err := store.SaveFundedTerminal(ctx, st, 0, nil, now); err != nil {
				t.Fatal(err)
			}
			var reserved string
			if err := pool.QueryRow(ctx, `SELECT reserved FROM capital_balances WHERE portfolio_id='fund' AND currency='USD'`).Scan(&reserved); err != nil || reserved != "0" {
				t.Fatalf("terminal status %s reserved=%s err=%v", status, reserved, err)
			}
		})
	}
}
