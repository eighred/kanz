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
//
// # A WITHDRAWAL IS AN ANSWER TOO (#924)
//
// "CANCELED" and "EXPIRED" used to freeze, because execution.OrderView had no
// withdrawn verdict. It has two now, and EXPIRED is the one that mattered: it is
// what Binance calls an IOC or FOK order that did not fill, so it is the
// ORDINARY terminal state of a time-in-force this platform declares supported
// (#486), and every interrupted one froze for a human on a complete answer.
//
// The withdrawal CARRIES WHATEVER TRADED BEFORE IT, from the same myTrades the
// FILLED path reads — an order pulled after a partial execution is the common
// shape, and a verdict that dropped those fills would record a traded order as
// untraded, which is worse than the freeze it replaces because it is silent.

import (
	"context"
	"errors"
	"fmt"
	"math/big"
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
		// A TERMINAL WITHDRAWAL, OR SOMETHING THIS CONNECTOR STILL WILL NOT GUESS
		// ABOUT (#924).
		//
		// WHICH STATUSES ARE WITHDRAWALS IS NOT DECIDED HERE. It is read off
		// binanceStatusToProto — the table the healing path has always used to
		// turn a Binance status into an order.v1 status — so the two paths cannot
		// come to disagree about what "CANCELED" means. A second string table in
		// this file is exactly how the query path would learn a status the
		// healing path did not, or answer a different terminal state for one they
		// both know.
		switch binanceStatusToProto(resp.Status) {
		case orderpb.OrderStatus_ORDER_STATUS_CANCELLED, orderpb.OrderStatus_ORDER_STATUS_EXPIRED:
			return v.withdrawnView(ctx, st, symbol, resp)
		}
		// "PENDING_CANCEL", "EXPIRED_IN_MATCH", and anything Binance adds later.
		//
		// PENDING_CANCEL IS DELIBERATELY NOT A WITHDRAWAL. It says a cancel is IN
		// FLIGHT, not that the order is gone: Binance may still fill it. Writing a
		// terminal withdrawal over a live exchange order is the same class of
		// error as re-placing one, and binanceStatusToProto has never claimed
		// otherwise — it answers UNSPECIFIED for it, which is what routes it here.
		//
		// EXPIRED_IN_MATCH (self-trade prevention) is a genuine terminal
		// withdrawal and it freezes ANYWAY, for the same reason: this connector's
		// one status table does not know it, and teaching only the query path
		// would give the platform two answers for one Binance status. It is a
		// smaller and rarer population than the IOC/FOK expiries #924 was opened
		// for, and it is fixed by teaching binanceStatusToProto, at which point
		// this arm picks it up with no change here.
		return OrderView{
			State: OrderViewIndeterminate,
			Reason: fmt.Sprintf(
				"binance reports order %s as %s, which this platform has no reconciliation answer for "+
					"(executedQty %s). Resolve it against the exchange's own order history",
				st.GetOrderId(), resp.Status, resp.ExecutedQty),
		}, nil
	}
}

// withdrawnView answers for an order Binance WITHDREW — "CANCELED", or the
// "EXPIRED" that is the ordinary terminal state of an unfilled IOC or FOK
// (#924).
//
// IT CARRIES THE FILLS THAT TRADED BEFORE THE WITHDRAWAL. That is the whole
// hazard of this verdict: an order pulled after a partial execution is the
// common shape, and adopting one without its fills would record a traded order
// as untraded — silently, and worse than the quarantine this replaces. The
// trades come from the SAME tradesFor the FILLED path uses, so they carry
// Binance's own "<symbol>-<tradeId>" identities and re-adopting the view is a
// no-op rather than a double count.
//
// AN UNTRADED WITHDRAWAL COSTS NO EXTRA WEIGHT. executedQty is zero for the
// population this exists for — an IOC that did not fill — and myTrades is not
// called at all for those.
func (v *BinanceVenue) withdrawnView(ctx context.Context, st *orderpb.OrderState, symbol string, resp *orderResponse) (OrderView, error) {
	state := OrderViewCancelled
	if binanceStatusToProto(resp.Status) == orderpb.OrderStatus_ORDER_STATUS_EXPIRED {
		state = OrderViewExpired
	}
	traded, ok := new(big.Rat).SetString(resp.ExecutedQty)
	if !ok && resp.ExecutedQty != "" {
		// BINANCE SENT A QUANTITY THIS CONNECTOR CANNOT READ, so it cannot
		// establish whether the order traded before it was pulled. Answering the
		// withdrawal now would adopt it as untraded on a field nobody parsed.
		return OrderView{
			State: OrderViewIndeterminate,
			Reason: fmt.Sprintf(
				"binance reports order %s as %s with an unreadable executedQty %q, so this adapter "+
					"cannot say whether it traded before it was withdrawn",
				st.GetOrderId(), resp.Status, resp.ExecutedQty),
		}, nil
	}
	if traded == nil || traded.Sign() <= 0 {
		return OrderView{State: state}, nil
	}
	fills, err := v.tradesFor(ctx, st, symbol, resp)
	if err != nil {
		// The question could not be finished. NEVER a fill-less withdrawal: that
		// would adopt a partially traded order as one that traded nothing.
		return OrderView{}, err
	}
	if len(fills) == 0 {
		// BINANCE CONTRADICTED ITSELF: it reports a traded quantity and returned
		// no trade for it. Same reasoning as the FILLED arm above — freeze, and
		// say which.
		return OrderView{
			State: OrderViewIndeterminate,
			Reason: fmt.Sprintf(
				"binance reports order %s as %s having traded %s before it was withdrawn, but returned "+
					"no trade for it, so this adapter cannot say what traded",
				st.GetOrderId(), resp.Status, resp.ExecutedQty),
		}, nil
	}
	return OrderView{State: state, Fills: fills}, nil
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
