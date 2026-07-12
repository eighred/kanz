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

	"github.com/kanz-eng/kanz/internal/dec"
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

// Closer is the optional Venue capability to withdraw a working order AT the
// exchange. It is separate from Venue because not every venue has an order
// resting externally to withdraw — SimVenue fills or rests in-process, so
// cancelling it is purely a ledger operation. The OMS type-asserts: a venue that
// implements Closer gets a real venue-side cancel dispatched before the ledger
// records the cancellation; one that does not is a ledger-only cancel.
//
// CancelOrder addresses the order by st.order_id — our deterministic clOrdId /
// origClientOrderId — the same identity the submit used, so a cancel is
// idempotent at the venue. A nil error means the venue CONFIRMED the withdrawal;
// any error (including an ambiguous timeout) leaves the close in flight and is
// the healing watchdog's to resolve — never assume a cancel landed.
type Closer interface {
	CancelOrder(ctx context.Context, st *orderpb.OrderState) error
}

// SelfHealing marks a Venue that owns its OWN in-flight-close seam — it tracks a
// dispatched close and heals it internally, so the OMS must NOT also track it.
//
// This exists because of the process split (INFRA-M7a). An out-of-process adapter
// runs its own reconciler and its own close registry. If the OMS ALSO tracked the
// close, its entry would never be resolved by anyone — and the in-process OKX
// reconciler drains the shared registry indiscriminately. For an instrument that
// trades on both venues (BTC-USD does), OKX would query ITSELF for a Binance order
// id, fail to find it, and "heal" an order that was never its own.
//
// A venue that heals itself says so, and the OMS keeps its hands off.
type SelfHealing interface {
	// OwnsCloseTracking is a marker: this venue tracks and heals its own closes.
	OwnsCloseTracking()
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
