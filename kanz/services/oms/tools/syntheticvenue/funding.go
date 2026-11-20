package syntheticvenue

import (
	"time"

	"github.com/eighred/kanz/internal/dec"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/services/oms/internal/capital"
	"github.com/eighred/kanz/services/oms/internal/order"
)

// PhysicalFunding computes the debit vector for the fixture's physically
// settled SYN/USD spot contract using the OMS capital owner and supplied fees.
func PhysicalFunding(cmd *orderpb.SubmitOrder, fees []*commonpb.Money) ([]string, []string, error) {
	if cmd.GetInstrumentId() != "SYNTH-USD" || cmd.GetVenue() != "XSIM" {
		return nil, nil, capital.ErrInvalid
	}
	debits, err := capital.PhysicalDebits(&orderpb.OrderState{Side: cmd.Side, OrderType: cmd.OrderType,
		OrderedQuantity: cmd.Quantity, LimitPrice: cmd.LimitPrice, MarginMode: cmd.MarginMode, Leverage: cmd.Leverage}, "SYN", "USD", fees)
	if err != nil {
		return nil, nil, err
	}
	var currencies, amounts []string
	for _, debit := range debits {
		currencies = append(currencies, debit.CurrencyCode)
		amounts = append(amounts, dec.FromProto(debit.Amount).RatString())
	}
	return currencies, amounts, nil
}

// ProjectWithdrawal derives the independently authorized state and FACT from
// the existing OMS aggregate at the gateway event's database timestamp.
func ProjectWithdrawal(state *orderpb.OrderState, at time.Time) (*orderpb.OrderState, *orderpb.OrderCancelled, error) {
	next, leaves, err := order.Cancel(state, at.Truncate(time.Microsecond))
	if err != nil {
		return nil, nil, err
	}
	return next, &orderpb.OrderCancelled{OrderId: state.OrderId, CancelledQuantity: leaves}, nil
}
