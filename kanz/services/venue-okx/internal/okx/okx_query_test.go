package okx

// OKXVenue as an execution.Querier (#920), against the hermetic REST fake — no
// network, no keys, no exchange.
//
// WHAT THESE TESTS CANNOT PROVE, STATED FIRST. The fake serves the response
// shapes OKX's documentation describes; nothing here has been run against the
// real exchange, so the CODES are asserted, not verified. The real-venue leg is
// #70/#72's territory. What IS proven is the thing that costs money if it is
// wrong: which inputs may produce OrderViewUnknown, and which may not.

import (
	"context"
	"errors"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/venueadapter/exchangeauth"
)

func okxLimit(id string) *orderpb.OrderState {
	return &orderpb.OrderState{
		OrderId: id, InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
		OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT, OrderedQuantity: odec("1"),
		LimitPrice: odec("50000"),
	}
}

func okxStop(id string) *orderpb.OrderState {
	return &orderpb.OrderState{
		OrderId: id, InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_SELL,
		OrderType: orderpb.OrderType_ORDER_TYPE_STOP, OrderedQuantity: odec("1"),
		StopPrice: odec("49000"),
	}
}

// A resting order is WORKING, and order.Reconcile LEAVEs it alone. Before this
// RPC the OMS froze it instead.
func TestOKXQuery_LiveIsWorking(t *testing.T) {
	f := newFakeOKX(t)
	f.queryBody = `{"code":"0","msg":"","data":[{"ordId":"1","clOrdId":"o1","state":"live","sz":"1","accFillSz":"0"}]}`

	view, err := okxVenueOver(f).QueryOrder(context.Background(), okxLimit("o1"))
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if view.State != OrderViewWorking {
		t.Fatalf("state = %v, want WORKING", view.State)
	}
	if f.sawClOrd != "o1" {
		t.Fatalf("asked about clOrdId %q, want o1 — the deterministic id the submit stamped", f.sawClOrd)
	}
}

// 51603 "Order does not exist" ON THE REGULAR ENDPOINT, FOR A REGULAR ORDER, IS
// THE ONE AFFIRMATIVE MISS. It is the only input in this file that may produce
// UNKNOWN, and UNKNOWN is what lets the OMS place the order.
func TestOKXQuery_OrderDoesNotExistIsUnknown(t *testing.T) {
	f := newFakeOKX(t)
	f.queryBody = `{"code":"51603","msg":"Order does not exist","data":[]}`

	view, err := okxVenueOver(f).QueryOrder(context.Background(), okxLimit("o1"))
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if view.State != OrderViewUnknown {
		t.Fatalf("state = %v, want UNKNOWN — an order OKX has never held is the one case the "+
			"OMS may re-drive", view.State)
	}
}

// A TRANSPORT FAULT IS NOT AN ANSWER, AND MUST NEVER BE UNKNOWN.
//
// This is the duplicate-order test at the connector: a closed server is what an
// OKX outage or a DNS failure looks like from here.
func TestOKXQuery_TransportFailureIsAnErrorNeverUnknown(t *testing.T) {
	f := newFakeOKX(t)
	v := okxVenueOver(f)
	f.srv.Close() // the exchange is gone

	view, err := v.QueryOrder(context.Background(), okxLimit("o1"))
	if err == nil {
		t.Fatal("an unreachable OKX produced no error — the OMS would act on a verdict nobody gave")
	}
	if view.State == OrderViewUnknown {
		t.Fatal("an unreachable OKX was reported as OrderViewUnknown, which order.Reconcile turns " +
			"into a RE-DRIVE: an outage would place every interrupted order a second time")
	}
}

// AN EXHAUSTED WEIGHT BUDGET IS NOT AN ANSWER EITHER. The connector never fired
// a request; OKX said nothing. The fake proves it: no GET reached it.
func TestOKXQuery_RateLimitedIsAnErrorNeverUnknown(t *testing.T) {
	f := newFakeOKX(t)
	bucket := NewWeightBucket(0, 2*time.Second, nil) // nothing is affordable
	rest := newOKXREST(okxRestConfig{BaseURL: f.srv.URL, APIKey: "k", APISecret: "s", Passphrase: "p", Bucket: bucket, Mode: exchangeauth.OKXDemo})
	v := &OKXVenue{mic: "OKX", rest: rest, symbols: StaticSymbolMap{"BTC-USD": "BTC-USDT"}, now: time.Now}

	view, err := v.QueryOrder(context.Background(), okxLimit("o1"))
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited so the caller backs off", err)
	}
	if view.State == OrderViewUnknown {
		t.Fatal("an exhausted weight budget was reported as OrderViewUnknown — the platform would " +
			"read its OWN back-pressure as OKX denying the order, and re-place it")
	}
	if f.gets != 0 {
		t.Fatalf("%d requests reached OKX for a refused query", f.gets)
	}
}

// A FILLED OR PARTIALLY FILLED ORDER IS ADOPTED WITH OKX'S OWN FILL IDENTITIES
// (#923).
//
// This is the answer the connector used to refuse. It refused because Execute
// synthesized ONE CUMULATIVE fill per order named "<instId>-<ordId>", a
// different identifier space from the "<instId>-<tradeId>" its user-data stream
// mints — so a queried view could only under-record the trade or fold a second
// copy of it. Both paths now build from OKX's own trade records through one
// function, so the ids here are the ids the platform already holds.
func TestOKXQuery_FilledCarriesTheVenuesOwnFillIdentities(t *testing.T) {
	for _, tc := range []struct {
		state string
		want  OrderViewState
	}{
		{"filled", OrderViewFilled},
		{"partially_filled", OrderViewPartiallyFilled},
	} {
		f := newFakeOKX(t)
		f.queryBody = `{"code":"0","msg":"","data":[{"ordId":"1","clOrdId":"o1","state":"` + tc.state +
			`","sz":"1","accFillSz":"0.6","avgPx":"50000"}]}`
		f.fillsBody = `{"code":"0","msg":"","data":[` +
			`{"instId":"BTC-USDT","tradeId":"42","ordId":"1","clOrdId":"o1","fillPx":"50000","fillSz":"0.2","ts":"1750000000000"},` +
			`{"instId":"BTC-USDT","tradeId":"43","ordId":"1","clOrdId":"o1","fillPx":"50010","fillSz":"0.4","ts":"1750000001000"}]}`

		view, err := okxVenueOver(f).QueryOrder(context.Background(), okxLimit("o1"))
		if err != nil {
			t.Fatalf("%s: QueryOrder: %v", tc.state, err)
		}
		if view.State != tc.want {
			t.Fatalf("%s: state = %v, want %v", tc.state, view.State, tc.want)
		}
		if len(view.Fills) != 2 {
			t.Fatalf("%s: fills = %d, want 2 — one per trade, not one aggregate", tc.state, len(view.Fills))
		}
		// THE IDENTITIES. The same fill_id okx_userdata.go mints for a live push
		// and OKXVenue.fills mints for a placement; a change to one without the
		// others double-counts a trade.
		if view.Fills[0].GetFillId() != "BTC-USDT-42" || view.Fills[1].GetFillId() != "BTC-USDT-43" {
			t.Fatalf("%s: fill ids = %q, %q; want BTC-USDT-42 and BTC-USDT-43", tc.state,
				view.Fills[0].GetFillId(), view.Fills[1].GetFillId())
		}
		// ADDRESSED BY OKX'S OWN ordId — fills-history has no clOrdId form.
		if f.sawFillsOrdID != "1" {
			t.Fatalf("%s: fills-history asked about ordId %q, want 1", tc.state, f.sawFillsOrdID)
		}
		// THE TRADE'S OWN INSTANT, not the recovery's. An order adopted an hour
		// after it traded must not be timestamped an hour late in every
		// execution-quality measurement that reads it.
		if got := view.Fills[0].GetExecutedAt().AsTime(); !got.Equal(time.UnixMilli(1750000000000).UTC()) {
			t.Fatalf("%s: executed_at = %s, want the exchange's own trade time", tc.state, got)
		}
	}
}

// OKX CONTRADICTING ITSELF FREEZES THE ORDER.
//
// It reports the order traded and returns no trade for it. Reporting FILLED with
// no fill leaves a traded order looking untraded — the OMS quarantines such a
// view and is right to; reporting UNKNOWN re-places an order OKX has just said
// it executed. Neither is a guess this platform makes.
func TestOKXQuery_FilledWithNoTradeIsIndeterminateNotUnknown(t *testing.T) {
	for _, state := range []string{"filled", "partially_filled"} {
		f := newFakeOKX(t)
		f.queryBody = `{"code":"0","msg":"","data":[{"ordId":"1","clOrdId":"o1","state":"` + state +
			`","sz":"1","accFillSz":"0.6","avgPx":"50000"}]}`
		// fillsBody keeps its default: code 0, empty data.

		view, err := okxVenueOver(f).QueryOrder(context.Background(), okxLimit("o1"))
		if err != nil {
			t.Fatalf("%s: QueryOrder: %v", state, err)
		}
		if view.State != OrderViewIndeterminate {
			t.Fatalf("%s: state = %v, want INDETERMINATE", state, view.State)
		}
		if view.Reason == "" {
			t.Fatalf("%s: no reason given — an operator reading the quarantine learns nothing", state)
		}
	}
}

// A FAILURE FETCHING THE TRADES IS A FAILURE, not a fill-less FILLED and not an
// UNKNOWN. The order query succeeded; the answer is still incomplete, and a
// half-read set of fills is worse than no answer.
func TestOKXQuery_TradeFetchFailureIsAnErrorNeverUnknown(t *testing.T) {
	f := newFakeOKX(t)
	f.queryBody = `{"code":"0","msg":"","data":[{"ordId":"1","clOrdId":"o1","state":"filled","sz":"1","accFillSz":"1","avgPx":"50000"}]}`
	f.fillsBody = `{"code":"50011","msg":"Requests too frequent","data":[]}`

	view, err := okxVenueOver(f).QueryOrder(context.Background(), okxLimit("o1"))
	if err == nil {
		t.Fatal("a refused fills-history produced no error")
	}
	if view.State == OrderViewUnknown || view.State == OrderViewFilled {
		t.Fatalf("state = %v; a half-fetched answer must be neither a re-drive nor an adoption", view.State)
	}
}

// A RESTING OR WITHDRAWN ORDER COSTS ONE REQUEST, NOT SEVEN. fills-history's own
// rate limit is six times tighter than /trade/order's, so asking it about every
// order — including the ones OKX says traded nothing — would spend the placement
// path's headroom on nothing.
func TestOKXQuery_NoTradesFetchedForAnOrderThatDidNotTrade(t *testing.T) {
	for _, state := range []string{"live", "canceled"} {
		f := newFakeOKX(t)
		f.queryBody = `{"code":"0","msg":"","data":[{"ordId":"1","clOrdId":"o1","state":"` + state +
			`","sz":"1","accFillSz":"0"}]}`

		if _, err := okxVenueOver(f).QueryOrder(context.Background(), okxLimit("o1")); err != nil {
			t.Fatalf("%s: QueryOrder: %v", state, err)
		}
		if f.fillsGets != 0 {
			t.Fatalf("%s: %d fills-history requests fired for an order that traded nothing",
				state, f.fillsGets)
		}
	}
}

// A WITHDRAWN ORDER FREEZES. execution.OrderView has no withdrawn answer, and
// inventing one would be a lie in whichever direction it was taken.
func TestOKXQuery_CancelledFreezesRatherThanGuessing(t *testing.T) {
	for _, state := range []string{"canceled", "mmp_canceled", "something_new"} {
		f := newFakeOKX(t)
		f.queryBody = `{"code":"0","msg":"","data":[{"ordId":"1","clOrdId":"o1","state":"` + state +
			`","sz":"1","accFillSz":"0"}]}`

		view, err := okxVenueOver(f).QueryOrder(context.Background(), okxLimit("o1"))
		if err != nil {
			t.Fatalf("%s: QueryOrder: %v", state, err)
		}
		if view.State != OrderViewIndeterminate {
			t.Fatalf("%s: state = %v, want INDETERMINATE", state, view.State)
		}
	}
}

// A CONDITIONAL ORDER IS ASKED ABOUT ON THE ALGO ENDPOINT, NEVER THE REGULAR
// ONE (#485).
//
// This is the trap this file exists for. /trade/order answers 51603 "Order does
// not exist" for a stop that is RESTING AND HEALTHY. Routing a stop there and
// reading 51603 as UNKNOWN would place a second stop at OKX for every
// interrupted one — the exact duplicate this whole error discipline is about.
func TestOKXQuery_AStopIsAskedOnTheAlgoEndpoint(t *testing.T) {
	f := newFakeOKX(t)
	// The regular endpoint would say 51603 for this resting stop.
	f.queryBody = `{"code":"51603","msg":"Order does not exist","data":[]}`
	f.algoQueryBody = `{"code":"0","msg":"","data":[{"algoId":"7","algoClOrdId":"o1","state":"live","sz":"1"}]}`

	view, err := okxVenueOver(f).QueryOrder(context.Background(), okxStop("o1"))
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if f.gets != 0 {
		t.Fatalf("a conditional order was asked about on /trade/order (%d GETs). That endpoint "+
			"answers 51603 for a resting stop, and reading it as UNKNOWN places a second one", f.gets)
	}
	if f.algoGets != 1 || f.sawAlgoClOrd != "o1" {
		t.Fatalf("algo endpoint calls = %d for algoClOrdId %q, want 1 for o1", f.algoGets, f.sawAlgoClOrd)
	}
	if view.State != OrderViewWorking {
		t.Fatalf("state = %v, want WORKING — OKX holds this stop and it has not fired", view.State)
	}
}

// A PAUSED STOP IS ALSO WORKING: OKX holds it.
func TestOKXQuery_APausedStopIsWorking(t *testing.T) {
	f := newFakeOKX(t)
	f.algoQueryBody = `{"code":"0","msg":"","data":[{"algoId":"7","algoClOrdId":"o1","state":"pause","sz":"1"}]}`

	view, err := okxVenueOver(f).QueryOrder(context.Background(), okxStop("o1"))
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if view.State != OrderViewWorking {
		t.Fatalf("state = %v, want WORKING", view.State)
	}
}

// AN ALGO LOOKUP THAT FAILS IS NEVER "THE VENUE NEVER HELD IT".
//
// queryAlgoOrder reports an empty data array as APIError{Code: 0}, and the algo
// endpoint's not-found encoding has never been established against the venue. A
// wrong guess here re-places a stop OKX is holding, so the answer is
// INDETERMINATE — the quarantine a conditional order got before this RPC
// existed. Retiring it needs a read-only probe against a real account (#925).
func TestOKXQuery_AnAlgoLookupFailureIsNeverUnknown(t *testing.T) {
	for _, body := range []string{
		`{"code":"51603","msg":"Order does not exist","data":[]}`,
		`{"code":"0","msg":"","data":[]}`,
		`{"code":"50011","msg":"rate limited","data":[]}`,
	} {
		f := newFakeOKX(t)
		f.algoQueryBody = body

		view, err := okxVenueOver(f).QueryOrder(context.Background(), okxStop("o1"))
		if err != nil {
			t.Fatalf("%s: QueryOrder: %v", body, err)
		}
		if view.State == OrderViewUnknown {
			t.Fatalf("%s: an unreadable algo lookup was reported as UNKNOWN — order.Reconcile "+
				"would place a second stop at OKX", body)
		}
		if view.State != OrderViewIndeterminate {
			t.Fatalf("%s: state = %v, want INDETERMINATE", body, view.State)
		}
	}
}

// A TRIGGERED STOP FREEZES. The order OKX created when the trigger fired carries
// a clOrdId of OKX's own choosing, and its fills reach this platform through the
// user-data stream keyed on algoClOrdId — not from here.
func TestOKXQuery_ATriggeredStopIsIndeterminate(t *testing.T) {
	f := newFakeOKX(t)
	f.algoQueryBody = `{"code":"0","msg":"","data":[{"algoId":"7","algoClOrdId":"o1","state":"effective","sz":"1"}]}`

	view, err := okxVenueOver(f).QueryOrder(context.Background(), okxStop("o1"))
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if view.State != OrderViewIndeterminate {
		t.Fatalf("state = %v, want INDETERMINATE", view.State)
	}
	if view.Reason == "" {
		t.Fatal("no reason given")
	}
}

// AN UNMAPPED INSTRUMENT IS A VERDICT, NOT AN ERROR: /trade/order refuses
// without an instId (50014), and asking again in a minute changes nothing.
func TestOKXQuery_UnmappedInstrumentIsIndeterminate(t *testing.T) {
	f := newFakeOKX(t)
	st := okxLimit("o1")
	st.InstrumentId = "DOGE-USD"

	view, err := okxVenueOver(f).QueryOrder(context.Background(), st)
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if view.State != OrderViewIndeterminate {
		t.Fatalf("state = %v, want INDETERMINATE", view.State)
	}
	if f.gets != 0 {
		t.Fatalf("%d requests reached OKX for an instrument this deployment cannot address", f.gets)
	}
}
