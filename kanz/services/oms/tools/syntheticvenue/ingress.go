package syntheticvenue

import (
	"context"
	"errors"
	"time"

	"github.com/eighred/kanz/internal/dec"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/pkg/bus"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Command and Receipt are the adapter's narrow side of the Rust OMS process
// boundary. JSON encodes protobuf byte slices as base64.
type Command struct {
	Op               string `json:"op"`
	Tenant           string `json:"tenant"`
	Portfolio        string `json:"portfolio"`
	OrderID          string `json:"order_id"`
	Principal        string `json:"principal"`
	EventID          string `json:"event_id"`
	EffectiveSeconds int64  `json:"effective_seconds"`
	EffectiveNanos   int32  `json:"effective_nanos"`
	VenueOrderID     string `json:"venue_order_id,omitempty"`
	Fill             []byte `json:"fill_b64,omitempty"`
	ExecutedQuantity []byte `json:"executed_quantity_b64,omitempty"`
}
type Receipt struct {
	OK           bool   `json:"ok"`
	Code         string `json:"code"`
	Reason       string `json:"reason,omitempty"`
	State        []byte `json:"state_b64"`
	OrderVersion uint64 `json:"order_version"`
}
type Owner func(context.Context, Command) (Receipt, error)
type Ingress struct {
	tenant, portfolio string
	venue             venuepb.VenueAdapterServiceClient
	owner             Owner
	workingPartial    bool
}

// NewWorkingPartialIngress models XSIM's independently confirmed execution
// before withdrawal. Its terminal state carries the signed withdrawal's
// effective time, distinct from the earlier fill's venue timestamp.
func NewWorkingPartialIngress(tenant, portfolio string, venue venuepb.VenueAdapterServiceClient, owner Owner) (*Ingress, error) {
	i, err := NewIngress(tenant, portfolio, venue, owner)
	if err != nil {
		return nil, err
	}
	i.workingPartial = true
	return i, nil
}

const AcceptedSubject = "order.order.accepted"

func NewIngress(tenant, portfolio string, venue venuepb.VenueAdapterServiceClient, owner Owner) (*Ingress, error) {
	if tenant == "" || portfolio == "" || venue == nil || owner == nil {
		return nil, errors.New("synthetic ingress requires scope, venue and OMS owner")
	}
	return &Ingress{tenant: tenant, portfolio: portfolio, venue: venue, owner: owner}, nil
}
func (i *Ingress) Subscribe(ctx context.Context, consumer *bus.Consumer, group string, ready func()) error {
	return consumer.SubscribeReady(ctx, AcceptedSubject, group, i.HandleAccepted, ready)
}

func (i *Ingress) HandleAccepted(ctx context.Context, e *envelopepb.Envelope, payload []byte) error {
	var event orderpb.OrderAccepted
	if e.GetEventType() != AcceptedSubject || e.GetSource() != "oms" || e.GetDomain() != "order" || e.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT ||
		e.GetSchemaVersion() != 1 || e.GetTenantId() != i.tenant || e.GetEventId() == "" || e.GetEventTime().CheckValid() != nil ||
		proto.Unmarshal(payload, &event) != nil || event.OrderId == "" || event.OrderId != e.GetPartitionKey() ||
		event.OrderId != event.State.GetOrderId() || event.State.GetPortfolioId() != i.portfolio {
		return bus.Terminal(errors.New("invalid synthetic accepted fact"))
	}
	// The event identifies an admitted order. Only the OMS owner's persisted
	// state supplies the terms for a venue call.
	return i.route(ctx, i.command("dispatch", event.OrderId, e.EventId, e.EventTime.AsTime()))
}

// Cancel is called by the isolated fixture orchestrator after authorization.
// It has no command subject, HTTP handler or caller-supplied principal.
func (i *Ingress) Cancel(ctx context.Context, orderID, eventID string, effective time.Time) error {
	if orderID == "" || eventID == "" || effective.IsZero() || timestamppb.New(effective).CheckValid() != nil {
		return errors.New("invalid synthetic cancellation")
	}
	return i.route(ctx, i.command("cancel_pending", orderID, eventID, effective))
}
func (i *Ingress) command(op, orderID, eventID string, effective time.Time) Command {
	return Command{Op: op, Tenant: i.tenant, Portfolio: i.portfolio, OrderID: orderID, Principal: OMSIdentity, EventID: eventID,
		EffectiveSeconds: effective.Unix(), EffectiveNanos: int32(effective.Nanosecond())}
}
func (i *Ingress) route(ctx context.Context, c Command) error {
	claim, err := i.owner(ctx, c)
	if err != nil || !claim.OK {
		return errors.New("synthetic OMS claim refused")
	}
	var st orderpb.OrderState
	if len(claim.State) == 0 || proto.Unmarshal(claim.State, &st) != nil || st.OrderId != c.OrderID || st.PortfolioId != i.portfolio ||
		st.Venue != MIC || st.VenueAccountId != Account || st.InstrumentId != Instrument {
		return errors.New("invalid synthetic OMS claim state")
	}
	if _, err := terms(&st); err != nil {
		return err
	}
	switch {
	case claim.Code == "DISPATCH_ONCE" && c.Op == "dispatch":
		reply, err := i.venue.Execute(ctx, &venuepb.ExecuteRequest{TenantId: i.tenant, State: &st})
		if err != nil {
			return i.uncertain(ctx, c)
		}
		if reply == nil || len(reply.Fills) != 0 {
			return errors.New("unexpected synthetic execution response")
		}
		if !i.workingPartial {
			return i.observeAck(ctx, c)
		}
	case claim.Code == "CANCEL_ONCE" && c.Op == "cancel_pending":
		_, err := i.venue.CancelOrder(ctx, &venuepb.CancelOrderRequest{TenantId: i.tenant, State: &st})
		if err != nil {
			return i.uncertain(ctx, c)
		}
	case claim.Code == "RECONCILE_REQUIRED":
		// A claim can survive the process that made it. Query the same remote
		// ID even when the first sender may have died before sending.
	default:
		return errors.New("unexpected OMS route claim")
	}
	view, err := i.venue.QueryOrder(ctx, &venuepb.QueryOrderRequest{TenantId: i.tenant, State: &st})
	if err != nil {
		return i.uncertain(ctx, c)
	}
	switch view.GetState() {
	case venuepb.OrderViewState_ORDER_VIEW_STATE_WORKING:
		if c.Op != "dispatch" {
			return i.uncertain(ctx, c)
		}
		if len(view.Fills) != 0 || view.ExecutedQuantity == nil || !dec.InDomain(view.ExecutedQuantity) || view.ExecutedQuantity.Coefficient != 0 {
			return errors.New("synthetic venue outcome remains unresolved")
		}
		return i.observeAck(ctx, c)
	case venuepb.OrderViewState_ORDER_VIEW_STATE_PARTIALLY_FILLED, venuepb.OrderViewState_ORDER_VIEW_STATE_CANCELLED:
		partial := view.State == venuepb.OrderViewState_ORDER_VIEW_STATE_PARTIALLY_FILLED
		if partial && (!i.workingPartial || c.Op != "dispatch") {
			return i.uncertain(ctx, c)
		}
		if view.ExecutedQuantity == nil || !dec.InDomain(view.ExecutedQuantity) || !dec.IsPositive(view.ExecutedQuantity) || len(view.Fills) != 1 {
			return errors.New("synthetic cancellation lacks complete venue history")
		}
		fill := view.Fills[0]
		if fill.GetFillId() == "" || fill.GetVenueExecutionId() != fill.FillId || fill.GetOrderId() != st.OrderId ||
			fill.GetVenue() != MIC || fill.GetVenueAccountId() != Account || fill.GetInstrumentId() != Instrument || fill.GetSide() != st.Side ||
			fill.GetExecutedAt().CheckValid() != nil || !proto.Equal(fill.Quantity, view.ExecutedQuantity) {
			return errors.New("synthetic execution scope or evidence mismatch")
		}
		wire, err := proto.Marshal(fill)
		if err != nil {
			return err
		}
		if partial {
			if err := i.observeAck(ctx, c); err != nil {
				return err
			}
		}
		obs := i.command("fill", st.OrderId, fill.FillId, fill.ExecutedAt.AsTime())
		obs.Fill = wire
		if err := i.observe(ctx, obs); err != nil {
			return err
		}
		if partial {
			return nil
		}
		qty, err := proto.Marshal(view.ExecutedQuantity)
		if err != nil {
			return err
		}
		effective := fill.ExecutedAt.AsTime()
		if i.workingPartial {
			if st.Status == orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
				effective = st.GetAsOf().AsTime()
			} else if c.Op == "cancel_pending" {
				effective = time.Unix(c.EffectiveSeconds, int64(c.EffectiveNanos))
			} else {
				return errors.New("confirmed funded withdrawal requires its signed cancel delivery")
			}
		}
		cancelled := i.command("cancelled", st.OrderId, fill.FillId+"/cancelled", effective)
		cancelled.ExecutedQuantity = qty
		return i.observe(ctx, cancelled)
	default:
		return i.uncertain(ctx, c)
	}
}
func (i *Ingress) observeAck(ctx context.Context, c Command) error {
	c.Op = "ack"
	c.EventID = sourceID("xsim-ack", c.Tenant, c.Portfolio, c.OrderID, c.EventID)
	c.VenueOrderID = c.OrderID
	return i.observe(ctx, c)
}
func (i *Ingress) uncertain(ctx context.Context, c Command) error {
	c.Op = "timeout"
	c.EventID = sourceID("xsim-timeout", c.Tenant, c.Portfolio, c.OrderID, c.EventID)
	if err := i.observe(ctx, c); err != nil {
		return err
	}
	return errors.New("synthetic acknowledgement unavailable; reconciliation required")
}
func (i *Ingress) observe(ctx context.Context, c Command) error {
	r, err := i.owner(ctx, c)
	if err != nil || !r.OK {
		return errors.New("synthetic OMS observation refused")
	}
	return nil
}
