package projection_test

import (
	"context"
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	marketpb "github.com/kanz-eng/kanz-schemas-go/market/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kanz-eng/kanz/internal/marketdata/mark"
	"github.com/kanz-eng/kanz/services/tv-sync/internal/projection"
)

func d(n int64) *commonpb.Decimal { return &commonpb.Decimal{Coefficient: n} }

func tradeEvent(t *testing.T, instrument string, price int64) []byte {
	t.Helper()
	b, err := proto.Marshal(&marketpb.MarketDataEvent{
		InstrumentId: instrument,
		EventTime:    timestamppb.Now(),
		Data:         &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{Price: d(price)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// End to end: a fill + a live mark makes the projection compute floating
// unrealized P&L dynamically, no manual trigger — the M2 seam resolved.
func TestMark_DrivesUnrealizedPnL(t *testing.T) {
	m := mark.New(time.Now, 0)
	proj := projection.New(time.Now, m)

	// Buy 2 BTC @ 100.
	st := &orderpb.OrderState{
		OrderId: "o1", PortfolioId: "fund-alpha", InstrumentId: "BTC", Side: orderpb.Side_SIDE_BUY,
		OrderType: orderpb.OrderType_ORDER_TYPE_MARKET, OrderedQuantity: d(2),
		FilledQuantity: d(2), LeavesQuantity: d(0), AverageFillPrice: d(100),
		Status: orderpb.OrderStatus_ORDER_STATUS_FILLED, AsOf: timestamppb.Now(),
	}
	fill := &orderpb.OrderFilled{OrderId: "o1", State: st, Fill: &orderpb.Fill{
		FillId: "f1", OrderId: "o1", InstrumentId: "BTC", Side: orderpb.Side_SIDE_BUY,
		Quantity: d(2), Price: d(100), ExecutedAt: timestamppb.Now(),
	}}
	fb, _ := proto.Marshal(fill)
	_ = proj.Handle(context.Background(), &envelopepb.Envelope{TenantId: "acme", EventType: "order.order.filled"}, fb)

	// Mark moves to 150 → unrealized = 2 × (150 − 100) = 100.
	//
	// EventType must be a real market.<assetClass>.trade — mark.Source now
	// guards on it before unmarshalling (isMarkBearingEventType in mark.go,
	// added to close the market.book.snapshot poisoning bug), and
	// bussink.go never publishes a MarketDataEvent with an empty EventType,
	// so this fixture is realistic rather than weakened.
	_ = m.Handle(context.Background(), &envelopepb.Envelope{EventType: "market.crypto.trade"}, tradeEvent(t, "BTC", 150))

	stDTO, ok := proj.State("acme", "fund-alpha", time.Time{})
	if !ok || stDTO.UnrealizedPnl != "100" {
		t.Fatalf("unrealized = %q, want 100 (dynamic from live mark)", stDTO.UnrealizedPnl)
	}
}
