//go:build synthetic_handoff

package syntheticvenue

import (
	"errors"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/services/oms/internal/capital"
	"github.com/eighred/kanz/services/oms/internal/order"
	"google.golang.org/protobuf/proto"
)

// ProjectRemainder binds the independently observed XSIM execution to the
// exact next aggregate, FACT and incremental debit for the settlement permit.
func ProjectRemainder(prior *orderpb.OrderState, retained *orderpb.Fill, view *venuepb.QueryOrderResponse, currencies []string) (SettlementProjection, error) {
	var p SettlementProjection
	if prior.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED ||
		view.GetState() != venuepb.OrderViewState_ORDER_VIEW_STATE_FILLED {
		return p, errors.New("synthetic remainder requires a working partial order and its complete XSIM history")
	}
	fill, err := order.SyntheticHandoffRemainder(prior, retained, execution.OrderView{
		State: execution.OrderViewFilled, ExecutedQuantity: view.GetExecutedQuantity(), Fills: view.GetFills(),
	})
	if err != nil {
		return p, err
	}
	p.Terms, err = terms(prior)
	if err != nil {
		return p, err
	}
	p.Body, err = proto.MarshalOptions{Deterministic: true}.Marshal(fill)
	if err != nil {
		return p, err
	}
	p.Next, err = order.ApplyFill(prior, fill, fill.GetExecutedAt().AsTime())
	if err != nil {
		return p, err
	}
	p.Fact = &orderpb.OrderFilled{OrderId: prior.GetOrderId(), Fill: fill, State: p.Next}

	executed := proto.Clone(prior).(*orderpb.OrderState)
	executed.OrderedQuantity, executed.LimitPrice = fill.GetQuantity(), fill.GetPrice()
	debits, err := capital.PhysicalDebits(executed, "SYN", "USD", []*commonpb.Money{fill.GetFee()})
	if err != nil {
		return p, err
	}
	if len(currencies) != 2 || currencies[0] != "BNB" || currencies[1] != "USD" ||
		len(debits) != 1 || debits[0].GetCurrencyCode() != "USD" {
		return p, errors.New("synthetic remainder requires the retained USD and BNB funding legs")
	}
	p.Currencies = append([]string(nil), currencies...)
	p.ExecutedDebits = []string{"0", dec.FromProto(debits[0].GetAmount()).RatString()}
	return p, nil
}
