package binance

// BinanceVenue as an execution.Querier (#920), against the hermetic REST fake —
// no network, no keys, no exchange.
//
// WHAT THESE TESTS CANNOT PROVE, STATED FIRST. The fake serves the response
// shapes Binance's documentation describes; nothing here has been run against
// the real exchange, so the CODES are asserted, not verified. The real-venue leg
// is #70/#72's territory. What IS proven is the thing that costs money if it is
// wrong: which inputs may produce OrderViewUnknown, and which may not.

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
)

func queryOrderState(id string) *orderpb.OrderState {
	return &orderpb.OrderState{
		OrderId: id, InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
		OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT, OrderedQuantity: bdec("1"),
	}
}

// A resting order is WORKING, and order.Reconcile LEAVEs it alone. Before this
// RPC the OMS froze it instead.
func TestBinanceQuery_NewIsWorking(t *testing.T) {
	f := newFakeBinance(t)
	f.queryBody = `{"symbol":"BTCUSDT","orderId":1,"clientOrderId":"o1","status":"NEW","executedQty":"0"}`

	view, err := venueOverFake(f).QueryOrder(context.Background(), queryOrderState("o1"))
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if view.State != OrderViewWorking {
		t.Fatalf("state = %v, want WORKING", view.State)
	}
	if f.myTradesCalls != 0 {
		t.Fatalf("myTrades was called %d times for an order that has not traded — that is 20 "+
			"weight spent on nothing, on the recovery path", f.myTradesCalls)
	}
}

// -2013 "Order does not exist." IS THE ONE AFFIRMATIVE MISS. Binance answered,
// and its answer is that it has never held this client order id. This is the
// only input in this file that may produce UNKNOWN, and UNKNOWN is what lets the
// OMS place the order.
func TestBinanceQuery_OrderDoesNotExistIsUnknown(t *testing.T) {
	f := newFakeBinance(t)
	f.queryStatus = 400
	f.queryBody = `{"code":-2013,"msg":"Order does not exist."}`

	view, err := venueOverFake(f).QueryOrder(context.Background(), queryOrderState("o1"))
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if view.State != OrderViewUnknown {
		t.Fatalf("state = %v, want UNKNOWN — an order Binance has never held is the one case "+
			"the OMS may re-drive, and freezing it means every interrupted order needs a human", view.State)
	}
}

// A 5xx IS NOT AN ANSWER, AND MUST NEVER BE UNKNOWN.
//
// This is the duplicate-order test at the connector. If a Binance outage came
// back as UNKNOWN, order.Reconcile would re-drive every interrupted order the
// exchange is in fact holding.
func TestBinanceQuery_ServerErrorIsAnErrorNeverUnknown(t *testing.T) {
	f := newFakeBinance(t)
	f.queryStatus = 503
	f.queryBody = `service unavailable`

	view, err := venueOverFake(f).QueryOrder(context.Background(), queryOrderState("o1"))
	if err == nil {
		t.Fatal("a 503 from Binance produced no error — the OMS would act on a verdict nobody gave")
	}
	if view.State == OrderViewUnknown {
		t.Fatal("a 503 from Binance was reported as OrderViewUnknown, which order.Reconcile turns " +
			"into a RE-DRIVE: an exchange outage would place every interrupted order a second time")
	}
}

// AN EXHAUSTED WEIGHT BUDGET IS NOT AN ANSWER EITHER. The connector never fired
// a request; Binance said nothing. The fake proves it: no GET reached it.
func TestBinanceQuery_RateLimitedIsAnErrorNeverUnknown(t *testing.T) {
	f := newFakeBinance(t)
	f.queryBody = `{"symbol":"BTCUSDT","orderId":1,"clientOrderId":"o1","status":"NEW","executedQty":"0"}`
	// A bucket with no weight at all: the query costs 2 and cannot be afforded.
	bucket := newWeightBucket(0, time.Minute, nil)
	rest := newBinanceREST(restConfig{BaseURL: f.srv.URL, APIKey: "k", APISecret: "s", Bucket: bucket})
	v := NewBinanceVenue(BinanceConfig{MIC: "BINANCE", Symbols: StaticSymbolMap{"BTC-USD": "BTCUSDT"}, REST: rest})

	view, err := v.QueryOrder(context.Background(), queryOrderState("o1"))
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited so the caller backs off", err)
	}
	if view.State == OrderViewUnknown {
		t.Fatal("an exhausted weight budget was reported as OrderViewUnknown — the platform would " +
			"read its OWN back-pressure as Binance denying the order, and re-place it")
	}
	if f.gets != 0 {
		t.Fatalf("%d requests reached Binance for a refused query", f.gets)
	}
}

// A FILLED ORDER IS ADOPTED WITH THE VENUE'S OWN FILL IDENTITIES.
//
// The fill_id must be byte-for-byte what the placement path and the user-data
// stream mint for the same trade ("<symbol>-<tradeId>"), because that is what the
// order aggregate and the position book dedup on. A synthetic id here would fold
// a second copy of a trade the platform already holds.
func TestBinanceQuery_FilledCarriesTheVenuesOwnFillIdentities(t *testing.T) {
	f := newFakeBinance(t)
	f.queryBody = `{"symbol":"BTCUSDT","orderId":9001,"clientOrderId":"o1","status":"FILLED","executedQty":"1.00000000"}`
	f.myTradesBody = `[{"symbol":"BTCUSDT","id":42,"orderId":9001,"price":"50000.00","qty":"0.60000000",` +
		`"commission":"0.001","commissionAsset":"BNB","time":1750000000000},` +
		`{"symbol":"BTCUSDT","id":43,"orderId":9001,"price":"50010.00","qty":"0.40000000",` +
		`"commission":"0.001","commissionAsset":"BNB","time":1750000001000}]`

	view, err := venueOverFake(f).QueryOrder(context.Background(), queryOrderState("o1"))
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if view.State != OrderViewFilled {
		t.Fatalf("state = %v, want FILLED", view.State)
	}
	if len(view.Fills) != 2 {
		t.Fatalf("fills = %d, want 2 — one per trade, not one aggregate", len(view.Fills))
	}
	// THE IDENTITIES. The same fill_id UserDataIngester.handle mints for a live
	// execution report and BinanceVenue.fills mints for a placement; a change to
	// one without the others double-counts a trade.
	if view.Fills[0].GetFillId() != "BTCUSDT-42" || view.Fills[1].GetFillId() != "BTCUSDT-43" {
		t.Fatalf("fill ids = %q, %q; want BTCUSDT-42 and BTCUSDT-43 — the same ids the placement "+
			"path and the user-data stream mint for these trades",
			view.Fills[0].GetFillId(), view.Fills[1].GetFillId())
	}
	if got := dec.FromProto(view.Fills[0].GetQuantity()); got.Cmp(big.NewRat(6, 10)) != 0 {
		t.Fatalf("fill 1 qty = %s, want 0.6", got.RatString())
	}
	if got := dec.FromProto(view.Fills[1].GetPrice()); got.Cmp(big.NewRat(50010, 1)) != 0 {
		t.Fatalf("fill 2 price = %s, want 50010", got.RatString())
	}
	// THE TRADE'S OWN INSTANT, not the recovery's. An order adopted an hour after
	// it traded must not be timestamped an hour late in every execution-quality
	// measurement that reads it.
	if got := view.Fills[0].GetExecutedAt().AsTime(); !got.Equal(time.UnixMilli(1750000000000).UTC()) {
		t.Fatalf("executed_at = %s, want the exchange's own trade time", got)
	}
}

// A PARTIAL FILL IS ADOPTED THE SAME WAY, and it is the shape that used to be
// refused outright: cumulative fills re-presented on a later query are skipped by
// fill_id, which is only safe because the ids are per-trade.
func TestBinanceQuery_PartiallyFilledCarriesTheTradesSoFar(t *testing.T) {
	f := newFakeBinance(t)
	f.queryBody = `{"symbol":"BTCUSDT","orderId":9001,"clientOrderId":"o1","status":"PARTIALLY_FILLED","executedQty":"0.60000000"}`
	f.myTradesBody = `[{"symbol":"BTCUSDT","id":42,"orderId":9001,"price":"50000.00","qty":"0.60000000",` +
		`"commission":"0.001","commissionAsset":"BNB","time":1750000000000}]`

	view, err := venueOverFake(f).QueryOrder(context.Background(), queryOrderState("o1"))
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if view.State != OrderViewPartiallyFilled {
		t.Fatalf("state = %v, want PARTIALLY_FILLED", view.State)
	}
	if len(view.Fills) != 1 || view.Fills[0].GetFillId() != "BTCUSDT-42" {
		t.Fatalf("fills = %+v", view.Fills)
	}
}

// BINANCE CONTRADICTING ITSELF FREEZES THE ORDER.
//
// It reports the order traded and returns no trade for it. Reporting FILLED with
// no fill leaves a traded order looking untraded; reporting UNKNOWN re-places an
// order Binance has just said it executed. Neither is a guess this platform makes.
func TestBinanceQuery_FilledWithNoTradeIsIndeterminateNotUnknown(t *testing.T) {
	f := newFakeBinance(t)
	f.queryBody = `{"symbol":"BTCUSDT","orderId":9001,"clientOrderId":"o1","status":"FILLED","executedQty":"1.00000000"}`
	f.myTradesBody = `[]`

	view, err := venueOverFake(f).QueryOrder(context.Background(), queryOrderState("o1"))
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if view.State != OrderViewIndeterminate {
		t.Fatalf("state = %v, want INDETERMINATE", view.State)
	}
	if view.Reason == "" {
		t.Fatal("no reason given — an operator reading the quarantine learns nothing")
	}
}

// A FAILURE FETCHING THE TRADES IS A FAILURE, not a fill-less FILLED and not an
// UNKNOWN. The order query succeeded; the answer is still incomplete.
func TestBinanceQuery_TradeFetchFailureIsAnErrorNeverUnknown(t *testing.T) {
	f := newFakeBinance(t)
	f.queryBody = `{"symbol":"BTCUSDT","orderId":9001,"clientOrderId":"o1","status":"FILLED","executedQty":"1.00000000"}`
	f.myTradesBody = `{"code":-1003,"msg":"Too many requests."}`

	view, err := venueOverFake(f).QueryOrder(context.Background(), queryOrderState("o1"))
	if err == nil {
		t.Fatal("a refused myTrades produced no error")
	}
	if view.State == OrderViewUnknown || view.State == OrderViewFilled {
		t.Fatalf("state = %v; a half-fetched answer must be neither a re-drive nor an adoption", view.State)
	}
}

// A REJECTION IS ADOPTED. The venue terminally refused the order, and the OMS
// records that rather than re-driving it.
func TestBinanceQuery_RejectedIsAdoptable(t *testing.T) {
	f := newFakeBinance(t)
	f.queryBody = `{"symbol":"BTCUSDT","orderId":9001,"clientOrderId":"o1","status":"REJECTED","executedQty":"0"}`

	view, err := venueOverFake(f).QueryOrder(context.Background(), queryOrderState("o1"))
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if view.State != OrderViewRejected {
		t.Fatalf("state = %v, want REJECTED", view.State)
	}
}

// A WITHDRAWN OR EXPIRED ORDER FREEZES, AND THE QUARANTINE NAMES THE VENUE
// STATUS.
//
// execution.OrderView has no withdrawn answer. REJECTED would write a refusal
// over an order that may have partially traded before it was pulled; UNKNOWN
// would re-place it. EXPIRED is the common one — it is what Binance calls an IOC
// or FOK order that did not fill — and closing that gap needs an OrderView value
// that does not exist yet (#924).
func TestBinanceQuery_CancelledAndExpiredFreezeRatherThanGuess(t *testing.T) {
	for _, st := range []string{"CANCELED", "EXPIRED", "PENDING_CANCEL", "SOMETHING_NEW"} {
		f := newFakeBinance(t)
		f.queryBody = `{"symbol":"BTCUSDT","orderId":9001,"clientOrderId":"o1","status":"` + st + `","executedQty":"0"}`

		view, err := venueOverFake(f).QueryOrder(context.Background(), queryOrderState("o1"))
		if err != nil {
			t.Fatalf("%s: QueryOrder: %v", st, err)
		}
		if view.State != OrderViewIndeterminate {
			t.Fatalf("%s: state = %v, want INDETERMINATE", st, view.State)
		}
		if view.Reason == "" {
			t.Fatalf("%s: no reason given", st)
		}
	}
}

// AN UNMAPPED INSTRUMENT IS A VERDICT, NOT AN ERROR. Asking again in a minute
// changes nothing, so a nack would loop to a DLQ where a quarantine names the
// actual problem to the operator who can fix it.
func TestBinanceQuery_UnmappedInstrumentIsIndeterminate(t *testing.T) {
	f := newFakeBinance(t)
	st := queryOrderState("o1")
	st.InstrumentId = "DOGE-USD"

	view, err := venueOverFake(f).QueryOrder(context.Background(), st)
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if view.State != OrderViewIndeterminate {
		t.Fatalf("state = %v, want INDETERMINATE", view.State)
	}
	if f.gets != 0 {
		t.Fatalf("%d requests reached Binance for an instrument this deployment cannot address", f.gets)
	}
}
