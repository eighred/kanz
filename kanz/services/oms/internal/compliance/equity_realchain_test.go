package compliance

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/marketdata/mark"
	"github.com/eighred/kanz/services/oms/internal/position"
)

// THE SAME CHAIN, WITH NOTHING STUBBED (#780).
//
// equity_test.go drives BookSource with a fixed snapshot and a map of prices,
// which is the right shape for asserting the branch behaviour. It does not prove
// the ONE thing that made this defect survive: that the three real folds — fills
// into holdings, market data into marks, an accounting announcement into cash —
// produce numbers that agree when they meet.
//
// Every component below is the production type:
//
//	position.Book   folds real orderpb.Fill messages through fillfact.Validate
//	mark.Source     folds a real MarketDataEvent off a real envelope
//	cashview.View   folds a real accounting.v1.PortfolioCashBalance
//	BookSource      the join under test
//	PreTradeGate    the real engine, evaluating a real mandate
//
// WHY IT MATTERS THAT THE FILLS ARE REAL. The position book values holdings at
// AVERAGE COST, which is the whole reason the old denominator was wrong, and a
// hand-written snapshot is free to state any cost it likes. Here the cost is
// whatever folding these fills actually produces — so if marking to market ever
// silently degraded back to cost, the numbers would coincide and the test would
// say so.

func fill(id, instrument string, qty, price int64, at time.Time) *orderpb.Fill {
	return &orderpb.Fill{
		FillId:       id,
		OrderId:      "o-" + id,
		InstrumentId: instrument,
		Venue:        "XSIM",
		Quantity:     &commonpb.Decimal{Coefficient: qty},
		Price:        &commonpb.Decimal{Coefficient: price},
		Side:         orderpb.Side_SIDE_BUY,
		ExecutedAt:   timestamppb.New(at),
	}
}

// fixedNow is a stopped clock for the mark fold.
//
// THESE TESTS MUST NOT RACE time.Now(). mark.Source expires an entry on
// s.now().Sub(asOf) > maxAge, and on Windows the monotonic clock is coarse
// enough that two consecutive time.Now() calls return the SAME instant — so a
// "one nanosecond of freshness" fixture is expired on one run and live on the
// next. That is a flaky test, and a flaky test on a control this size is worse
// than no test: it teaches the next person to re-run rather than to read.
// Stopping the clock makes the age exact and the assertion about the RULE.
func fixedNow(at time.Time) func() time.Time { return func() time.Time { return at } }

// markAt folds one market-data event through the real Source, envelope and all.
func markAt(t *testing.T, src *mark.Source, instrument string, price int64, at time.Time) {
	t.Helper()
	payload, err := proto.Marshal(&marketpb.MarketDataEvent{
		InstrumentId: instrument,
		EventTime:    timestamppb.New(at),
		Data: &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{
			Price: &commonpb.Decimal{Coefficient: price},
		}},
	})
	if err != nil {
		t.Fatalf("marshal market data: %v", err)
	}
	env := &envelopepb.Envelope{EventType: "market.instrument.trade"}
	if err := src.Handle(context.Background(), env, payload); err != nil {
		t.Fatalf("fold mark: %v", err)
	}
	if src.Mark(instrument) == nil {
		t.Fatalf("the mark fold accepted the event and holds no price for %s — the fixture is not "+
			"exercising what it claims to", instrument)
	}
}

// TestTheRealFoldsAgreeOnEquity drives one levered book all the way through.
//
// The fund buys 1,000 AAPL at $100 — $100,000 of stock — having been funded with
// $50,000. The other $50,000 is borrowed, so cash is NEGATIVE and equity is
// $50,000 against $100,000 of exposure: 2.0x gross leverage, an ordinary margin
// position and a plain breach of a 1.5x cap.
//
// Under the positions-only denominator this book scored 100,000/100,000 = 1.0
// and passed. That is the number this test exists to make impossible to restore.
func TestTheRealFoldsAgreeOnEquity(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()

	book := position.NewBook("USD")
	if _, err := book.Apply(ctx, "PF1", fill("f1", "AAPL", 1_000, 100, now), now, nil); err != nil {
		t.Fatalf("fold fill: %v", err)
	}

	// The holding is at COST until something marks it. Asserting that first means
	// the marked figures below cannot be confused with the cost ones.
	snap, err := book.Snapshot(ctx, "PF1", now)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if got := dec.Str(dec.FromProto(snap.GetPortfolio().GetTotalMarketValue().GetAmount())); got != "100000" {
		t.Fatalf("position book total = %s, want 100000 at cost", got)
	}

	marks := mark.New(fixedNow(now), time.Hour)
	markAt(t, marks, "AAPL", 100, now)

	src := NewBookSource(book, cashAt(t, -50_000), nil, marks)
	b, err := src.Book(ctx, "PF1")
	if err != nil {
		t.Fatalf("Book: %v", err)
	}
	if b.NAVBasis != comp.NAVBasisEquity {
		t.Fatalf("NAVBasis = %v (detail %q) — every real fold supplied its half and the join still "+
			"could not establish equity", b.NAVBasis, b.NAVBasisDetail)
	}
	if got := dec.Str(dec.FromProto(b.NAV.GetAmount())); got != "50000" {
		t.Fatalf("equity = %s, want 50000 ($100,000 of stock less the $50,000 borrowed to buy it). "+
			"100000 means cash was dropped and the denominator is the positions total again — the "+
			"#780 defect", got)
	}

	gate := comp.NewPreTradeGate(comp.NewEngine(nil), src,
		stubMandate{leverageMandate("PF1", 15, -1)}, nil, nil, nil) // 1.5x
	verdict, err := gate.Evaluate(ctx, comp.OrderDelta{
		TenantID: "t1", PortfolioID: "PF1", InstrumentID: "AAPL",
		SignedQuantity: at(1), Price: at(100), Currency: "USD", OrderID: "o2",
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if verdict.Allowed {
		t.Fatal("a book at 2.0x gross leverage was ADMITTED under a 1.5x cap, with every fold real. " +
			"This is the production path: fills folded, marks folded, cash announced, and the " +
			"control still did not fire")
	}
}

// TestAStaleMarkStopsEquityRatherThanAgeingIntoIt is the failure this platform
// cares about more than a missing price: a feed that WAS reporting and stopped.
//
// mark.Source expires a price past its maxAge and returns nil for it, refusing to
// distinguish "never seen" from "seen and stale" on a decision path. So a book
// whose marks have aged out must fall back to the proxy basis and refuse
// leverage — never keep quoting the last price it saw as though it were current.
func TestAStaleMarkStopsEquityRatherThanAgeingIntoIt(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()

	book := position.NewBook("USD")
	if _, err := book.Apply(ctx, "PF1", fill("f1", "AAPL", 1_000, 100, now), now, nil); err != nil {
		t.Fatalf("fold fill: %v", err)
	}

	// An hour-old price against a one-minute freshness bound — stale by a factor
	// of sixty, on a stopped clock, so the arithmetic cannot come out either way.
	marks := mark.New(fixedNow(now), time.Minute)
	payload, err := proto.Marshal(&marketpb.MarketDataEvent{
		InstrumentId: "AAPL", EventTime: timestamppb.New(now.Add(-time.Hour)),
		Data: &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{
			Price: &commonpb.Decimal{Coefficient: 150},
		}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := marks.Handle(ctx, &envelopepb.Envelope{EventType: "market.instrument.trade"}, payload); err != nil {
		t.Fatalf("fold mark: %v", err)
	}

	// THE FIXTURE HAS TO BE STALE, NOT ABSENT. Lookup is the diagnostic accessor
	// and reports the entry regardless of expiry: seen, with a price the safe
	// accessor refuses. Without this the test would also pass for an event the
	// fold rejected outright, which proves nothing about staleness.
	if _, _, seen := marks.Lookup("AAPL"); !seen {
		t.Fatal("the mark fold never accepted the event — this fixture is testing a REJECTED " +
			"event, not a stale one")
	}
	if marks.Mark("AAPL") != nil {
		t.Fatal("the mark is still live; the freshness bound in this fixture is not doing anything")
	}

	b, err := NewBookSource(book, cashAt(t, -50_000), nil, marks).Book(ctx, "PF1")
	if err != nil {
		t.Fatalf("Book: %v", err)
	}
	if b.NAVBasis == comp.NAVBasisEquity {
		t.Fatal("equity was computed from an EXPIRED mark. A stalled feed would then keep valuing " +
			"the book at the last price it ever saw, and the leverage cap would be enforced " +
			"against a number that stopped moving")
	}
	if b.NAVBasisDetail == "" {
		t.Error("no reason recorded for a book that could not be valued — the refusal downstream " +
			"cannot say a stalled feed is why")
	}
}
