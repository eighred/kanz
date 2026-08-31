package binance

// The Querier half of the venue contract (#920): what did Binance do with this
// order?
//
// It is a READ, and it is the read the OMS's crash recovery is built on. Until
// venue.v1 carried a query RPC, SimVenue was the only execution.Querier in the
// platform, so `Service.resume` quarantined EVERY interrupted ROUTED order at
// every real venue and order.Reconcile's policy table was exercised only against
// a simulator. This connector always knew the answer — its own reconciler has
// called `queryOrder` since INFRA-M7a — and nothing surfaced it.
//
// # THE ONE MISTAKE THIS FILE EXISTS TO NOT MAKE
//
// OrderViewUnknown is an AFFIRMATIVE statement: Binance says it has no such
// order. order.Reconcile turns it into ActionRedrive when the OMS holds no venue
// ack, and ActionRedrive PLACES THE ORDER AT A REAL EXCHANGE. So the only thing
// in this file that may produce it is Binance's own -2013 "Order does not
// exist", decoded off a response Binance actually sent.
//
// A timeout, a DNS failure, a 5xx, an unreadable body and an exhausted weight
// budget are all returned as ERRORS — "the question could not be asked" — and
// the OMS backs off. A venue status this connector cannot map is
// OrderViewIndeterminate, which quarantines. Neither ever becomes UNKNOWN.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// binanceOrderDoesNotExist is Binance's -2013 "Order does not exist." on a
// QUERY. It is the only code that means what OrderViewUnknown means, and it is
// deliberately a different constant from binanceUnknownOrder (-2011), which is
// what a CANCEL gets for an order that is no longer working. The two look alike
// and mean opposite things: -2011 says "it is not open" about an order that may
// well have filled; -2013 says "I have never held this id".
const binanceOrderDoesNotExist = -2013

// QueryOrder reports what Binance did with st, addressed by the deterministic
// clientOrderId the submit stamped.
//
// It satisfies execution.Querier, which the venue adapter's gRPC face serves to
// the OMS. Safe to call repeatedly: it places nothing and mutates nothing.
func (v *BinanceVenue) QueryOrder(ctx context.Context, st *orderpb.OrderState) (OrderView, error) {
	if st == nil {
		return OrderView{}, errors.New("binance: nil order state")
	}
	symbol, ok := v.symbols.Symbol(st.GetInstrumentId())
	if !ok {
		// PERMANENT, so it is a verdict rather than an error. This deployment has
		// no symbol for the instrument, and asking again in a minute changes
		// nothing — a nack would loop to a DLQ where a quarantine names the actual
		// problem to the operator who can fix it.
		return OrderView{
			State: OrderViewIndeterminate,
			Reason: fmt.Sprintf(
				"binance: no symbol mapping for %s, so this adapter cannot ask the exchange about order %s",
				st.GetInstrumentId(), st.GetOrderId()),
		}, nil
	}

	resp, err := v.rest.queryOrder(ctx, symbol, st.GetOrderId())
	if err != nil {
		// THE RATE-LIMIT CHECK IS FIRST AND IS NOT AN APIError. An exhausted
		// weight budget is this connector refusing to fire a request at all; the
		// exchange said nothing, so there is nothing to read as a verdict.
		if errors.Is(err, ErrRateLimited) {
			return OrderView{}, err
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Code == binanceOrderDoesNotExist {
			// Binance ANSWERED, and its answer is that it has never held this
			// client order id. This is the one branch that authorizes a re-drive.
			return OrderView{State: OrderViewUnknown}, nil
		}
		// Any other exchange error, any transport fault, any undecodable body:
		// the question could not be asked. Back off and ask again.
		return OrderView{}, err
	}

	switch resp.Status {
	case "NEW":
		return OrderView{State: OrderViewWorking}, nil

	case "PARTIALLY_FILLED", "FILLED":
		fills, ferr := v.tradesFor(ctx, st, symbol, resp)
		if ferr != nil {
			return OrderView{}, ferr
		}
		if len(fills) == 0 {
			// BINANCE CONTRADICTED ITSELF: it reports the order traded and
			// produced no trade record for it. Reporting FILLED with no fill
			// would leave a traded order looking untraded (the OMS refuses such a
			// view, and is right to); reporting UNKNOWN would re-place an order
			// Binance has just said it executed. Freeze it and say which.
			return OrderView{
				State: OrderViewIndeterminate,
				Reason: fmt.Sprintf(
					"binance reports order %s as %s (executedQty %s) but returned no trade for it, so "+
						"this adapter cannot say what traded", st.GetOrderId(), resp.Status, resp.ExecutedQty),
			}, nil
		}
		state := OrderViewPartiallyFilled
		if resp.Status == "FILLED" {
			state = OrderViewFilled
		}
		return OrderView{State: state, Fills: fills}, nil

	case "REJECTED":
		return OrderView{
			State:  OrderViewRejected,
			Reason: fmt.Sprintf("binance rejected order %s", st.GetOrderId()),
		}, nil

	default:
		// "CANCELED", "EXPIRED", "PENDING_CANCEL", and anything Binance adds
		// later.
		//
		// execution.OrderView HAS NO WITHDRAWN ANSWER, and inventing one here
		// would be a lie in whichever direction it was taken: REJECTED writes a
		// refusal over an order that may have partially traded before it was
		// pulled, UNKNOWN re-places it. So these freeze, which is exactly what
		// they did before this RPC existed — no order is worse off, and the
		// quarantine record now names the venue status instead of naming the
		// absence of a Querier. EXPIRED is the common one: it is what Binance
		// calls an IOC or FOK order that did not fill, and closing that gap needs
		// an OrderView verdict that does not exist yet (#924).
		return OrderView{
			State: OrderViewIndeterminate,
			Reason: fmt.Sprintf(
				"binance reports order %s as %s, which this platform has no reconciliation answer for "+
					"(executedQty %s). Resolve it against the exchange's own order history",
				st.GetOrderId(), resp.Status, resp.ExecutedQty),
		}, nil
	}
}

// tradesFor fetches the individual executions behind a filled or partially
// filled order.
//
// # Why the order response is not enough
//
// GET /api/v3/order reports executedQty and cummulativeQuoteQty and NO trade
// list — the inline `fills` array exists only on the POST that placed the order.
// Synthesizing one aggregate fill from those two numbers was the obvious
// shortcut and it is unsafe: a fill's identity is what the order aggregate and
// the position book dedup on, the live path mints "<symbol>-<tradeId>" per
// trade (here and in the user-data stream), and a synthetic
// "<symbol>-<orderId>" is a DIFFERENT id for the same execution. Adopting it
// beside fills the websocket already delivered double-counts the trade.
//
// So the real trades are fetched, and the fills are built by the SAME v.fills
// used on the placement path — one construction, so the ids cannot drift apart.
func (v *BinanceVenue) tradesFor(ctx context.Context, st *orderpb.OrderState, symbol string, resp *orderResponse) ([]*orderpb.Fill, error) {
	if resp.OrderID == 0 {
		// myTrades addresses trades by the EXCHANGE's numeric order id; there is
		// no client-order-id form of it. Without one there is nothing to ask.
		return nil, nil
	}
	trades, err := v.rest.myTrades(ctx, symbol, resp.OrderID)
	if err != nil {
		// Including ErrRateLimited: the question could not be finished, and a
		// half-answered fill is worse than no answer.
		return nil, err
	}
	if len(trades) == 0 {
		return nil, nil
	}
	// Rebuilt in the placement response's shape so v.fills is the ONE place a
	// Binance fill is constructed. Its fill_id is fmt.Sprintf("%s-%d", Symbol,
	// TradeID) — byte for byte what the placement path and the user-data stream
	// produce for the same trade, which is what makes re-adoption a no-op
	// instead of a double count.
	shaped := &orderResponse{Symbol: symbol, Fills: make([]orderFill, 0, len(trades))}
	for _, t := range trades {
		shaped.Fills = append(shaped.Fills, orderFill{
			Price:           t.Price,
			Qty:             t.Qty,
			Commission:      t.Commission,
			CommissionAsset: t.CommissionAsset,
			TradeID:         t.ID,
		})
	}
	fills, err := v.fills(shaped, st)
	if err != nil {
		return nil, err
	}
	// THE TRADE'S OWN INSTANT, NOT THIS ONE. v.fills stamps executed_at from the
	// connector clock, which is right on the placement path (the trade just
	// happened) and wrong here: this is the RECOVERY path, and an order adopted
	// an hour after it traded would be timestamped an hour late in every
	// execution-quality measurement that reads it.
	for i, t := range trades {
		if t.Time > 0 {
			fills[i].ExecutedAt = timestamppb.New(time.UnixMilli(t.Time).UTC())
		}
	}
	return fills, nil
}

var _ Querier = (*BinanceVenue)(nil)
