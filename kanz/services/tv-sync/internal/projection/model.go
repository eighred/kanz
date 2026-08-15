// Package projection is tv-sync's zero-truth read model: a bus.EventHandler that
// folds the order/fill FACT stream into a per-(tenant, account) view of orders,
// positions, executions, and P&L, and serves it to the TradingView Broker API.
// It owns no source of truth — a full re-fold of the FACT log reproduces the
// view exactly (the house determinism contract). Reads are bitemporal: every
// element carries an effective time (when it economically happened) and a
// knowledge time (when tv-sync learned of it), so an as-of-knowledge read
// reconstructs "what we knew at time T".
package projection

import (
	"math/big"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// execution is one folded Fill. Effective = venue execution time; knowledge =
// when tv-sync folded the FACT.
type execution struct {
	fillID     string
	orderID    string
	instrument string
	venue      string
	side       orderpb.Side
	qty        *big.Rat
	price      *big.Rat
	fee        *big.Rat
	effective  time.Time
	knowledge  time.Time
}

// orderRev is one revision of an order's materialized state. State may be nil
// for a terminal-only transition (reject/cancel/expire) that carried no full
// OrderState; status/reason still record the outcome.
type orderRev struct {
	state     *orderpb.OrderState
	status    orderpb.OrderStatus
	reason    string
	venue     string
	effective time.Time
	knowledge time.Time
}

// account holds one trading account's append-only folded history under a tenant.
type account struct {
	tenant    string
	id        string
	execs     []execution
	orders    map[string][]orderRev // order_id -> revisions in fold order
	seenFills map[string]bool       // fill_id dedup (sync venue path + async ws echo)
}

func newAccount(tenant, id string) *account {
	return &account{tenant: tenant, id: id, orders: make(map[string][]orderRev), seenFills: make(map[string]bool)}
}

// --- output DTOs (the TradingView Broker-API JSON shapes) ---

// PositionDTO is one net position in an instrument (aggregated across venues).
type PositionDTO struct {
	Instrument    string `json:"instrument"`
	Side          string `json:"side"` // long | short | flat
	Qty           string `json:"qty"`
	AvgPrice      string `json:"avgPrice"`
	RealizedPnl   string `json:"realizedPl"`
	UnrealizedPnl string `json:"unrealizedPl,omitempty"`
}

// OrderDTO is one order's current (or as-of) state.
type OrderDTO struct {
	OrderID      string `json:"id"`
	Instrument   string `json:"instrument"`
	Venue        string `json:"venue,omitempty"`
	Side         string `json:"side"`
	Type         string `json:"type"`
	Status       string `json:"status"`
	Qty          string `json:"qty"`
	FilledQty    string `json:"filledQty"`
	LeavesQty    string `json:"leavesQty"`
	AvgFillPrice string `json:"avgFillPrice,omitempty"`
	LimitPrice   string `json:"limitPrice,omitempty"`
	UpdatedAt    string `json:"updatedAt"`
	// ParentOrderID names the working parent this order is a slice of (#435,
	// #484). Empty for an ordinary order.
	//
	// WITHOUT IT A SIX-SLICE ORDER IS SEVEN UNRELATED ROWS. The parent rests at
	// "scheduled" and its children each appear as their own order, with derived
	// ids that deliberately carry no readable relation — an operator looking at
	// the blotter during an incident cannot tell which of them belong together,
	// or that pulling the parent is what stops the rest.
	//
	// It is the one field the client needs to group them; the grouping itself is
	// the client's, because how to present it is a design question and this
	// projection's job is to stop throwing the answer away.
	ParentOrderID string `json:"parentOrderId,omitempty"`
}

// ExecutionDTO is one execution report.
type ExecutionDTO struct {
	FillID     string `json:"id"`
	OrderID    string `json:"orderId"`
	Instrument string `json:"instrument"`
	Venue      string `json:"venue"`
	Side       string `json:"side"`
	Qty        string `json:"qty"`
	Price      string `json:"price"`
	Fee        string `json:"fee,omitempty"`
	ExecutedAt string `json:"time"`
}

// StateDTO is the account manager summary (balance/equity/P&L).
type StateDTO struct {
	AccountID     string `json:"id"`
	Currency      string `json:"currency"`
	Balance       string `json:"balance"`
	Equity        string `json:"equity"`
	RealizedPnl   string `json:"realizedPl"`
	UnrealizedPnl string `json:"unrealizedPl"`
	OpenPositions int    `json:"openPositions"`
}

// AccountDTO identifies an account in GET /accounts.
type AccountDTO struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Currency string `json:"currency"`
}

// Delta is a real-time update pushed on the streaming channel when a fold
// changes an account.
type Delta struct {
	Kind    string `json:"kind"` // order | execution | position | state
	Account string `json:"account"`
	Payload any    `json:"payload"`
}

func sideString(s orderpb.Side) string {
	switch s {
	case orderpb.Side_SIDE_BUY:
		return "buy"
	case orderpb.Side_SIDE_SELL:
		return "sell"
	default:
		return "unspecified"
	}
}

func statusString(s orderpb.OrderStatus) string {
	switch s {
	case orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW:
		return "pending"
	case orderpb.OrderStatus_ORDER_STATUS_ROUTED:
		return "working"
	case orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED:
		return "partially_filled"
	case orderpb.OrderStatus_ORDER_STATUS_FILLED:
		return "filled"
	case orderpb.OrderStatus_ORDER_STATUS_CANCELLED:
		return "cancelled"
	case orderpb.OrderStatus_ORDER_STATUS_REJECTED:
		return "rejected"
	case orderpb.OrderStatus_ORDER_STATUS_EXPIRED:
		return "expired"
	case orderpb.OrderStatus_ORDER_STATUS_WORKING_SCHEDULED:
		// A PARENT BEING WORKED AS A SCHEDULE (#435). Distinct from "working",
		// which means ROUTED — resting at a venue. This order is at no venue at
		// all; its children are.
		//
		// Without this case it fell to "unspecified", which is the one answer that
		// must never be given for a state the platform knows perfectly well: an
		// operator reading this projection would see a live parent order in an
		// unknown state and have no way to tell it from a decode failure.
		return "scheduled"
	default:
		return "unspecified"
	}
}

func typeString(t orderpb.OrderType) string {
	switch t {
	case orderpb.OrderType_ORDER_TYPE_MARKET:
		return "market"
	case orderpb.OrderType_ORDER_TYPE_LIMIT:
		return "limit"
	case orderpb.OrderType_ORDER_TYPE_STOP:
		return "stop"
	case orderpb.OrderType_ORDER_TYPE_STOP_LIMIT:
		return "stopLimit"
	default:
		return "unspecified"
	}
}
