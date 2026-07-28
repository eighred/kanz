package ledger

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
)

func day(d int) time.Time { return time.Date(2026, 1, d, 0, 0, 0, 0, time.UTC) }

// trade builds a TRADE entry with the cash leg computed (buy pays out, sell takes
// in), at the given effective/knowledge days.
func trade(id, inst string, qty, price string, eff, know int) *Event {
	q := dec.Rat(qty)
	p := dec.Rat(price)
	cash := new(big.Rat).Mul(new(big.Rat).Abs(q), p)
	if q.Sign() > 0 {
		cash.Neg(cash) // buy
	}
	return &Event{
		EntryID: id, PortfolioID: "PF", Type: EntryTrade, InstrumentID: inst,
		Quantity: q, Price: p, Cash: cash, CashCurrency: "USD",
		Effective: day(eff), Knowledge: day(know),
	}
}

func cashEntry(id string, amount string, eff, know int) *Event {
	return &Event{
		EntryID: id, PortfolioID: "PF", Type: EntryCash,
		Cash: dec.Rat(amount), CashCurrency: "USD",
		Effective: day(eff), Knowledge: day(know),
	}
}

// nav is a tiny test valuation: cash + Σ qty·price + accrued.
func nav(b *Book, prices map[string]string) *big.Rat {
	total := b.CashBalance("USD")
	total.Add(total, b.AccruedBalance("USD"))
	for inst, p := range b.Positions {
		if p.Qty.Sign() == 0 {
			continue
		}
		total.Add(total, new(big.Rat).Mul(p.Qty, dec.Rat(prices[inst])))
	}
	return total
}

func TestReplayDeterministicNAV(t *testing.T) {
	events := []*Event{
		cashEntry("c1", "1000000", 1, 1),
		trade("t1", "AAPL", "100", "150", 2, 2),
		trade("t2", "MSFT", "200", "300", 3, 3),
		trade("t3", "AAPL", "-40", "160", 4, 4), // partial sell, realizes P&L
	}
	prices := map[string]string{"AAPL": "160", "MSFT": "310"}

	b1 := Replay("PF", events)
	b2 := Replay("PF", reverse(events)) // order-independent: sorted by (eff,know,id)
	n1, n2 := nav(b1, prices), nav(b2, prices)
	if n1.Cmp(n2) != 0 {
		t.Fatalf("replay not deterministic: %s vs %s", n1.FloatString(2), n2.FloatString(2))
	}

	// Snapshot midway then fold the tail must equal a full replay.
	st := NewMemoryStore()
	for _, e := range events {
		_ = st.Append(context.Background(), e)
	}
	mid := Replay("PF", events[:2])
	snap := mid.Snapshot(day(2))
	_ = st.SaveSnapshot(context.Background(), snap)
	got, err := MaterializeCurrent(context.Background(), st, "PF")
	if err != nil {
		t.Fatal(err)
	}
	if nav(got, prices).Cmp(n1) != 0 {
		t.Fatalf("snapshot+tail NAV %s != full replay %s", nav(got, prices).FloatString(2), n1.FloatString(2))
	}
}

func TestApplyIdempotent(t *testing.T) {
	b := NewBook("PF")
	e := trade("t1", "AAPL", "100", "150", 2, 2)
	b.Apply(e)
	b.Apply(e) // replay of same entry id: no-op
	if got := b.Positions["AAPL"].Qty; got.Cmp(big.NewRat(100, 1)) != 0 {
		t.Fatalf("idempotency broken: qty=%s", got.RatString())
	}
}

func TestStockSplitConservesMarketValue(t *testing.T) {
	b := NewBook("PF")
	b.Apply(trade("t1", "AAPL", "100", "10", 1, 1)) // 100 @ 10, cost basis 1000
	before := new(big.Rat).Mul(b.Positions["AAPL"].Qty, b.Positions["AAPL"].AvgCost)

	split := &Event{
		EntryID: "ca1", PortfolioID: "PF", Type: EntryCorporateAction, InstrumentID: "AAPL",
		Action:    &Action{Kind: CorpActSplit, Ratio: big.NewRat(2, 1)},
		Effective: day(2), Knowledge: day(2),
	}
	b.Apply(split)

	p := b.Positions["AAPL"]
	if p.Qty.Cmp(big.NewRat(200, 1)) != 0 {
		t.Fatalf("split qty: want 200 got %s", p.Qty.RatString())
	}
	if p.AvgCost.Cmp(big.NewRat(5, 1)) != 0 {
		t.Fatalf("split avg cost: want 5 got %s", p.AvgCost.RatString())
	}
	after := new(big.Rat).Mul(p.Qty, p.AvgCost)
	if before.Cmp(after) != 0 {
		t.Fatalf("split changed cost-basis market value: %s -> %s", before.RatString(), after.RatString())
	}
	// Market value at the (halved) post-split price is conserved too.
	mvBefore := big.NewRat(100*10, 1)
	mvAfter := new(big.Rat).Mul(p.Qty, big.NewRat(5, 1))
	if mvBefore.Cmp(mvAfter) != 0 {
		t.Fatalf("split changed market value at price: %s -> %s", mvBefore.RatString(), mvAfter.RatString())
	}
}

func TestDividendPaysOnHeldQuantity(t *testing.T) {
	b := NewBook("PF")
	b.Apply(trade("t1", "AAPL", "100", "10", 1, 1))
	div := &Event{
		EntryID: "ca1", PortfolioID: "PF", Type: EntryCorporateAction, InstrumentID: "AAPL",
		Action:    &Action{Kind: CorpActDividend, PerUnit: dec.Rat("0.5"), Currency: "USD"},
		Effective: day(2), Knowledge: day(2),
	}
	b.Apply(div)
	// cash = -1000 (the buy) + 100*0.5 = -950
	if got := b.CashBalance("USD"); got.Cmp(big.NewRat(-950, 1)) != 0 {
		t.Fatalf("dividend cash: want -950 got %s", got.RatString())
	}
}

func TestMergerConvertsCarryingBasis(t *testing.T) {
	b := NewBook("PF")
	b.Apply(trade("t1", "A", "100", "10", 1, 1)) // basis 1000
	merge := &Event{
		EntryID: "ca1", PortfolioID: "PF", Type: EntryCorporateAction, InstrumentID: "A",
		Action: &Action{Kind: CorpActMerger, Ratio: dec.Rat("1.5"), PerUnit: dec.Rat("2"),
			Target: "B", Currency: "USD"},
		Effective: day(2), Knowledge: day(2),
	}
	b.Apply(merge)
	if b.Positions["A"].Qty.Sign() != 0 {
		t.Fatalf("merger left source position: %s", b.Positions["A"].Qty.RatString())
	}
	pb := b.Positions["B"]
	if pb.Qty.Cmp(big.NewRat(150, 1)) != 0 {
		t.Fatalf("merger target qty: want 150 got %s", pb.Qty.RatString())
	}
	// basis 1000 over 150 shares ⇒ avg 6.6667; total basis conserved.
	if got := new(big.Rat).Mul(pb.Qty, pb.AvgCost); got.Cmp(big.NewRat(1000, 1)) != 0 {
		t.Fatalf("merger basis not carried: %s", got.RatString())
	}
	// cash: -1000 (buy) + 100*2 (merger cash) = -800
	if got := b.CashBalance("USD"); got.Cmp(big.NewRat(-800, 1)) != 0 {
		t.Fatalf("merger cash: want -800 got %s", got.RatString())
	}
}

func reverse(in []*Event) []*Event {
	out := make([]*Event, len(in))
	for i, e := range in {
		out[len(in)-1-i] = e
	}
	return out
}
