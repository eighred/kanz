package okx

// The Querier half of the venue contract (#920): what did OKX do with this
// order?
//
// Same shape and same discipline as the Binance connector's binance_query.go,
// and the same reason for existing: until venue.v1 carried a query RPC, SimVenue
// was the only execution.Querier in the platform, so the OMS quarantined every
// interrupted ROUTED order at every real venue.
//
// # THE ONE MISTAKE THIS FILE EXISTS TO NOT MAKE
//
// OrderViewUnknown is an AFFIRMATIVE statement — OKX says it has no such order —
// and order.Reconcile turns it into ActionRedrive, which places the order at a
// real exchange. Only OKX's own 51603 "Order does not exist", ON THE ENDPOINT
// THAT ORDER ACTUALLY LIVES ON, may produce it. A timeout, a 5xx, an egress
// denial and an exhausted weight budget are ERRORS ("the question could not be
// asked"); anything unmappable is INDETERMINATE, which quarantines.
//
// # WHAT THIS CONNECTOR DELIBERATELY WILL NOT ANSWER, AND WHY
//
// It reports WORKING and UNKNOWN, and it refuses to report FILLED or
// PARTIALLY_FILLED — those answer INDETERMINATE, which is exactly the quarantine
// the OMS performed before this RPC existed, so no order is worse off.
//
// THE REASON IS FILL IDENTITY, WHICH IS THE THING THE ORDER AGGREGATE DEDUPS ON.
// This connector's Execute reports ONE CUMULATIVE fill per order, built from
// accFillSz and avgPx and named "<instId>-<ordId>" (OKXVenue.fills). That name is
// stable while the quantity behind it is not: an order that was 40% done when
// its first fill was folded comes back from a later query as the SAME fill_id
// carrying 100. The OMS skips a fill_id it already holds — correctly, that is
// what stops a double count — so adopting this view would silently leave a
// fully-traded order recorded as 40% filled, with nothing able to notice. The
// other direction is worse: OKX's per-trade fills (and its own user-data stream)
// are named "<instId>-<tradeId>", a DIFFERENT id space from Execute's, so
// fetching them here would fold a second copy of a trade the platform already
// has.
//
// That id disagreement is a real defect and it is not this change's to fix
// (#923). Reporting a fill state whose fills cannot be trusted would be, so this
// connector says so instead of guessing, and the quarantine record names the
// venue's own state and filled size for the operator resolving it.

import (
	"context"
	"errors"
	"fmt"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// QueryOrder reports what OKX did with st, addressed by the deterministic
// clOrdId the submit stamped.
//
// It satisfies execution.Querier, which the venue adapter's gRPC face serves to
// the OMS. Safe to call repeatedly: it places nothing and mutates nothing.
func (v *OKXVenue) QueryOrder(ctx context.Context, st *orderpb.OrderState) (OrderView, error) {
	if st == nil {
		return OrderView{}, errors.New("okx: nil order state")
	}
	if isAlgoOrder(st.GetOrderType()) {
		return v.queryAlgo(ctx, st)
	}
	instID, ok := v.symbols.Symbol(st.GetInstrumentId())
	if !ok {
		// PERMANENT, so it is a verdict rather than an error: /trade/order refuses
		// without an instId (50014), and asking again in a minute changes nothing.
		return OrderView{
			State: OrderViewIndeterminate,
			Reason: fmt.Sprintf(
				"okx: no symbol mapping for %s, so this adapter cannot ask the exchange about order %s",
				st.GetInstrumentId(), st.GetOrderId()),
		}, nil
	}

	o, err := v.rest.queryOrder(ctx, instID, st.GetOrderId())
	if err != nil {
		// THE RATE-LIMIT CHECK IS FIRST AND IS NOT AN APIError. An exhausted
		// weight budget is this connector declining to fire a request; OKX said
		// nothing, so there is nothing to read as a verdict.
		if errors.Is(err, ErrRateLimited) {
			return OrderView{}, err
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Code == okxOrderDoesNotExist {
			// OKX ANSWERED, on the endpoint a regular order lives on, that it has
			// never held this clOrdId. The one branch that authorizes a re-drive.
			//
			// THE SAME CODE FROM THE SAME ENDPOINT MEANS SOMETHING ELSE ENTIRELY
			// FOR A CONDITIONAL ORDER — a resting stop is invisible to
			// /trade/order and answers 51603 while perfectly healthy (#485). That
			// is why an algo order never reaches this line: it is dispatched to
			// queryAlgo above, before a symbol is even looked up.
			return OrderView{State: OrderViewUnknown}, nil
		}
		return OrderView{}, err
	}
	return okxOrderView(st, o.State, o.AccFillSz), nil
}

// queryAlgo answers for a CONDITIONAL order, which lives on a different endpoint
// and has its own state vocabulary (#485).
//
// NO SYMBOL IS NEEDED: /trade/order-algo is addressed by algoClOrdId alone, so
// this path cannot fail for the one reason the regular path can.
func (v *OKXVenue) queryAlgo(ctx context.Context, st *orderpb.OrderState) (OrderView, error) {
	algo, err := v.rest.queryAlgoOrder(ctx, st.GetOrderId())
	if err != nil {
		if errors.Is(err, ErrRateLimited) {
			return OrderView{}, err
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			// OKX ANSWERED AND THIS CONNECTOR WILL NOT READ THE ANSWER AS "NEVER
			// PLACED". The algo endpoint's not-found encoding has never been
			// established against the venue — queryAlgoOrder reports an empty data
			// array as APIError{Code: 0} — and the cost of guessing wrong here is
			// re-placing a stop OKX is holding. INDETERMINATE quarantines, which
			// is what a conditional order got before this RPC existed. Retiring
			// this needs a read-only probe against a real account (#925).
			return OrderView{
				State: OrderViewIndeterminate,
				Reason: fmt.Sprintf(
					"okx could not be read about conditional order %s (%v), and this adapter will not "+
						"read that as the exchange never having held it", st.GetOrderId(), apiErr),
			}, nil
		}
		// A transport fault: the question could not be asked at all.
		return OrderView{}, err
	}
	switch algo.State {
	case "live", "pause":
		// The trigger is resting and has not fired. The venue holds it.
		return OrderView{State: OrderViewWorking}, nil
	case "effective":
		// It FIRED, and the order it created is the thing that matters now. That
		// order carries a clOrdId of OKX's own choosing, so establishing what it
		// did means walking order history — and its fills reach this platform
		// through the user-data stream keyed on algoClOrdId, not from here.
		return OrderView{
			State: OrderViewIndeterminate,
			Reason: fmt.Sprintf(
				"okx reports conditional order %s as TRIGGERED; the order it created is what the "+
					"exchange now holds, and this adapter cannot attribute its fills back to this id",
				st.GetOrderId()),
		}, nil
	default:
		// "canceled", "order_failed", and anything OKX adds later.
		return OrderView{
			State: OrderViewIndeterminate,
			Reason: fmt.Sprintf(
				"okx reports conditional order %s as %s, which this platform has no reconciliation "+
					"answer for. Resolve it against the exchange's own order history",
				st.GetOrderId(), algo.State),
		}, nil
	}
}

// okxOrderView maps a regular order's OKX state onto a verdict.
//
// Split out so the mapping is one function with one table rather than a switch
// buried in a call path, and so a test can drive every OKX state without an HTTP
// server.
func okxOrderView(st *orderpb.OrderState, state, accFillSz string) OrderView {
	switch state {
	case "live":
		return OrderView{State: OrderViewWorking}
	case "partially_filled", "filled":
		// SEE THIS FILE'S HEADER. The venue's truth is known and this connector
		// cannot express it as fills whose identities the order aggregate can
		// reconcile, so it says that rather than adopting a fill it would have to
		// invent a name for.
		return OrderView{
			State: OrderViewIndeterminate,
			Reason: fmt.Sprintf(
				"okx reports order %s as %s having traded %s, but this adapter reports one cumulative "+
					"fill per order whose identity cannot be reconciled with what the order already "+
					"holds, so adopting it could under-record or double-count the trade. Resolve it "+
					"against the exchange's own order history",
				st.GetOrderId(), state, accFillSz),
		}
	default:
		// "canceled", "mmp_canceled", and anything OKX adds later.
		//
		// execution.OrderView HAS NO WITHDRAWN ANSWER, and inventing one would be
		// a lie in whichever direction it was taken: REJECTED writes a refusal
		// over an order that may have partially traded before it was pulled,
		// UNKNOWN re-places it. So these freeze — which is what they did before
		// this RPC existed. Closing the gap needs an OrderView verdict that does
		// not exist yet (#924).
		return OrderView{
			State: OrderViewIndeterminate,
			Reason: fmt.Sprintf(
				"okx reports order %s as %s (filled %s), which this platform has no reconciliation "+
					"answer for. Resolve it against the exchange's own order history",
				st.GetOrderId(), state, accFillSz),
		}
	}
}

var _ Querier = (*OKXVenue)(nil)
