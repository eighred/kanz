package corpact

import (
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

func day(d int) time.Time { return time.Date(2026, 1, d, 0, 0, 0, 0, time.UTC) }

func buy(id, inst string, qty, price string, eff, know int) *ledger.Event {
	q := dec.Rat(qty)
	p := dec.Rat(price)
	cash := new(big.Rat).Neg(new(big.Rat).Mul(q, p))
	return &ledger.Event{
		EntryID: id, PortfolioID: "PF", Type: ledger.EntryTrade, InstrumentID: inst,
		Quantity: q, Price: p, Cash: cash, CashCurrency: "USD",
		Effective: day(eff), Knowledge: day(know),
	}
}

// A dividend announced LATE (knowledge day 10) with an ex-date of day 2 must not
// appear in a book read as-of knowledge day 5, but must appear as-of day 10 —
// bitemporal, knowledge-time correct restatement.
func TestRestatementKnowledgeTimeCorrect(t *testing.T) {
	hold := buy("t1", "AAPL", "100", "10", 1, 1)
	div := CorporateAction{
		ActionID: "ca1", PortfolioID: "PF", Kind: Dividend, InstrumentID: "AAPL",
		PerUnit: dec.Rat("0.5"), Currency: "USD",
		ExDate: day(2), AnnouncedAt: day(10), // learned of it late
	}
	journal := []*ledger.Event{hold, div.ToEntry()}

	// As known on day 5: the dividend is not yet known ⇒ cash is just the buy.
	asKnownDay5 := ledger.ReplayAsOf("PF", journal, day(7), day(5))
	if got := asKnownDay5.CashBalance("USD"); got.Cmp(big.NewRat(-1000, 1)) != 0 {
		t.Fatalf("day-5 knowledge should not see late dividend: cash=%s", got.RatString())
	}

	// As known on day 10: the restatement applies the dividend at its ex-date.
	asKnownDay10 := ledger.ReplayAsOf("PF", journal, day(11), day(10))
	if got := asKnownDay10.CashBalance("USD"); got.Cmp(big.NewRat(-950, 1)) != 0 {
		t.Fatalf("day-10 knowledge should include restated dividend: cash=%s", got.RatString())
	}
}

// The action's effect is evaluated against the holding at the ex-date during the
// fold: a restatement that inserts an earlier purchase changes the dividend cash.
func TestRestatementChangesActionEffect(t *testing.T) {
	div := CorporateAction{
		ActionID: "ca1", PortfolioID: "PF", Kind: Dividend, InstrumentID: "AAPL",
		PerUnit: dec.Rat("1"), Currency: "USD", ExDate: day(5), AnnouncedAt: day(5),
	}.ToEntry()

	base := []*ledger.Event{buy("t1", "AAPL", "100", "10", 1, 1), div}
	b1 := ledger.Replay("PF", base)
	// cash = -1000 + 100*1 = -900
	if got := b1.CashBalance("USD"); got.Cmp(big.NewRat(-900, 1)) != 0 {
		t.Fatalf("base dividend cash: %s", got.RatString())
	}

	// A late-discovered earlier buy of 50 more (effective day 2, before ex-date)
	// restates the holding-at-ex-date to 150 ⇒ dividend now pays on 150.
	restated := append([]*ledger.Event{}, base...)
	restated = append(restated, buy("t2", "AAPL", "50", "10", 2, 9))
	b2 := ledger.Replay("PF", restated)
	// cash = -1000 -500 + 150*1 = -1350
	if got := b2.CashBalance("USD"); got.Cmp(big.NewRat(-1350, 1)) != 0 {
		t.Fatalf("restated dividend should pay on 150 shares: cash=%s", got.RatString())
	}
}
