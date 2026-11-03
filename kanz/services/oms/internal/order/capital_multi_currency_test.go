package order

import (
	"context"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/outbox"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/services/oms/internal/capital"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestFundedFeeCurrencyFollowsFillAndTerminal(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, cash := range []struct{ currency, total string }{{"BNB", "3"}, {"USD", "100"}} {
		if err := capital.Apply(ctx, pool, capital.CashEvent{PortfolioID: "fund", Currency: cash.currency, Revision: 1, Total: dec.Exact(cash.total), Complete: true, ObservedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	store := NewPostgres(pool)
	st := &orderpb.OrderState{OrderId: "fee-asset", PortfolioId: "fund", InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
		OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT, OrderedQuantity: d(1, 0), LeavesQuantity: d(1, 0), LimitPrice: d(100, 0), Status: orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW}
	debits := []*commonpb.Money{{CurrencyCode: "BNB", Amount: d(2, 0)}, {CurrencyCode: "USD", Amount: d(100, 0)}}
	if err := store.CreateFunded(ctx, st, nil, debits, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	fill := &orderpb.Fill{OrderId: st.OrderId, FillId: "fee-fill", InstrumentId: st.InstrumentId, Side: st.Side,
		Quantity: d(1, 0), Price: d(90, 0), Fee: &commonpb.Money{CurrencyCode: "BNB", Amount: d(1, 0)}, ExecutedAt: timestamppb.New(now)}
	current, version, err := store.Load(ctx, st.OrderId)
	if err != nil {
		t.Fatal(err)
	}
	next, err := ApplyFill(current, fill, now)
	if err != nil {
		t.Fatal(err)
	}
	fact, err := NewEmitter(&fakeBus{}).FillFact(testCtx(), fill, next)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, next, version, []outbox.Record{fact}, fill.FillId); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []struct{ currency, executed string }{{"BNB", "1"}, {"USD", "90"}} {
		var got string
		if err := pool.QueryRow(ctx, `SELECT executed_debit FROM capital_commitments WHERE order_id=$1 AND currency=$2`, st.OrderId, expected.currency).Scan(&got); err != nil || got != expected.executed {
			t.Fatalf("%s executed=%s err=%v", expected.currency, got, err)
		}
	}
}
