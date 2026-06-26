// Package execution is the OMS's EMS layer (OMS-01c): a smart-order-router that
// selects a Venue and works an order, producing Fills. Venue is a seam — the
// SimVenue here fills marketable orders deterministically so the whole submit→
// fill path runs with no external dependency; a real FIX/venue adapter
// implements the same interface and is wired at the composition root (the
// DEBT-02 inject-the-side-effect stance), so the router and command handler
// never change.
package execution

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kanz-eng/kanz/services/oms/internal/dec"
)

// Venue works an order and returns the fills it produced. The MIC identifies
// the venue on the resulting FACTs.
type Venue interface {
	// MIC is the ISO 10383 venue code stamped on routing + fills.
	MIC() string
	// Execute works st and returns zero or more fills (each strictly within the
	// order's open quantity). Returning no fills is valid — a resting order that
	// did not trade — and leaves the order working.
	Execute(ctx context.Context, st *orderpb.OrderState) ([]*orderpb.Fill, error)
}

// PriceFunc resolves the execution price for an order. SimVenue uses it for
// MARKET orders (which carry no limit); priced orders fill at their limit. A
// nil result means "no price available" — the order does not fill.
type PriceFunc func(st *orderpb.OrderState) *commonpb.Decimal

// SimVenue is the in-process simulation venue. It fills a marketable order in
// full, in one fill, at the order's limit price (LIMIT/STOP_LIMIT) or the
// resolved mark price (MARKET). Deterministic given its clock + id generator.
type SimVenue struct {
	mic   string
	price PriceFunc
	now   func() time.Time
	newID func() string
}

// SimOption customizes a SimVenue.
type SimOption func(*SimVenue)

// WithPrice sets the MARKET-order price resolver.
func WithPrice(p PriceFunc) SimOption { return func(v *SimVenue) { v.price = p } }

// WithClock overrides the fill timestamp source (tests).
func WithClock(now func() time.Time) SimOption { return func(v *SimVenue) { v.now = now } }

// WithIDGen overrides the fill_id generator (tests).
func WithIDGen(f func() string) SimOption { return func(v *SimVenue) { v.newID = f } }

// NewSimVenue returns a simulation venue with the given MIC.
func NewSimVenue(mic string, opts ...SimOption) *SimVenue {
	v := &SimVenue{mic: mic, now: time.Now, newID: uuid.NewString}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

// MIC returns the venue code.
func (v *SimVenue) MIC() string { return v.mic }

// Execute fills the open quantity in full at the resolved price.
func (v *SimVenue) Execute(_ context.Context, st *orderpb.OrderState) ([]*orderpb.Fill, error) {
	if st == nil {
		return nil, errors.New("execution: nil order state")
	}
	price := v.executionPrice(st)
	if price == nil || dec.IsZero(price) {
		return nil, nil // no price ⇒ no fill; order rests
	}
	fill := &orderpb.Fill{
		FillId:       v.newID(),
		OrderId:      st.GetOrderId(),
		InstrumentId: st.GetInstrumentId(),
		Side:         st.GetSide(),
		Quantity:     st.GetLeavesQuantity(),
		Price:        price,
		Venue:        v.mic,
		ExecutedAt:   timestamppb.New(v.now().UTC()),
	}
	return []*orderpb.Fill{fill}, nil
}

func (v *SimVenue) executionPrice(st *orderpb.OrderState) *commonpb.Decimal {
	switch st.GetOrderType() {
	case orderpb.OrderType_ORDER_TYPE_LIMIT, orderpb.OrderType_ORDER_TYPE_STOP_LIMIT:
		return st.GetLimitPrice()
	default:
		if v.price != nil {
			return v.price(st)
		}
		return nil
	}
}
