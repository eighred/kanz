package okx

// ONE EXECUTION HAS ONE FILL ID, WHICHEVER PATH REPORTS IT (#923).
//
// This connector has three ingress paths for a fill and they used to disagree:
//
//	placement (Execute)   "<instId>-<ordId>"    ONE CUMULATIVE fill per order
//	query    (QueryOrder) refused to answer at all, because of that disagreement
//	websocket (user data) "<instId>-<tradeId>"  one fill per trade
//
// ordId and tradeId are different OKX identifier spaces. The order aggregate
// (order_fills, #782) and the position book (position_fills) both make folding
// exactly-once by DEDUPING ON fill_id, so two names for one trade means either a
// double count or an overfill that quarantines the order — and the cumulative
// name fails the other way too, since it is stable while its quantity grows, so
// an order folded at 40% is re-presented as the same id carrying 100 and is
// correctly skipped, leaving it recorded at 40% forever.
//
// The test below drives ONE trade through all three paths and requires them to
// produce the same name. It is the connector-side precondition for the OMS's
// TestAdoptFoldsEveryFillOnceAndReadoptionIsANoOp: that test proves the fold
// skips a fill_id the order already holds, and this one proves the ids OKX
// produces are the same ids, so the skip lands on the right fill.

import (
	"context"
	"math/big"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
)

// The SAME execution as OKX reports it in each of its three shapes: trade 7 on
// BTC-USDT, 1 unit at 50000, belonging to order 312 which carries our clOrdId o1.
const (
	oneTradeQueryBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"o1","state":"filled",` +
		`"sz":"1","accFillSz":"1","avgPx":"50000","fee":"-0.05","feeCcy":"USDT"}]}`
	oneTradeFillsBody = `{"code":"0","msg":"","data":[{"instId":"BTC-USDT","tradeId":"7","ordId":"312",` +
		`"clOrdId":"o1","side":"buy","fillPx":"50000","fillSz":"1","fee":"-0.05","feeCcy":"USDT",` +
		`"ts":"1700000000000"}]}`
	oneTradePush = `{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","ordId":"312",` +
		`"clOrdId":"o1","state":"filled","fillSz":"1","fillPx":"50000","accFillSz":"1","tradeId":"7",` +
		`"fillFee":"-0.05","fillFeeCcy":"USDT","uTime":"1700000000000"}]}`
)

func TestOKX_OneExecutionHasOneFillIdAcrossEveryPath(t *testing.T) {
	// 1. THE PLACEMENT PATH. Execute places, queries the order, then reads the
	//    trades behind it.
	fp := newFakeOKX(t)
	fp.placeBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"o1","sCode":"0","sMsg":""}]}`
	fp.queryBody = oneTradeQueryBody
	fp.fillsBody = oneTradeFillsBody
	placed, err := okxVenueOver(fp).Execute(context.Background(), okxMarket("o1"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(placed) != 1 {
		t.Fatalf("placement fills = %d, want 1", len(placed))
	}

	// 2. THE QUERY PATH — the crash-recovery read the OMS adopts (#920).
	fq := newFakeOKX(t)
	fq.queryBody = oneTradeQueryBody
	fq.fillsBody = oneTradeFillsBody
	view, err := okxVenueOver(fq).QueryOrder(context.Background(), okxLimit("o1"))
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if view.State != OrderViewFilled {
		t.Fatalf("view state = %v, want FILLED — an adapter that can name the venue's own fills "+
			"can adopt them, and refusing to would freeze every interrupted OKX order", view.State)
	}
	if len(view.Fills) != 1 {
		t.Fatalf("view fills = %d, want 1", len(view.Fills))
	}

	// 3. THE PRIVATE WEBSOCKET — the live path, which publishes the FACT the
	//    ledger and the position book fold.
	cap := &okxCapture{}
	_ = okxIngesterOver([][]byte{[]byte(oneTradePush)}, cap).Run(context.Background())
	var pushed *orderpb.Fill
	for _, e := range cap.events {
		if f, ok := e.Payload.(*orderpb.OrderFilled); ok {
			pushed = f.GetFill()
		}
	}
	if pushed == nil {
		t.Fatal("no OrderFilled published from the user-data push")
	}

	// THE ASSERTION THIS FILE EXISTS FOR.
	ids := map[string][]string{}
	ids[placed[0].GetFillId()] = append(ids[placed[0].GetFillId()], "placement")
	ids[view.Fills[0].GetFillId()] = append(ids[view.Fills[0].GetFillId()], "query")
	ids[pushed.GetFillId()] = append(ids[pushed.GetFillId()], "user-data")
	if len(ids) != 1 {
		t.Fatalf("one OKX execution produced %d distinct fill_ids: %v. Both the order aggregate "+
			"and the position book dedup on fill_id, so a second name for this trade folds a "+
			"second copy of it — or overfills past leaves and quarantines the order", len(ids), ids)
	}
	if _, ok := ids["BTC-USDT-7"]; !ok {
		t.Fatalf("the agreed fill_id is %v, want BTC-USDT-7 — instId and OKX's own tradeId. An id "+
			"built from ordId names the ORDER, and re-presenting it later carries a different "+
			"quantity under the same name", ids)
	}

	// AND THE SAME TRADE, not merely the same label. An id that agreed while the
	// numbers did not would be worse than a disagreement: the fold would skip the
	// second sighting and keep whichever quantity arrived first.
	for _, f := range []*orderpb.Fill{placed[0], view.Fills[0], pushed} {
		if got := dec.FromProto(f.GetQuantity()); got.Cmp(big.NewRat(1, 1)) != 0 {
			t.Fatalf("fill %s quantity = %s, want 1", f.GetFillId(), got.RatString())
		}
		if got := dec.FromProto(f.GetPrice()); got.Cmp(big.NewRat(50000, 1)) != 0 {
			t.Fatalf("fill %s price = %s, want 50000", f.GetFillId(), got.RatString())
		}
		if f.GetVenueExecutionId() != "7" {
			t.Fatalf("fill %s venue_execution_id = %q, want OKX's tradeId 7",
				f.GetFillId(), f.GetVenueExecutionId())
		}
		if got := dec.FromProto(f.GetFee().GetAmount()); got.Cmp(big.NewRat(1, 20)) != 0 {
			t.Fatalf("fill %s fee = %s, want 0.05 — the magnitude of the negative OKX reports",
				f.GetFillId(), got.RatString())
		}
	}
}

// RE-ADOPTING A QUERIED VIEW IS A NO-OP, AT THE ONLY LAYER THIS PACKAGE CAN
// PROVE IT.
//
// The OMS's fold is what actually dedups (Store.Save claims (tenant_id,
// fill_id) in the same transaction as the state and the FACT, #782/#798); its
// own TestAdoptFoldsEveryFillOnceAndReadoptionIsANoOp covers that. What this
// connector owes that mechanism is STABILITY: two queries of an unchanged order
// must name the same fills, or the skip has nothing to match on.
//
// The old cumulative fill failed this in the way that hides: its id was stable
// and its QUANTITY was not, so the second view was skipped and the order kept
// the first view's smaller size forever.
func TestOKX_ReQueryingAnUnchangedOrderNamesTheSameFills(t *testing.T) {
	f := newFakeOKX(t)
	f.queryBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"o1","state":"partially_filled",` +
		`"sz":"1","accFillSz":"0.4","avgPx":"50000"}]}`
	f.fillsBody = `{"code":"0","msg":"","data":[{"instId":"BTC-USDT","tradeId":"7","ordId":"312",` +
		`"clOrdId":"o1","fillPx":"50000","fillSz":"0.4","ts":"1700000000000"}]}`
	v := okxVenueOver(f)

	first, err := v.QueryOrder(context.Background(), okxLimit("o1"))
	if err != nil {
		t.Fatalf("first QueryOrder: %v", err)
	}
	if first.State != OrderViewPartiallyFilled || len(first.Fills) != 1 {
		t.Fatalf("first view = %v with %d fills, want PARTIALLY_FILLED with 1",
			first.State, len(first.Fills))
	}

	// THE ORDER TRADES ON. OKX now reports it filled, and reports BOTH trades —
	// the one already folded and a new one.
	f.queryBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"o1","state":"filled",` +
		`"sz":"1","accFillSz":"1","avgPx":"50000"}]}`
	f.fillsBody = `{"code":"0","msg":"","data":[` +
		`{"instId":"BTC-USDT","tradeId":"7","ordId":"312","clOrdId":"o1","fillPx":"50000","fillSz":"0.4","ts":"1700000000000"},` +
		`{"instId":"BTC-USDT","tradeId":"9","ordId":"312","clOrdId":"o1","fillPx":"50100","fillSz":"0.6","ts":"1700000002000"}]}`

	second, err := v.QueryOrder(context.Background(), okxLimit("o1"))
	if err != nil {
		t.Fatalf("second QueryOrder: %v", err)
	}
	if second.State != OrderViewFilled || len(second.Fills) != 2 {
		t.Fatalf("second view = %v with %d fills, want FILLED with 2", second.State, len(second.Fills))
	}
	// THE FILL ALREADY FOLDED COMES BACK UNCHANGED — same name AND same quantity,
	// so the OMS's claim skips it and folds only the new one. A cumulative fill
	// would have come back as the same name carrying 1 instead of 0.4, been
	// skipped, and left the order recorded at 0.4 with the remaining 0.6 lost.
	if second.Fills[0].GetFillId() != first.Fills[0].GetFillId() {
		t.Fatalf("the fill already folded is now named %q, was %q — the OMS's fill_id claim has "+
			"nothing to match on and folds it a second time",
			second.Fills[0].GetFillId(), first.Fills[0].GetFillId())
	}
	if got := dec.FromProto(second.Fills[0].GetQuantity()); got.Cmp(big.NewRat(4, 10)) != 0 {
		t.Fatalf("the re-presented fill %s now carries %s, was 0.4. A fill whose quantity moves "+
			"under a stable name is skipped by the fold, and the order keeps the smaller size "+
			"forever with nothing able to notice", second.Fills[0].GetFillId(), got.RatString())
	}
	if second.Fills[1].GetFillId() != "BTC-USDT-9" {
		t.Fatalf("the new fill is named %q, want BTC-USDT-9", second.Fills[1].GetFillId())
	}
}
