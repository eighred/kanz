package order

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/outbox"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/services/oms/internal/capital"
)

func TestPostgresFundedAmendSharesCashAndOrderCAS(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := capital.Apply(ctx, pool, capital.CashEvent{PortfolioID: "fund", Currency: "USD", Revision: 1, Total: "100", Complete: true, ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	store := NewPostgres(pool)
	st := &orderpb.OrderState{OrderId: "amend", PortfolioId: "fund", InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
		OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT, Status: orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW,
		OrderedQuantity: d(1, 0), LimitPrice: d(50, 0), LeavesQuantity: d(1, 0)}
	if err := store.CreateFunded(ctx, st, nil, []*commonpb.Money{{CurrencyCode: "USD", Amount: d(50, 0)}}, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	other := &orderpb.OrderState{OrderId: "other", PortfolioId: "fund", Status: orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW}
	if err := store.CreateFunded(ctx, other, nil, []*commonpb.Money{{CurrencyCode: "USD", Amount: d(30, 0)}}, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	st.LimitPrice = d(80, 0)
	if err := store.SaveFundedAmend(ctx, st, 0, []outbox.Record{}, []*commonpb.Money{{CurrencyCode: "USD", Amount: d(80, 0)}}, now); !errors.Is(err, capital.ErrInsufficient) {
		t.Fatalf("unfunded amendment: %v", err)
	}
	loaded, version, err := store.Load(ctx, "amend")
	if err != nil || version != 0 || loaded.GetLimitPrice().GetCoefficient() != 50 {
		t.Fatalf("refused amendment changed state: %v %d %v", loaded, version, err)
	}
	var reserved string
	if err := pool.QueryRow(ctx, `SELECT reserved FROM capital_balances WHERE portfolio_id='fund' AND currency='USD'`).Scan(&reserved); err != nil || reserved != "80" {
		t.Fatalf("reserved=%s err=%v", reserved, err)
	}
	st.LimitPrice = d(70, 0)
	if err := store.SaveFundedAmend(ctx, st, 0, nil, []*commonpb.Money{{CurrencyCode: "USD", Amount: d(70, 0)}}, now); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveFundedAmend(ctx, st, 0, nil, []*commonpb.Money{{CurrencyCode: "USD", Amount: d(70, 0)}}, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale amendment: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT reserved FROM capital_balances WHERE portfolio_id='fund' AND currency='USD'`).Scan(&reserved); err != nil || reserved != "100" {
		t.Fatalf("amended reserved=%s err=%v", reserved, err)
	}
}
