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
// portfolio_id, instrument_id, static terms. Bound to the OMS order store.
type OrderLookup interface {
	Lookup(orderID string) (*orderpb.OrderState, bool)
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
type ExpectedBalances interface {
	Balance(asset string) (amount *big.Rat, ok bool)
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
	MaintenanceMargin *big.Rat
	// MarginRatio is the exchange's own margin ratio for the account, in the
	// VENUE'S units and direction — nothing normalises it. nil ⇒ UNKNOWN.
	MarginRatio *big.Rat
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

// WorkerDeps are the composition-root-supplied collaborators an exchange
// connector's background workers need. Shared by both connectors' Start.
type WorkerDeps struct {
	Publisher Publisher
	Lookup    OrderLookup
	Expected  ExpectedOrders
	Balances  ExpectedBalances
	// Margin is the venue's own margin state for this adapter's account (#408).
	// nil ⇒ this adapter publishes no margin observations, which venuemargin
	// .Announce makes loud rather than silent. It is a FIELD ON WorkerDeps
	// precisely so a new venue adapter cannot omit it by accident:
	// test/arch/workerdeps_completeness_test.go fails the build on a literal
	// that does not name it, so an absent margin seam is a decision visible in a
	// diff — the mechanism that closed the identical #418 omission.
	Margin            VenueMarginSource
	Tenant            string
	ReconcileInterval time.Duration
	TickerInterval    time.Duration
	// MarginInterval is the margin observation poll. <=0 ⇒ venuemargin's default.
	MarginInterval time.Duration
	// Closes is the in-flight-close registry the healing watchdog drains. Nil ⇒
	// the healing seam is disabled.
	Closes PendingCloses
	// CloseTimeout is the in-flight-close force-clear trigger. <=0 ⇒ 1500ms.
	CloseTimeout time.Duration
	// HealInterval is the healing watchdog tick. <=0 ⇒ 500ms.
	HealInterval time.Duration
	Logger       *slog.Logger
}
