package syntheticvenue

import (
	"bytes"
	"errors"
	"time"

	"github.com/eighred/kanz/internal/dec"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/services/oms/internal/capital"
	"github.com/eighred/kanz/services/oms/internal/order"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SettlementProjection contains only the transition authorized by an isolated
// XSIM observation. OMS retains ownership of the order, its FACT and its cash.
type SettlementProjection struct {
	Terms, Body                []byte
	Next                       *orderpb.OrderState
	Fact                       proto.Message
	Currencies, ExecutedDebits []string
}

func ProjectSettlement(prior *orderpb.OrderState, c Command, view *venuepb.QueryOrderResponse, currencies []string) (SettlementProjection, error) {
	var p SettlementProjection
	refused := errors.New("synthetic settlement evidence does not match the scoped order")
	if prior.GetOrderId() != c.OrderID || prior.GetPortfolioId() != c.Portfolio || prior.GetVenue() != MIC || prior.GetVenueAccountId() != Account || prior.GetInstrumentId() != Instrument || c.Principal != OMSIdentity {
		return p, refused
	}
	var err error
	p.Terms, err = terms(prior)
	if err != nil {
		return p, err
	}
	at := time.Unix(c.EffectiveSeconds, int64(c.EffectiveNanos)).UTC()
	switch c.Op {
	case "dispatch", "cancel_pending":
		return p, nil
	case "timeout":
		if prior.Quarantine == nil {
			p.Next = proto.Clone(prior).(*orderpb.OrderState)
			p.Next.Quarantine = &orderpb.OrderQuarantine{At: timestamppb.New(at), Reason: "synthetic venue outcome unknown"}
		}
		return p, nil
	case "ack":
		if c.VenueOrderID != c.OrderID || view.GetExecutedQuantity() == nil || !dec.InDomain(view.ExecutedQuantity) {
			return p, refused
		}
		switch view.GetState() {
		case venuepb.OrderViewState_ORDER_VIEW_STATE_WORKING:
			if view.ExecutedQuantity.Coefficient != 0 || len(view.Fills) != 0 {
				return p, refused
			}
		case venuepb.OrderViewState_ORDER_VIEW_STATE_PARTIALLY_FILLED:
			if !dec.IsPositive(view.ExecutedQuantity) || len(view.Fills) != 1 {
				return p, refused
			}
			fill := view.Fills[0]
			if fill.GetOrderId() != c.OrderID || fill.GetInstrumentId() != Instrument || fill.GetVenue() != MIC || fill.GetVenueAccountId() != Account || fill.GetSide() != prior.Side || fill.GetFillId() == "" || fill.GetFillId() != fill.GetVenueExecutionId() || !proto.Equal(fill.GetQuantity(), view.ExecutedQuantity) {
				return p, refused
			}
		default:
			return p, refused
		}
		p.Body = []byte(c.VenueOrderID)
		p.Next, err = order.Route(prior, at)
		if err != nil {
			return p, err
		}
		p.Next.VenueAckAt = timestamppb.New(at)
		p.Next.Quarantine = nil
		p.Fact = &orderpb.OrderRouted{OrderId: c.OrderID, Venue: MIC, VenueOrderId: c.VenueOrderID}
		return p, nil
	case "fill", "cancelled":
	default:
		return p, refused
	}
	// A terminal query can be the first observation of a real execution.
	// Account for the verified fill before releasing the remaining order.
	cancelled := view.GetState() == venuepb.OrderViewState_ORDER_VIEW_STATE_CANCELLED
	workingFill := c.Op == "fill" && view.GetState() == venuepb.OrderViewState_ORDER_VIEW_STATE_PARTIALLY_FILLED
	if (!cancelled && !workingFill) || len(view.GetFills()) != 1 {
		return p, refused
	}
	fill := view.Fills[0]
	if fill.GetOrderId() != c.OrderID || fill.GetInstrumentId() != Instrument || fill.GetSide() != prior.Side || fill.GetVenue() != MIC || fill.GetVenueAccountId() != Account || fill.GetFillId() == "" || fill.GetFillId() != fill.GetVenueExecutionId() ||
		fill.GetExecutedAt().CheckValid() != nil || fill.ExecutedAt.AsTime().After(at) || !proto.Equal(view.GetExecutedQuantity(), fill.Quantity) || !dec.IsPositive(fill.Price) || !dec.IsPositive(fill.Quantity) {
		return p, refused
	}
	price := dec.FromProto(fill.Price).Cmp(dec.FromProto(prior.LimitPrice))
	if (prior.Side == orderpb.Side_SIDE_BUY && price > 0) || (prior.Side == orderpb.Side_SIDE_SELL && price < 0) {
		return p, refused
	}
	wire := proto.MarshalOptions{Deterministic: true}
	if c.Op == "fill" {
		p.Body, err = wire.Marshal(fill)
		if err != nil || !bytes.Equal(c.Fill, p.Body) || c.EventID != fill.FillId || !fill.ExecutedAt.AsTime().Equal(at) {
			return p, refused
		}
		p.Next, err = order.ApplyFill(prior, fill, at)
		if err != nil {
			return p, err
		}
		if p.Next.Status != orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED {
			return p, refused
		}
		p.Fact = &orderpb.OrderPartiallyFilled{OrderId: c.OrderID, Fill: fill, State: p.Next}
	} else {
		p.Body, err = wire.Marshal(view.ExecutedQuantity)
		if err != nil || !bytes.Equal(c.ExecutedQuantity, p.Body) || c.EventID != fill.FillId+"/cancelled" || dec.FromProto(prior.FilledQuantity).Cmp(dec.FromProto(fill.Quantity)) != 0 {
			return p, refused
		}
		next, leaves, e := order.Cancel(prior, at)
		if e != nil {
			return p, e
		}
		next.Quarantine = nil
		p.Next = next
		p.Fact = &orderpb.OrderCancelled{OrderId: c.OrderID, CancelledQuantity: leaves}
	}
	// The sole supported receipt is for one partial physical fill. Calculate
	// the execution debit at its reported price, including its reported fee.
	executed := proto.Clone(prior).(*orderpb.OrderState)
	executed.OrderedQuantity, executed.LimitPrice = fill.Quantity, fill.Price
	if fill.Fee == nil || fill.Fee.CurrencyCode != "USD" || fill.Fee.Amount == nil {
		return p, refused
	}
	debits, err := capital.PhysicalDebits(executed, "SYN", "USD", []*commonpb.Money{fill.Fee})
	if err != nil {
		return p, err
	}
	actual := make(map[string]string, len(debits))
	for _, debit := range debits {
		actual[debit.CurrencyCode] = dec.FromProto(debit.Amount).RatString()
	}
	for _, currency := range currencies {
		value, ok := actual[currency]
		if !ok {
			value = "0"
		}
		p.Currencies = append(p.Currencies, currency)
		p.ExecutedDebits = append(p.ExecutedDebits, value)
		delete(actual, currency)
	}
	if len(actual) != 0 {
		return p, refused
	}
	return p, nil
}
