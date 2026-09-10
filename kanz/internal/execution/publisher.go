package execution

import (
	"context"
	"log/slog"
	"math/big"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/pkg/bus"
)

// The seams the exchange connectors' background workers wire to. They are
// defined here (untagged) so both the Binance and OKX connectors — and the
// composition root that supplies them — reference one set of interfaces; the
// concrete workers that consume them are compiled only under an exchange tag.

// Publisher is the bus publish surface — satisfied by *bus.Producer.
type Publisher interface {
	Publish(ctx context.Context, e bus.Event) error
}

// OrderLookup enriches an exchange execution report (which carries only the
// client order id = our order_id, and the symbol) with Kanz's order context —
// portfolio_id, instrument_id, static terms. Bound to the adapter's own order
// view.
//
// THE ANSWER HAS THREE VALUES, NOT TWO (#1047). It returned (state, ok) and the
// one implementation folded a store failure into (nil, false) — the same answer
// it gives for an order this adapter does not hold. The ingesters then answered
// that collapsed value the only way it can be answered, with a skip, so a
// Postgres blip inside a venue adapter deleted real executions from the
// order.order.filled stream: no FACT, so no position booked and no cash
// journalled, and nothing downstream could notice because there was nothing to
// notice.
//
// The two are opposite readings of the same silence. "The view answered, and
// does not hold this order" is a legitimate skip — a shared exchange account and
// a second replica both produce reports that are not ours. "I could not read the
// view" means this adapter almost certainly DOES own the order and cannot tell,
// and treating it as somebody else's applies the safe reading of the first case
// to the second.
//
// This is the same three-value discipline ExpectedBalances takes for an unknown
// balance (#418) and OrderViewState takes for INDETERMINATE: an unknown is a
// value, never the benign default.
type OrderLookup interface {
	// Lookup returns this adapter's record of orderID.
	//
	//	(st,  true,  nil) — held, and this is it
	//	(nil, false, nil) — the view answered, and does not hold it
	//	(nil, false, err) — the view could not be read; the caller knows NOTHING
	//	                    about this order and must not treat it as somebody
	//	                    else's
	Lookup(orderID string) (*orderpb.OrderState, bool, error)
}

// OrderTracker is the adapter's OWN order view, read and write.
//
// THE READ HALF ALONE IS WHY A FILLED ORDER LEAKED (#904). OrderLookup had no
// write half, so the ONLY thing that ever advanced an order to a terminal status
// was the venue-confirmed-cancel branch of the gRPC CancelOrder handler. The
// user-data ingester receives the fill, computes the healed OrderState, and
// publishes it as a FACT — and then had nowhere to put it. An order that FILLED
// therefore stayed at the status the OMS handed Execute for the life of the
// process, with three consequences on the execution path: ExpectedOrders.
// OpenOrders returned it forever, so the reconciler spent REST weight re-querying
// orders that finished last week; healedState found permanent drift and
// re-emitted an order.state_healed FACT for each of them on EVERY pass; and
// #891's terminal-order eviction could never reach them, because eviction is
// keyed on going terminal and they never did.
//
// IT IS ONE INTERFACE AND NOT TWO FIELDS, deliberately. A separate, nil-able
// recorder seam would let a composition root wire the reader and forget the
// writer, and the result would be exactly the defect above with nothing to say
// so — AGENTS.md's "nothing configured" and "checked, and fine" must never look
// the same. Widening the type a venue adapter already supplies makes a
// write-less order view a COMPILE error instead.
type OrderTracker interface {
	OrderLookup
	// Progressed records the venue's own report of an order's progress onto this
	// adapter's view: the state the caller has ALREADY published as a FACT, so
	// the view agrees with what the adapter itself told the platform.
	//
	// It does not return an error, for the reason Lookup and OpenOrders do not:
	// it is called from inside a websocket read loop that must not acquire an
	// error path. An implementation reports its own failures (orderview.NewSeam
	// takes the callback), and a failure leaves the view stale — never wrong.
	//
	// IT IS NOT A SECOND SOURCE OF TRUTH. The OMS owns an order's status; this
	// is the adapter's record of what it was asked to work and what the exchange
	// has since said about it. See orderview.Progress for what it may and may
	// not overwrite.
	Progressed(st *orderpb.OrderState)

	// Quarantined FREEZES an order because the venue and the platform disagree
	// about what was authorised — today, a venue reporting a cumulative filled
	// quantity larger than the quantity this platform ever sent (#1045).
	//
	// IT IS THE OTHER HALF OF REFUSING A REPORT. Refusing alone would drop the
	// execution silently, which trades a wrong number for a missing one; the
	// freeze is what makes the refusal readable afterwards, and what stops the
	// order being re-dispatched at a size nobody can state — orderview.Dispatch
	// declines a quarantined order the way it declines a terminal one.
	//
	// Non-failing for the same reason Progressed is: it runs inside a websocket
	// read loop. The caller has already counted and logged the refusal before
	// calling this, so an implementation that cannot persist the freeze degrades
	// the record rather than erasing the finding.
	Quarantined(st *orderpb.OrderState, reason string)
}

// ExpectedOrders is Kanz's internal view of the orders it believes are open —
// bound to the OMS order store. The reconciler queries each on the exchange and
// heals any that have drifted.
type ExpectedOrders interface {
	OpenOrders() []*orderpb.OrderState
}

// ExpectedBalances is Kanz's internal per-asset balance for the venue account —
// bound to the book of record's announcements (#450).
//
// ok=false MEANS UNKNOWN, AND IT IS NOT ZERO (#418). The signature used to
// return only a *big.Rat, documented as "a nil result ⇒ zero", and both
// reconcilers duly substituted zero. That turns a portfolio nobody has announced
// — a cold adapter, a lost announcement, a balance too old to trust — into the
// claim that Kanz believes it holds NOTHING, so every asset the exchange
// actually holds is reported as a discrepancy.
//
// A reconciliation that reports a break on every asset the first time it runs is
// worse than one that does not run: it trains an operator to ignore the layer,
// which is the failure the observability guards in test/arch exist to prevent.
// So an unknown balance SKIPS the asset, loudly, and a known one is compared.
//
// THE UNKNOWN NAMES ITSELF (#1063), which is the third value's other half. "We
// have never been told what this account holds" and "what we were told is too
// old to compare against" are the same ok=false and are not the same incident:
// the first is a cold adapter or a cash spine that has never delivered, the
// second is a spine that has stopped. An operator chases different things, and a
// counter that cannot separate them is a counter that only says "something".
// The reason is returned BESIDE the verdict rather than asked for afterwards
// because the two must describe the same instant — the healing watchdog
// reconciles balances from its own goroutine, so a second call could read a view
// that has since been announced to and label a real gap as healthy.
type ExpectedBalances interface {
	// Balance returns the amount of asset this account is believed to hold.
	//
	//	(amount, true,  "")       — known, and this is it
	//	(nil,    false, reason)   — UNKNOWN, and reason says why
	//
	// reason is one of the BalanceUnknown* constants below. An implementation
	// that answers UNKNOWN without one is normalised to
	// BalanceUnknownUnattributed by the caller rather than counted under an
	// empty label, so a seam that forgets to name its reason is visible instead
	// of being folded into a series that reads like a real cause.
	Balance(asset string) (amount *big.Rat, ok bool, reason string)
}

// The reasons an expected balance is UNKNOWN — a BOUNDED set, because they are a
// Prometheus label (#1063). Declared here rather than in balancerecon so both
// reconcilers, both composition roots and the alert rule name one spelling; a
// venue that invented a second word for "stale" would split the series an
// operator is paged on.
const (
	// BalanceUnknownNeverAnnounced: no cash announcement has EVER reached this
	// adapter for this account. A cold adapter reads this for its first few
	// passes, which is expected; an adapter that reads it for hours is one whose
	// cash spine subscription never landed, and reconciliation has therefore
	// never once compared this account against the exchange.
	BalanceUnknownNeverAnnounced = "never_announced"
	// BalanceUnknownStale: an announcement arrived and is now older than the
	// freshness bound. The spine has stopped rather than never started, and the
	// comparison is being skipped to avoid reporting staleness as a break.
	BalanceUnknownStale = "stale"
	// BalanceUnknownUnattributed: the seam answered UNKNOWN and named no reason.
	// It is here so that "the implementation did not say" is a LABEL rather than
	// an empty string — an empty label value is still a series, and it reads on a
	// dashboard like a cause somebody chose.
	BalanceUnknownUnattributed = "unattributed"
)

// BalanceUnknownReasons is the whole bounded set, in the order a dashboard reads
// them. Both composition roots seed their counter from THIS slice rather than
// from a list retyped per venue: a reason nobody seeds exports no series until
// its first increment, and an alert over a series that does not exist is silent
// in exactly the state it detects (#973, #963).
var BalanceUnknownReasons = []string{
	BalanceUnknownNeverAnnounced,
	BalanceUnknownStale,
	BalanceUnknownUnattributed,
}

// NamedBalanceUnknown normalises a seam's reason for an UNKNOWN balance onto the
// bounded set. An empty or unrecognised reason becomes
// BalanceUnknownUnattributed — the label cardinality is fixed HERE, at the one
// place both reconcilers pass through, so a venue adapter cannot widen it by
// returning a new string.
func NamedBalanceUnknown(reason string) string {
	for _, known := range BalanceUnknownReasons {
		if reason == known {
			return reason
		}
	}
	return BalanceUnknownUnattributed
}

// VenueMarginSource is the EXCHANGE'S own margin state for the account this
// adapter's credential spends from (#408, control 1).
//
// IT IS A READ OF THE VENUE, NOT A COMPUTATION. internal/collateral can model
// what a margin requirement SHOULD be under an agreement we hold; the exchange
// margins on its own tiers, its own mark and its own schedule, and liquidates
// without asking. #408 names the failure this seam exists for — "the exchange
// sold our collateral while we were reading stale books" — and a number derived
// from our own positions IS that stale book. So an implementation reports what
// the venue said and NOTHING where the venue said nothing.
//
// A nil source is not an error and must not refuse startup: an adapter that
// cannot see margin can still route and fill unleveraged orders, and taking the
// pod down over a reporting seam would be a self-inflicted trading outage. It
// must simply never be quiet about it — see venuemargin.Announce.
type VenueMarginSource interface {
	MarginState(ctx context.Context) (VenueMargin, error)
}

// SupportStatus is the adapter's observed capability for one venue field.
// Unknown fails closed. Unsupported says the field is inapplicable to the
// observed account mode; it is never permission to substitute numeric zero.
type SupportStatus uint8

const (
	SupportUnknown SupportStatus = iota
	SupportSupported
	SupportUnsupported
)

// VenueMargin is one observation of a venue account's margin state.
//
// EVERY QUANTITY IS A POINTER AND nil MEANS THE VENUE DID NOT REPORT IT. This is
// the whole discipline of the type: a *big.Rat that is nil cannot be added to,
// compared against a limit, or mistaken for a measured zero, whereas a zero
// big.Rat is a claim — "this account needs no collateral" — that reads as
// perfectly healthy on every dashboard. This estate has shipped that confident
// zero five times in two days; margin is the one where being wrong costs the
// fund its collateral.
//
// A SOURCE MUST NEVER SYNTHESISE A FIELD. If the exchange returns an empty
// string, a field the API version does not carry, or a number that will not
// parse, the answer is nil and the reporter turns that into coverage evidence a
// consumer refuses on. Filling it in from our own book would be the defect.
type VenueMargin struct {
	// MaintenanceMargin is the collateral the exchange requires the account to
	// keep, in the account's own valuation currency. nil ⇒ UNKNOWN.
	MaintenanceMargin        *big.Rat
	MaintenanceMarginSupport SupportStatus
	// MarginRatio is the exchange's own margin ratio for the account, in the
	// VENUE'S units and direction — nothing normalises it. nil ⇒ UNKNOWN.
	MarginRatio        *big.Rat
	MarginRatioSupport SupportStatus
	// Positions are the open positions the venue reported, each with the
	// liquidation price the venue gave for it — or a nil price where it gave
	// none. A source lists the position EITHER WAY: dropping the ones without a
	// price would hide, rather than report, the positions nobody can see the
	// liquidation distance of.
	Positions []VenuePositionMargin
	// ObservedAt is when this state was true AT THE VENUE. A source prefers the
	// exchange's own update stamp and falls back to its fetch time only where the
	// API carries none. The zero value is refused by the reporter rather than
	// substituted: a margin number that cannot be dated is not a margin number.
	ObservedAt time.Time
}

// VenuePositionMargin is one open position as the venue reported it.
type VenuePositionMargin struct {
	// Symbol is the EXCHANGE'S instrument id, not a Kanz instrument_id. Mapping
	// it back through the adapter's one-way symbol table would drop any position
	// the table does not cover — which is exactly the position nobody is
	// watching, and the one whose liquidation price matters most.
	Symbol string
	// LiquidationPrice is the venue's liquidation price for the position. nil ⇒
	// the venue reported the position but no liquidation price for it.
	LiquidationPrice *big.Rat
}

// UserDataStream is the exchange private-websocket transport seam: it yields raw
// frames. The concrete Binance/OKX implementations dial the venue; tests inject
// a fake, so the fill-conversion logic is certified without a network.
type UserDataStream interface {
	Recv(ctx context.Context) ([]byte, error)
}

// THE THREE WINDOWS THE BACKGROUND WORKERS RUN ON, IN ONE PLACE (#891).
//
// Each of these was a literal inside BOTH reconcilers, restated as prose in the
// WorkerDeps field docs below, which owned neither copy and were checked against
// neither. A third consumer then made the duplication load-bearing:
// orderview.DefaultTerminalRetention is DERIVED from the reconcile interval,
// because how long a venue adapter keeps a finished order readable is not an
// independent choice from how often it re-asks the exchange for truth. That is
// the same argument bus.defaultDedupTTL makes for the DLQ drain's minimum age —
// two intervals with a required ordering must not be two literals in two files.
const (
	// DefaultReconcileInterval is how often a reconciler re-reads venue truth for
	// the orders it believes are open. It is the widest window this adapter
	// operates on: the longest it tolerates its own view diverging from the
	// exchange before something re-checks.
	DefaultReconcileInterval = time.Minute
	// DefaultCloseTimeout is how long an in-flight close may stay unconfirmed
	// before the healing watchdog force-clears it (the In-Flight Certainty
	// mandate's trigger).
	DefaultCloseTimeout = 1500 * time.Millisecond
	// DefaultHealInterval is the healing watchdog's own tick — deliberately
	// faster than DefaultCloseTimeout, so a close becoming due is acted on inside
	// one tick rather than one full timeout later.
	DefaultHealInterval = 500 * time.Millisecond
)

// WorkerDeps are the composition-root-supplied collaborators an exchange
// connector's background workers need. Shared by both connectors' Start.
type WorkerDeps struct {
	Publisher Publisher
	// Orders is the adapter's own order view — the read half the user-data
	// ingester enriches an execution report from, AND the write half that
	// advances the order when the venue reports it filled (#904). It was
	// `Lookup OrderLookup` and read-only, which is why a filled order was never
	// marked terminal here; see execution.OrderTracker.
	Orders   OrderTracker
	Expected ExpectedOrders
	Balances ExpectedBalances
	// Margin is the venue's own margin state for this adapter's account (#408).
	// nil ⇒ this adapter publishes no margin observations, which venuemargin
	// .Announce makes loud rather than silent. It is a FIELD ON WorkerDeps
	// precisely so a new venue adapter cannot omit it by accident:
	// test/arch/workerdeps_completeness_test.go fails the build on a literal
	// that does not name it, so an absent margin seam is a decision visible in a
	// diff — the mechanism that closed the identical #418 omission.
	Margin VenueMarginSource
	Tenant string
	// ReconcileInterval is the venue-truth poll. <=0 ⇒ DefaultReconcileInterval.
	ReconcileInterval time.Duration
	TickerInterval    time.Duration
	// MarginInterval is the margin observation poll. <=0 ⇒ venuemargin's default.
	MarginInterval time.Duration
	// Closes is the in-flight-close registry the healing watchdog drains. Nil ⇒
	// the healing seam is disabled.
	Closes PendingCloses
	// CloseTimeout is the in-flight-close force-clear trigger. <=0 ⇒ DefaultCloseTimeout.
	CloseTimeout time.Duration
	// HealInterval is the healing watchdog tick. <=0 ⇒ DefaultHealInterval.
	HealInterval time.Duration
	// OnMarkTickDropped is called for EVERY reference-mark tick the exchange
	// answered and the bus did not accept (#673). The composition root wires it
	// to a counter labelled by instrument, so "which instruments went dark, and
	// how often" is a number rather than an inference from silence.
	//
	// It is a FIELD ON WorkerDeps for the reason Margin is: a third venue adapter
	// must not be able to omit it by accident. The counter is the alertable half
	// of the signal — MarkTickPublisher's WARN is rate-limited by design and a
	// log line is not a threshold — so nil here is a real reduction in what an
	// operator can see, and the completeness guard in test/arch makes choosing
	// nil a visible decision in a diff rather than a field nobody typed.
	OnMarkTickDropped func(mic, instrumentID string)

	// OnFillRefused is called for EVERY venue execution report an ingester
	// refused to turn into a fill FACT (#1045): the venue reported a cumulative
	// filled quantity the platform never authorised, or one that will not convert
	// to a Decimal at all.
	//
	// IT IS THE ALERTABLE HALF, and on this path there is no other. The
	// synchronous and recovery fill paths meet the OMS aggregate, whose OVERFILL
	// refusal quarantines the order and moves the OMS's own quarantine counter.
	// The user-data websocket meets neither — the OMS order aggregate does not
	// consume the fill subject — so without this an over-fill is a log line in a
	// venue adapter, and "the venue over-filled us and we refused" is
	// indistinguishable from "no venue has ever over-filled us".
	//
	// A FIELD ON WorkerDeps for the reason Margin and OnMarkTickDropped are: a
	// third venue adapter must not be able to omit it by accident, and the
	// completeness guard in test/arch makes choosing nil a visible decision in a
	// diff rather than a field nobody typed.
	OnFillRefused func(mic, orderID, reason string)

	// OnFillDropped is called for EVERY venue execution report an ingester did
	// not turn into a fill FACT because it could not RESOLVE it to an order this
	// adapter holds (#1047), with the reason separating the two cases that used
	// to be one.
	//
	// IT IS A DIFFERENT QUESTION FROM OnFillRefused, and that is why it is a
	// different seam. A refusal means the report was understood and contradicts
	// what this platform authorised — a standing disagreement that freezes the
	// order. A drop means the report was never resolved to an order at all:
	// either it belongs to somebody else (DropUnknownOrder, routine on a shared
	// exchange account) or the order view could not be read (DropStoreError, an
	// execution this adapter probably owns and is blind to).
	//
	// WHAT IT ANSWERS. "How many fills did we lose during that outage?" — a
	// question the ERROR log cannot answer and no other series on this path can
	// either. The unknown_order arm is what makes the store_error arm readable:
	// an adapter that routinely sees other people's orders has a visible baseline
	// to compare against.
	//
	// A FIELD ON WorkerDeps for the reason OnFillRefused is: a third venue
	// adapter must not be able to omit it by accident, and the completeness guard
	// in test/arch makes choosing nil a visible decision in a diff.
	OnFillDropped func(mic, orderID, reason string)

	// OnCloseUnhealable is called for EVERY in-flight close the healing watchdog
	// dropped WITHOUT asking the exchange anything (#1036): the intent named no
	// instrument, or named one this venue has no symbol for. Either way the
	// watchdog could not form the venue query, so no StateHealed was emitted, no
	// force-clear ran and no balance was re-anchored — while the OMS has already
	// written CANCELLED, which is terminal.
	//
	// IT IS THE ALERTABLE HALF, and this seam had none. The drop happened on the
	// same Resolve() line as a close the watchdog had genuinely healed against
	// venue truth, so "In-Flight Certainty is working" and "In-Flight Certainty has
	// never once reached the exchange" were the same silence — which is exactly how
	// the empty InstrumentID this field was added with survived: every close the
	// out-of-process adapter tracked was dropped as "untradeable here".
	//
	// A FIELD ON WorkerDeps for the reason Margin, OnMarkTickDropped and
	// OnFillRefused are: a third venue adapter must not be able to omit it by
	// accident, and the completeness guard in test/arch makes choosing nil a
	// visible decision in a diff rather than a field nobody typed.
	OnCloseUnhealable func(orderID, instrumentID, reason string)

	// OnUnknownBalance is called for EVERY asset a reconciliation pass could not
	// check, because Kanz has no usable expected balance for it (#1063), with the
	// reason from BalanceUnknownReasons.
	//
	// IT IS THE ALERTABLE HALF, and this seam had none. Both reconcilers have
	// honoured the callback since #418 and NEITHER composition root wired it, so
	// an asset whose expected balance is UNKNOWN produced output identical to one
	// that reconciled cleanly: no FACT, no counter, no log, a pass that returns
	// nil. accounting.balance.reconciled is emitted only on a DISCREPANCY, so the
	// silence of a skipped asset and the silence of an agreeing one are the same
	// silence — AGENTS.md's rule verbatim.
	//
	// WHY IT IS NOT A BREAK. A break FACT carries expected, actual and delta, and
	// the whole condition here is that expected does not exist. Publishing one
	// means inventing expected=0, which is precisely the fabrication #418 removed
	// from both reconcilers — it reports every asset the exchange holds as a
	// discrepancy on the first cold pass, and the journal folds it bitemporally,
	// so the invented figure becomes durable book state rather than a dashboard
	// mistake. A break says the two sides DISAGREE; this says one side is
	// MISSING, and only the first is an incident an operator resolves against the
	// exchange's own history.
	//
	// WHAT IT COSTS TO BE SILENT. Balance reconciliation is the last thing that
	// can notice a mis-booked position — a fill the websocket missed, one posted
	// to the wrong account, a residual a sweep left behind. A pass that skips
	// what it could not evaluate is running in name only for that asset, and
	// nothing anywhere said so.
	//
	// A FIELD ON WorkerDeps for the reason Margin, OnMarkTickDropped,
	// OnFillRefused and OnCloseUnhealable are: a third venue adapter must not be
	// able to omit it by accident, and the completeness guard in test/arch makes
	// choosing nil a visible decision in a diff rather than a field nobody typed.
	// That guard is the reason this field exists HERE rather than only on the two
	// ReconcilerConfigs — the omission it repairs was in the composition roots.
	OnUnknownBalance func(asset, reason string)

	Logger *slog.Logger
}
