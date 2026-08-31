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
	"github.com/eighred/kanz/pkg/bus"
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

	// mu guards executed, lastSweep and forgotten. SimVenue is shared by every
	// goroutine handling orders for this MIC, so the record is concurrent by
	// construction.
	mu sync.Mutex
	// executed maps order_id → what this venue reported for it.
	//
	// IT USED TO BE UNBOUNDED, and the comment said so in as many words: "it is a
	// simulator, its lifetime is a process". The dedup half of that argument is
	// sound and is why the record still exists; the "it is a simulator" half was
	// checked and did not survive (#895). services/oms/cmd/oms/venues.go selects
	// NewSimVenue for every MIC whenever OMS_VENUE_ENDPOINTS is empty, so the
	// process holding this map is a long-lived OMS pod with an order rate, not a
	// test binary — one permanent entry per order, for the life of the pod, on
	// the execution path.
	executed map[string]simRecord
	// retain is how long a record stays readable after the execution that
	// produced it. <=0 disables eviction entirely.
	retain time.Duration
	// lastSweep is when evictExecuted last walked the record. The sweep is
	// AMORTIZED — at most once per retain, on the write path — so Execute stays
	// O(1) at the order rate. The cost is that an entry lives somewhere between
	// retain and 2*retain; what matters is that its lifetime is a function of the
	// ORDER RATE over a fixed window rather than of how long the pod has been up.
	// orderview.Memory bounds itself the same way and for the same reason.
	lastSweep time.Time
	// forgotten latches the first time evictExecuted actually drops something.
	// It is what keeps QueryOrder honest — see the answer it gives on a miss.
	forgotten bool
}

// simRecord is what SimVenue reported for one order, plus when it reported it.
// The instant is what the retention is measured from; it is kept beside the
// fills rather than read back out of Fill.executed_at so that eviction does not
// depend on a wire field a future change could stop setting.
type simRecord struct {
	fills      []*orderpb.Fill
	executedAt time.Time
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

// WithRetention overrides how long an executed order stays remembered. It exists
// for this package's own tests, which cannot wait out DefaultSimRetention; a
// non-positive value disables eviction, which is the pre-#895 behaviour and is
// what the double-fill tests pin against. Nothing outside a test sets it.
func WithRetention(d time.Duration) SimOption { return func(v *SimVenue) { v.retain = d } }

// DefaultSimRetention is how long SimVenue remembers an order it executed, and
// it is DERIVED rather than chosen: one full bus.WorkRedeliveryBudget().
//
// WHAT THE RECORD IS FOR, WHICH IS WHAT SIZES IT. A submit command that fails
// after venue.Execute returned — the store.Save of venue_ack_at, the fill fold,
// the relay flush — is NAKed and redelivered, and the redelivery re-drives the
// same order. This record is what makes the second Execute return the FIRST
// one's fills instead of minting a second fill_id for one execution. So the
// window it has to cover is the window in which the broker can keep re-offering
// that command, and that is not a number this package gets to pick: it is
// workTuning's delivery budget, which pkg/bus computes and exports.
//
// WHY THE RESIDUAL IS NOT A DOUBLE FILL. The budget is not the only replay path
// — the OMS's periodic sweep (OMS_SWEEP_INTERVAL) re-reads non-terminal rows of
// ANY age, so an order stuck at ROUTED can be re-driven hours later, and no
// finite retention can cover that. That is exactly why eviction here also sets
// the forgotten latch: past the retention this venue stops claiming it never
// executed an order it cannot find, QueryOrder answers INDETERMINATE, and
// order.Reconcile quarantines instead of re-driving. The retention decides how
// often that costs an operator a quarantine; it does not decide whether a
// forgotten order can be filled twice.
func DefaultSimRetention() time.Duration { return bus.WorkRedeliveryBudget() }

// NewSimVenue returns a simulation venue with the given MIC.
func NewSimVenue(mic string, opts ...SimOption) *SimVenue {
	v := &SimVenue{
		mic:      mic,
		account:  "sim:" + mic,
		now:      time.Now,
		newID:    uuid.NewString,
		executed: make(map[string]simRecord),
		retain:   DefaultSimRetention(),
	}
	for _, opt := range opts {
		opt(v)
	}
	// AFTER the options, so an injected clock is the one the first sweep window
	// is measured from. Seeding it from time.Now under a frozen test clock would
	// make the first sweep due immediately or never, depending on the epoch.
	v.lastSweep = v.now()
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
		return prior.fills, nil
	}
	v.mu.Unlock()

	// ONE clock read for the whole execution: the fill's executed_at, the
	// retention this record is measured against and the sweep window must all be
	// the same instant, or a record can be born already expired.
	executedAt := v.now().UTC()

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
		ExecutedAt:     timestamppb.New(executedAt),
	}
	fills := []*orderpb.Fill{fill}

	v.mu.Lock()
	defer v.mu.Unlock()
	// Re-check under the lock: two concurrent Executes of one order id must
	// produce ONE execution, and the loser adopts the winner's fills. Returning
	// its own would be the double trade in miniature.
	if prior, ok := v.executed[st.GetOrderId()]; ok {
		return prior.fills, nil
	}
	v.executed[st.GetOrderId()] = simRecord{fills: fills, executedAt: executedAt}
	// SWEEP ON THE WRITE PATH, and only there. This map grows in exactly one
	// place, so that is the one place a bound can be enforced without the
	// simulator owning a goroutine's lifecycle; a SimVenue nobody executes
	// against does not grow, so there is nothing a background sweep would catch
	// that this misses. Same shape as orderview.Memory.evictTerminal.
	v.evictExecuted(executedAt)
	return fills, nil
}

// evictExecuted drops records older than the retention. Caller holds v.mu.
//
// EVERY DROP LATCHES forgotten, and that latch is the load-bearing half of this
// change rather than bookkeeping. Before it, QueryOrder's UNKNOWN was an
// AFFIRMATIVE statement — "I keep a complete record, so my silence means I never
// executed this" — and order.Reconcile is built on exactly that: UNKNOWN with no
// venue_ack_at recorded means RE-DRIVE. Evicting without saying so would leave
// that sentence false and turn a forgotten order into a second fill on the very
// path this record exists to protect.
func (v *SimVenue) evictExecuted(now time.Time) {
	if v.retain <= 0 || now.Sub(v.lastSweep) < v.retain {
		return
	}
	v.lastSweep = now
	for id, rec := range v.executed {
		if now.Sub(rec.executedAt) >= v.retain {
			delete(v.executed, id)
			v.forgotten = true
		}
	}
}

// QueryOrder answers what this venue did with an order — the Querier capability.
//
// SimVenue fills in full or not at all, so an order it remembers is FILLED.
//
// AN ORDER IT DOES NOT REMEMBER IS UNKNOWN ONLY WHILE IT HAS FORGOTTEN NOTHING.
// UNKNOWN is an AFFIRMATIVE statement — the venue positively asserting it never
// executed this order — and order.Reconcile turns it into a RE-DRIVE when the
// OMS holds no venue ack. That was true while this record was permanent. Once
// the retention has dropped even one entry (#895) this venue can no longer tell
// "I never executed it" from "I executed it and forgot", and asserting the first
// would be a second fill for one order.
//
// So a miss after any eviction is INDETERMINATE, which Reconcile quarantines.
// The cost is a false quarantine for an order this simulator genuinely never
// executed, on a pod that has been up longer than the retention; the cost of the
// other direction is the fund trading twice. Fail closed.
//
// THE LATCH IS DELIBERATELY NOT APPLIED TO Execute, which is the asymmetry a
// reader will ask about. Execute cannot distinguish a forgotten order from a
// brand-new one — a new order is a miss too — so refusing every miss there would
// not bound anything, it would stop the simulator dead. It does not need to: the
// only path that re-drives an order this venue may already hold runs through
// resume(), which queries FIRST and quarantines on INDETERMINATE, and which
// quarantines outright against a venue that implements no Querier at all.
func (v *SimVenue) QueryOrder(_ context.Context, st *orderpb.OrderState) (OrderView, error) {
	if st == nil {
		return OrderView{}, errors.New("execution: nil order state")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	rec, ok := v.executed[st.GetOrderId()]
	if !ok {
		if v.forgotten {
			return OrderView{
				State: OrderViewIndeterminate,
				Reason: fmt.Sprintf(
					"%s is a simulator that has dropped executed-order records older than %s, so it "+
						"cannot state whether it executed this one. Re-driving might fill the fund twice",
					v.mic, v.retain),
			}, nil
		}
		return OrderView{State: OrderViewUnknown}, nil
	}
	return OrderView{State: OrderViewFilled, Fills: rec.fills}, nil
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
