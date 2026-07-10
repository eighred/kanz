package execution

import (
	"context"
	"log/slog"
	"math/big"
	"time"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"

	"github.com/kanz-eng/kanz/pkg/bus"
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
// bound to the accounting projection. A nil result ⇒ zero.
type ExpectedBalances interface {
	Balance(asset string) *big.Rat
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
	Logger            *slog.Logger
}
