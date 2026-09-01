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
// # FILL IDENTITY IS WHAT MAKES A FILLED ANSWER ADOPTABLE (#923)
//
// This connector used to report WORKING and UNKNOWN and refuse to answer FILLED
// or PARTIALLY_FILLED, because its Execute synthesized ONE CUMULATIVE fill per
// order named "<instId>-<ordId>" while its own user-data stream named per-trade
// fills "<instId>-<tradeId>". Two names for one execution, in an id space the
// order aggregate (order_fills) and the position book (position_fills) dedup on,
// which meant a queried view could only ever under-record the trade or fold a
// second copy of it.
//
// Both paths now build fills from OKX's own trade records — one per trade,
// "<instId>-<tradeId>" — through the single OKXVenue.fills, so a view fetched
// here carries the ids the platform already holds and re-adopting it is a no-op
// rather than a double count. That is what authorizes the FILLED and
// PARTIALLY_FILLED answers below.
//
// # A WITHDRAWAL IS AN ANSWER TOO (#924)
//
// "canceled" and "mmp_canceled" used to freeze, because execution.OrderView had
// no withdrawn verdict. It has two now, and this connector answers CANCELLED for
// both — OKX has no expiry of its own, an unfilled IOC comes back "canceled"
// here where Binance calls the same thing EXPIRED. The withdrawal CARRIES
// WHATEVER TRADED BEFORE IT, from the same fills-history the FILLED path reads,
// because an order pulled after a partial execution is the common shape and a
// verdict that dropped those fills would record a traded order as untraded.
//
// WHAT STILL FREEZES: an order OKX calls filled — or withdrawn after a trade —
// while returning no trade for it. Reporting a verdict with nothing to fold
// would leave a traded order looking untraded (the OMS refuses such a view, and
// is right to); reporting UNKNOWN would re-place an order OKX has just said it
// executed. So it quarantines, and the reason names the venue's own state and
// filled size. So does a CONDITIONAL order in any terminal state — see
// queryAlgo, and #925.

import (
	"context"
	"errors"
	"fmt"
	"math/big"

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
	// THE MAPPING DECIDES WHETHER TO SPEND THE WEIGHT, because it is the thing
	// that knows which verdicts can carry fills.
	//
	// IT USED TO BE A SECOND PREDICATE HERE, gating the fetch beside the switch
	// that gated the verdict, and #924 is what made that shape untenable: a
	// withdrawn order may have partially traded before it was pulled, so a third
	// family of states now needs its trades too, and every state added to one
	// side and not the other produces a verdict with nothing fetched to carry.
	// Handing okxOrderView the fetch itself removes the second list entirely —
	// there is one switch, and the arm that answers is the arm that asks.
	//
	// A resting or unmappable order still costs one weight unit and not seven:
	// tradedFills asks nothing when OKX reports no filled size.
	//
	// A FAILURE TO FINISH READING THE TRADES IS AN ERROR, never a fill-less
	// FILLED and never an UNKNOWN that would re-place an order OKX has just said
	// it executed.
	return okxOrderView(st, o.State, o.AccFillSz, func() ([]*orderpb.Fill, error) {
		return v.tradedFills(ctx, o, st, instID)
	})
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
		//
		// A CONDITIONAL ORDER GETS NO WITHDRAWN VERDICT, DELIBERATELY (#924). The
		// regular path can answer one because it can also establish what traded
		// before the withdrawal; this path cannot. An algo order's executions
		// belong to the order its trigger CREATED, under a clOrdId of OKX's own
		// choosing, which is the same reason "effective" freezes above — so
		// answering CANCELLED here would rest on an assumption that a withdrawn
		// trigger never fired, and being wrong about that adopts a traded order as
		// one that traded nothing. The algo endpoint's own answers are not yet
		// established against a real account either (#925). Fail closed.
		return OrderView{
			State: OrderViewIndeterminate,
			Reason: fmt.Sprintf(
				"okx reports conditional order %s as %s, which this platform has no reconciliation "+
					"answer for. Resolve it against the exchange's own order history",
				st.GetOrderId(), algo.State),
		}, nil
	}
}

// okxStateTraded reports whether an OKX order state means the venue attributes
// EXECUTIONS to the order and the order is still live at it.
func okxStateTraded(state string) bool {
	return state == "partially_filled" || state == "filled"
}

// okxStateWithdrawn reports whether OKX has PULLED the order: "canceled", and
// "mmp_canceled" for one market-maker protection removed.
//
// IT IS DERIVED FROM okxStateToProto RATHER THAN LISTING THE STRINGS AGAIN
// (#924). That table is what the healing path off the private websocket already
// uses to name an OKX state, so reading the withdrawal set off it means the two
// paths cannot come to disagree about which states are terminal withdrawals, and
// a state taught to one is taught to both.
//
// OKX HAS NO EXPIRY OF ITS OWN HERE, and that is a venue difference rather than
// an omission: an unfilled IOC comes back "canceled" at OKX, where Binance calls
// the same thing EXPIRED. The connector reports what its venue says.
func okxStateWithdrawn(state string) bool {
	return okxStateToProto(state) == orderpb.OrderStatus_ORDER_STATUS_CANCELLED
}

// okxOrderView maps a regular order's OKX state onto a verdict, fetching the
// order's trades through fetch for the states that can carry them.
//
// IT TAKES THE FETCH RATHER THAN THE FILLS, so the arm that answers is the arm
// that asks. The alternative — a predicate at the call site deciding whether to
// fetch, and this switch deciding what to answer — is two lists of OKX states
// that must agree, and the direction that fails is a verdict carrying no fills
// because the caller's list had not learned the state. #924 added a third family
// of fill-carrying states, which is when keeping the two in step stopped being
// worth the second list.
//
// Split out from QueryOrder so a test can drive every OKX state without an HTTP
// server.
func okxOrderView(st *orderpb.OrderState, state, accFillSz string, fetch func() ([]*orderpb.Fill, error)) (OrderView, error) {
	switch {
	case state == "live":
		return OrderView{State: OrderViewWorking}, nil

	case okxStateTraded(state):
		fills, err := fetch()
		if err != nil {
			return OrderView{}, err
		}
		if len(fills) == 0 {
			// OKX CONTRADICTED ITSELF: it reports the order traded and produced no
			// trade record for it. Reporting FILLED with no fill would leave a
			// traded order looking untraded (the OMS refuses such a view, and is
			// right to); reporting UNKNOWN would re-place an order OKX has just
			// said it executed. Freeze it and say which.
			return OrderView{
				State: OrderViewIndeterminate,
				Reason: fmt.Sprintf(
					"okx reports order %s as %s having traded %s but returned no trade for it, so "+
						"this adapter cannot say what traded. Resolve it against the exchange's own "+
						"order history",
					st.GetOrderId(), state, accFillSz),
			}, nil
		}
		// THE VENUE'S OWN FILL IDENTITIES, byte for byte what the placement path
		// and the user-data stream mint for these trades — which is what makes
		// adopting this view a no-op for a fill the order already holds instead of
		// a second copy of it (#923).
		verdict := OrderViewPartiallyFilled
		if state == "filled" {
			verdict = OrderViewFilled
		}
		return OrderView{State: verdict, Fills: fills}, nil

	case okxStateWithdrawn(state):
		// OKX PULLED THE ORDER AND IT IS TERMINAL (#924). Before this verdict
		// existed these froze, because inventing one out of the other five was a
		// lie in whichever direction it was taken: REJECTED writes a refusal over
		// an order that may have partially traded before it was pulled, UNKNOWN
		// re-places it.
		//
		// THE FILLED SIZE IS READ BEFORE THE TRADES ARE ASKED FOR, because
		// tradedFills answers "no trades" for a size it cannot parse — which on
		// the traded arm above is caught by the empty-fills refusal, and here
		// would silently adopt a partially traded order as one that traded
		// nothing.
		traded, ok := new(big.Rat).SetString(accFillSz)
		if !ok && accFillSz != "" {
			return OrderView{
				State: OrderViewIndeterminate,
				Reason: fmt.Sprintf(
					"okx reports order %s as %s with an unreadable filled size %q, so this adapter "+
						"cannot say whether it traded before it was withdrawn",
					st.GetOrderId(), state, accFillSz),
			}, nil
		}
		if traded == nil || traded.Sign() <= 0 {
			// The ordinary case, and the population #924 was opened for: an order
			// withdrawn without trading. Nothing to fetch and nothing to carry.
			return OrderView{State: OrderViewCancelled}, nil
		}
		fills, err := fetch()
		if err != nil {
			// The question could not be finished. NEVER a fill-less withdrawal —
			// that adopts a partially traded order as one that traded nothing, and
			// a fill nobody recorded is worse than the freeze this replaces.
			return OrderView{}, err
		}
		if len(fills) == 0 {
			return OrderView{
				State: OrderViewIndeterminate,
				Reason: fmt.Sprintf(
					"okx reports order %s as %s having traded %s before it was withdrawn, but returned "+
						"no trade for it, so this adapter cannot say what traded. Resolve it against "+
						"the exchange's own order history",
					st.GetOrderId(), state, accFillSz),
			}, nil
		}
		return OrderView{State: OrderViewCancelled, Fills: fills}, nil

	default:
		// Anything OKX adds later, and anything okxStateToProto has not been
		// taught. An answer this connector cannot map is not an answer.
		return OrderView{
			State: OrderViewIndeterminate,
			Reason: fmt.Sprintf(
				"okx reports order %s as %s (filled %s), which this platform has no reconciliation "+
					"answer for. Resolve it against the exchange's own order history",
				st.GetOrderId(), state, accFillSz),
		}, nil
	}
}

var _ Querier = (*OKXVenue)(nil)
