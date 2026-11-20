package order

import (
	"context"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/outbox"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/services/oms/internal/capital"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestPostgresFundedPartialFillAndAccountingHandoff(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := capital.Apply(ctx, pool, capital.CashEvent{PortfolioID: "fund", Currency: "USD", Revision: 1, Total: "100", Complete: true, ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	store := NewPostgres(pool)
	st := &orderpb.OrderState{OrderId: "funded", PortfolioId: "fund", InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY, OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT,
		OrderedQuantity: d(1, 0), LimitPrice: d(100, 0), LeavesQuantity: d(1, 0), Status: orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW}
	accepted, err := NewEmitter(&fakeBus{}).AcceptedFact(testCtx(), st)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateFunded(ctx, st, []outbox.Record{accepted}, []*commonpb.Money{{CurrencyCode: "USD", Amount: d(100, 0)}}, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	for index, price := range []int64{90, 100} {
		if index == 1 {
			store = NewPostgres(pool)
		} // new OMS store instance, same durable commitments
		fill := &orderpb.Fill{OrderId: "funded", FillId: "fill-" + string(rune('a'+index)), InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
			Quantity: d(5, -1), Price: d(price, 0), ExecutedAt: timestamppb.New(now)}
		current, version, err := store.Load(ctx, "funded")
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
		if err := NewPostgres(pool).Save(ctx, next, version+1, []outbox.Record{fact}, fill.FillId); err != ErrFillApplied {
			t.Fatalf("redelivered fill changed capital: %v", err)
		}
		var required, executed, reserved string
		if err := pool.QueryRow(ctx, `SELECT required_debit,executed_debit FROM capital_commitments WHERE order_id='funded'`).Scan(&required, &executed); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT reserved FROM capital_balances WHERE portfolio_id='fund' AND currency='USD'`).Scan(&reserved); err != nil {
			t.Fatal(err)
		}
		want := [][3]string{{"100", "45", "100"}, {"95", "95", "95"}}[index]
		if required != want[0] || executed != want[1] || reserved != want[2] {
			t.Fatalf("fill %d: required=%s executed=%s reserved=%s", index, required, executed, reserved)
		}
	}
	if err := capital.Apply(ctx, pool, capital.CashEvent{PortfolioID: "fund", Currency: "USD", Revision: 2, Total: "5", Complete: true, ObservedAt: now.Add(time.Second), Applied: []capital.Applied{{OrderID: "funded", Debit: "95"}}}); err != nil {
		t.Fatal(err)
	}
	var reserved string
	if err := pool.QueryRow(ctx, `SELECT reserved FROM capital_balances WHERE portfolio_id='fund' AND currency='USD'`).Scan(&reserved); err != nil || reserved != "0" {
		t.Fatalf("booked liability reserved=%s err=%v", reserved, err)
	}
}
