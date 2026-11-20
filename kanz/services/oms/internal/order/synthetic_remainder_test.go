//go:build synthetic_handoff

package order

import (
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/execution"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestSyntheticHandoffRemainder(t *testing.T) {
	when := time.Date(2026, 11, 20, 21, 12, 54, 321, time.UTC)
	half := &commonpb.Decimal{Coefficient: 625, Exponent: -3}
	full := &commonpb.Decimal{Coefficient: 125, Exponent: -2}
	price := &commonpb.Decimal{Coefficient: 200}
	firstID := "xsim-partial/" + strings.Repeat("1", 64)
	secondID := "xsim-remainder/" + strings.Repeat("2", 64)
	first := &orderpb.Fill{
		FillId: firstID, VenueExecutionId: firstID, OrderId: "order",
		InstrumentId: "SYNTH-USD", Venue: "XSIM", VenueAccountId: "synthetic-spot", Side: orderpb.Side_SIDE_BUY,
		Quantity: half, Price: price, Fee: &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: 1}},
		ExecutedAt: timestamppb.New(when),
	}
	second := proto.Clone(first).(*orderpb.Fill)
	second.FillId, second.VenueExecutionId, second.ExecutedAt = secondID, secondID, timestamppb.New(when.Add(time.Nanosecond))
	partial := &orderpb.OrderState{
		OrderId: first.OrderId, PortfolioId: "portfolio", InstrumentId: first.InstrumentId, Venue: first.Venue,
		VenueAccountId: first.VenueAccountId, Side: first.Side, Status: orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED,
		AsOf: first.ExecutedAt, VenueAckAt: timestamppb.New(when.Add(-time.Second)), OrderedQuantity: full,
		FilledQuantity: half, LeavesQuantity: half, LimitPrice: price, AverageFillPrice: price,
	}
	view := execution.OrderView{State: execution.OrderViewFilled, ExecutedQuantity: full, Fills: []*orderpb.Fill{first, second}}
	validate := func(st *orderpb.OrderState) {
		t.Helper()
		got, err := SyntheticHandoffRemainder(st, first, view)
		if err != nil || !proto.Equal(got, second) {
			t.Fatalf("typed history did not return the remainder: %v", err)
		}
	}
	validate(partial)
	terminal, err := ApplyFill(partial, second, second.ExecutedAt.AsTime())
	if err != nil {
		t.Fatal(err)
	}
	validate(terminal)

	for _, tc := range []struct {
		name string
		edit func(*execution.OrderView)
	}{
		{"missing retained fill", func(v *execution.OrderView) { v.Fills = v.Fills[1:] }},
		{"changed retained fee", func(v *execution.OrderView) { v.Fills[0].Fee.Amount.Coefficient++ }},
		{"reused execution ID", func(v *execution.OrderView) { v.Fills[1].VenueExecutionId = v.Fills[0].VenueExecutionId }},
		{"same execution time", func(v *execution.OrderView) { v.Fills[1].ExecutedAt = v.Fills[0].ExecutedAt }},
		{"overfilled", func(v *execution.OrderView) { v.Fills[1].Quantity.Coefficient++ }},
		{"unknown total", func(v *execution.OrderView) { v.ExecutedQuantity = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := execution.OrderView{State: view.State, ExecutedQuantity: full,
				Fills: []*orderpb.Fill{proto.Clone(first).(*orderpb.Fill), proto.Clone(second).(*orderpb.Fill)}}
			tc.edit(&candidate)
			if _, err := SyntheticHandoffRemainder(partial, first, candidate); err == nil {
				t.Fatal("accepted an observation outside the qualified XSIM remainder")
			}
		})
	}
}
