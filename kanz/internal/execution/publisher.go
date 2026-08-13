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

// UserDataStream is the exchange private-websocket transport seam: it yields raw
// frames. The concrete Binance/OKX implementations dial the venue; tests inject
// a fake, so the fill-conversion logic is certified without a network.
type UserDataStream interface {
	Recv(ctx context.Context) ([]byte, error)
}

// WorkerDeps are the composition-root-supplied collaborators an exchange
// connector's background workers need. Shared by both connectors' Start.
type WorkerDeps struct {
	Publisher         Publisher
	Lookup            OrderLookup
	Expected          ExpectedOrders
	Balances          ExpectedBalances
	Tenant            string
	ReconcileInterval time.Duration
	TickerInterval    time.Duration
	// Closes is the in-flight-close registry the healing watchdog drains. Nil ⇒
	// the healing seam is disabled.
	Closes PendingCloses
	// CloseTimeout is the in-flight-close force-clear trigger. <=0 ⇒ 1500ms.
	CloseTimeout time.Duration
	// HealInterval is the healing watchdog tick. <=0 ⇒ 500ms.
	HealInterval time.Duration
	Logger       *slog.Logger
}
