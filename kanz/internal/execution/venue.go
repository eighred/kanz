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
	"fmt"
	"sync"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dec"
)

// Venue works an order and returns the fills it produced. The MIC identifies
// the venue on the resulting FACTs.
type Venue interface {
	// MIC is the ISO 10383 venue code stamped on routing + fills.
	MIC() string
	// Account is the EXCHANGE ACCOUNT this venue trades — the sub-account behind
	// the API credential it holds. It is the collateral boundary: whatever this
	// venue fills is margined, netted and liquidated against this account, whoever
	// the order was for. One adapter deployment holds one credential and is
	// therefore exactly one account.
	Account() string
	// Execute works st and returns zero or more fills (each strictly within the
	// order's open quantity). Returning no fills is valid — a resting order that
	// did not trade — and leaves the order working.
	Execute(ctx context.Context, st *orderpb.OrderState) ([]*orderpb.Fill, error)
}

// AccountProof is the EXCHANGE's own confirmation that an adapter's API credential
// belongs to the account the adapter claims to be (SOV-02a).
//
// Account() alone is a CLAIM: an adapter reads its account from its own config, so
// two mis-configured deployments agree with each other perfectly while the exchange
// debits a third account entirely. The only authority on which account a key belongs
// to is the venue that issued the key, so the adapter asks it at startup and carries
// the answer here.
//
// The ZERO VALUE IS UNVERIFIED, deliberately. An adapter that never checks reports
// the safe answer rather than the flattering one, and the OMS can see the difference
// between "the exchange confirmed this" and "nobody has ever checked" — which were
// previously the same observable state.
type AccountProof struct {
	// Verified is true only when the exchange itself confirmed the credential
	// belongs to the claimed account.
	Verified bool
	// ExchangeAccountID is the exchange's OWN id for the account behind the
	// credential (a Binance/OKX uid) — what the platform's account label is a label
	// FOR. Empty when nothing was verified.
	ExchangeAccountID string
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
//
// IT REMEMBERS WHAT IT EXECUTED, and that is not a convenience. A real exchange
// dedups a resubmitted clOrdId and can be asked what it did with an order; a
// simulator that does neither cannot stand in for one on the crash-recovery
// path, which is precisely the path we use it to prove. Without the record,
// re-driving an order after a crash mints a SECOND fill_id for the same
// execution — and position_fills, which dedups on fill_id, folds it twice.
type SimVenue struct {
	mic     string
	account string
	price   PriceFunc
	now     func() time.Time
	newID   func() string

	// mu guards executed. SimVenue is shared by every goroutine handling orders
	// for this MIC, so the record is concurrent by construction.
	mu sync.Mutex
	// executed maps order_id → the fills this venue reported for it. Unbounded
	// by design: it is a simulator, its lifetime is a process, and forgetting an
	// order would resurrect the exact bug this record exists to close.
	executed map[string][]*orderpb.Fill
}

// SimOption customizes a SimVenue.
type SimOption func(*SimVenue)

// WithPrice sets the MARKET-order price resolver.
func WithPrice(p PriceFunc) SimOption { return func(v *SimVenue) { v.price = p } }

// WithAccount names the exchange account this simulator stands in for. Without it
// the account is "sim:<MIC>" — deliberately not a plausible account id, because a
// simulated fill must never be mistaken for collateral that moved somewhere real.
func WithAccount(a string) SimOption { return func(v *SimVenue) { v.account = a } }

// WithClock overrides the fill timestamp source (tests).
func WithClock(now func() time.Time) SimOption { return func(v *SimVenue) { v.now = now } }

// WithIDGen overrides the fill_id generator (tests).
func WithIDGen(f func() string) SimOption { return func(v *SimVenue) { v.newID = f } }

// NewSimVenue returns a simulation venue with the given MIC.
func NewSimVenue(mic string, opts ...SimOption) *SimVenue {
	v := &SimVenue{
		mic:      mic,
		account:  "sim:" + mic,
		now:      time.Now,
		newID:    uuid.NewString,
		executed: make(map[string][]*orderpb.Fill),
	}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

// MIC returns the venue code.
func (v *SimVenue) MIC() string { return v.mic }

// Account returns the simulated exchange account.
func (v *SimVenue) Account() string { return v.account }

// Execute fills the open quantity in full at the resolved price. A second call
// for an order id it has already executed returns THE SAME fills rather than
// executing again — the behaviour a real exchange's clOrdId dedup gives us, and
// the behaviour crash recovery depends on.
func (v *SimVenue) Execute(_ context.Context, st *orderpb.OrderState) ([]*orderpb.Fill, error) {
	if st == nil {
		return nil, errors.New("execution: nil order state")
	}

	v.mu.Lock()
	if prior, ok := v.executed[st.GetOrderId()]; ok {
		v.mu.Unlock()
		return prior, nil
	}
	v.mu.Unlock()

	price := v.executionPrice(st)
	if price == nil || dec.IsZero(price) {
		// PERMANENT, not transient: this venue has no price source for this order
		// type and will not acquire one at runtime, so every retry resolves the
		// same way. It used to return (nil, nil) — "no fill" — which left the
		// order RESTING FOREVER and indistinguishable from a working limit order.
		// A capital-path no-op that looks like normal operation is the wrong
		// failure direction; the caller must refuse the order, not re-queue it.
		//
		// Nothing is recorded here: an order this venue refused to price was
		// never executed, so a later query must answer UNKNOWN, not FILLED.
		return nil, fmt.Errorf("%w: %s has no price source for order type %s",
			ErrUnpriced, v.mic, st.GetOrderType())
	}
	fill := &orderpb.Fill{
		FillId:       v.newID(),
		OrderId:      st.GetOrderId(),
		InstrumentId: st.GetInstrumentId(),
		Side:         st.GetSide(),
		Quantity:     st.GetLeavesQuantity(),
		Price:        price,
		Venue:        v.mic,
		// The account that ACTUALLY executed — the venue's own, not the order's
		// intent. The ledger posts where the cash moved, not where it was meant to.
		VenueAccountId: v.account,
		ExecutedAt:     timestamppb.New(v.now().UTC()),
	}
	fills := []*orderpb.Fill{fill}

	v.mu.Lock()
	defer v.mu.Unlock()
	// Re-check under the lock: two concurrent Executes of one order id must
	// produce ONE execution, and the loser adopts the winner's fills. Returning
	// its own would be the double trade in miniature.
	if prior, ok := v.executed[st.GetOrderId()]; ok {
		return prior, nil
	}
	v.executed[st.GetOrderId()] = fills
	return fills, nil
}

// QueryOrder answers what this venue did with an order — the Querier capability.
//
// SimVenue fills in full or not at all, so an order it remembers is FILLED and
// an order it does not is UNKNOWN. UNKNOWN here is an AFFIRMATIVE statement:
// this venue keeps a complete record for its process lifetime, so its silence
// about an order really does mean it never executed one.
func (v *SimVenue) QueryOrder(_ context.Context, st *orderpb.OrderState) (OrderView, error) {
	if st == nil {
		return OrderView{}, errors.New("execution: nil order state")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	fills, ok := v.executed[st.GetOrderId()]
	if !ok {
		return OrderView{State: OrderViewUnknown}, nil
	}
	return OrderView{State: OrderViewFilled, Fills: fills}, nil
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
