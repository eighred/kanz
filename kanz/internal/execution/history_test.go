package execution

import (
	"errors"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func historyFixture() (*orderpb.OrderState, OrderView) {
	d := func(s string) *commonpb.Decimal { v, _ := ParseDec(s); return v }
	st := &orderpb.OrderState{OrderId: "order", PortfolioId: "fund", InstrumentId: "BTC-USD", Venue: "BINANCE", VenueAccountId: "account", Side: orderpb.Side_SIDE_BUY, OrderedQuantity: d("2"), Status: orderpb.OrderStatus_ORDER_STATUS_CANCELLED}
	f := &orderpb.Fill{FillId: "fill-1", VenueExecutionId: "1", OrderId: st.OrderId, InstrumentId: st.InstrumentId, Venue: st.Venue, VenueAccountId: st.VenueAccountId, Side: st.Side, Quantity: d("0.5"), Price: d("100"), Fee: &commonpb.Money{Amount: d("0.01"), CurrencyCode: "USD"}, ExecutedAt: timestamppb.New(time.Unix(100, 0))}
	g := proto.Clone(f).(*orderpb.Fill)
	g.FillId, g.VenueExecutionId = "fill-2", "2"
	g.ExecutedAt = timestamppb.New(time.Unix(200, 0))
	return st, OrderView{State: OrderViewCancelled, ExecutedQuantity: d("1"), Fills: []*orderpb.Fill{g, f, proto.Clone(f).(*orderpb.Fill)}}
}

func TestCompleteHistoryDetachesSortsAndDeduplicatesWithoutReopening(t *testing.T) {
	st, view := historyFixture()
	fills, err := CompleteHistory(st, view)
	if err != nil || len(fills) != 2 || fills[0].FillId != "fill-1" || fills[1].FillId != "fill-2" {
		t.Fatalf("fills=%v err=%v", fills, err)
	}
	fills[0].Price.Coefficient = 99
	if view.Fills[1].Price.Coefficient == 99 || st.Status != orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatal("history mutated original evidence or reopened terminal order")
	}
}

func TestCompleteHistoryRefusesIncompleteOrConflictingEconomics(t *testing.T) {
	cases := map[string]func(*orderpb.OrderState, *OrderView){
		"unknown total":              func(_ *orderpb.OrderState, v *OrderView) { v.ExecutedQuantity = nil },
		"partial page":               func(_ *orderpb.OrderState, v *OrderView) { v.Fills = v.Fills[:1] },
		"overfill":                   func(_ *orderpb.OrderState, v *OrderView) { v.ExecutedQuantity, _ = ParseDec("3") },
		"contradictory status":       func(_ *orderpb.OrderState, v *OrderView) { v.State = OrderViewWorking },
		"unproven state":             func(_ *orderpb.OrderState, v *OrderView) { v.State = OrderViewUnknown },
		"another account":            func(_ *orderpb.OrderState, v *OrderView) { v.Fills[0].VenueAccountId = "other" },
		"another order":              func(_ *orderpb.OrderState, v *OrderView) { v.Fills[0].OrderId = "other" },
		"another instrument":         func(_ *orderpb.OrderState, v *OrderView) { v.Fills[0].InstrumentId = "other" },
		"side mismatch":              func(_ *orderpb.OrderState, v *OrderView) { v.Fills[0].Side = orderpb.Side_SIDE_SELL },
		"missing execution id":       func(_ *orderpb.OrderState, v *OrderView) { v.Fills[0].VenueExecutionId = "" },
		"missing fee":                func(_ *orderpb.OrderState, v *OrderView) { v.Fills[0].Fee = nil },
		"missing time":               func(_ *orderpb.OrderState, v *OrderView) { v.Fills[0].ExecutedAt = nil },
		"conflicting fee":            func(_ *orderpb.OrderState, v *OrderView) { v.Fills[2].Fee.Amount, _ = ParseDec("0.02") },
		"fill id collision":          func(_ *orderpb.OrderState, v *OrderView) { v.Fills[0].FillId = v.Fills[1].FillId },
		"hostile exponent":           func(_ *orderpb.OrderState, v *OrderView) { v.Fills[0].Quantity.Exponent = 2000000000 },
		"booked total exceeds venue": func(s *orderpb.OrderState, _ *OrderView) { s.FilledQuantity, _ = ParseDec("1.5") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			st, view := historyFixture()
			mutate(st, &view)
			fills, err := CompleteHistory(st, view)
			if !errors.Is(err, ErrHistoryIncomplete) || fills != nil {
				t.Fatalf("partial economics escaped: %v %v", fills, err)
			}
		})
	}
}

func TestWithExecutionTotalDoesNotPreserveStaleEvidence(t *testing.T) {
	for _, raw := range []string{"", "invalid", "-1"} {
		_, view := historyFixture()
		if WithExecutionTotal(view, raw).ExecutedQuantity != nil {
			t.Fatalf("%q retained stale total", raw)
		}
	}
	if WithExecutionTotal(OrderView{}, "0").ExecutedQuantity == nil {
		t.Fatal("known zero became unknown")
	}
}
