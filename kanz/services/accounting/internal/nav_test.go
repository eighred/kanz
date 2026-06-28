package accounting

import (
	"math/big"
	"testing"
	"time"

	"github.com/kanz-eng/kanz/services/accounting/internal/dec"
	"github.com/kanz-eng/kanz/services/accounting/internal/ledger"
)

func day(d int) time.Time { return time.Date(2026, 1, d, 0, 0, 0, 0, time.UTC) }

func buy(id, inst, qty, price string, eff int) *ledger.Event {
	q := dec.Rat(qty)
	p := dec.Rat(price)
	return &ledger.Event{
		EntryID: id, PortfolioID: "PF", Type: ledger.EntryTrade, InstrumentID: inst,
		Quantity: q, Price: p, Cash: new(big.Rat).Neg(new(big.Rat).Mul(q, p)), CashCurrency: "USD",
		Effective: day(eff), Knowledge: day(eff),
	}
}

func cash(id, amt string, eff int) *ledger.Event {
	return &ledger.Event{
		EntryID: id, PortfolioID: "PF", Type: ledger.EntryCash,
		Cash: dec.Rat(amt), CashCurrency: "USD", Effective: day(eff), Knowledge: day(eff),
	}
}

func dividend(id, inst, dps string, eff int) *ledger.Event {
	return &ledger.Event{
		EntryID: id, PortfolioID: "PF", Type: ledger.EntryCorporateAction, InstrumentID: inst,
		Action:    &ledger.Action{Kind: ledger.CorpActDividend, PerUnit: dec.Rat(dps), Currency: "USD"},
		Effective: day(eff), Knowledge: day(eff),
	}
}

func TestComputeNAVComponents(t *testing.T) {
	b := ledger.Replay("PF", []*ledger.Event{cash("c1", "100000", 1), buy("t1", "AAPL", "100", "150", 2)})
	nav, err := ComputeNAV(b, "USD", day(2), map[string]*big.Rat{"AAPL": dec.Rat("160")})
	if err != nil {
		t.Fatal(err)
	}
	// cash = 100000 - 15000 = 85000; sec = 100*160 = 16000; total = 101000
	if nav.Cash.Cmp(big.NewRat(85000, 1)) != 0 {
		t.Fatalf("cash: %s", nav.Cash.RatString())
	}
	if nav.SecurityValue.Cmp(big.NewRat(16000, 1)) != 0 {
		t.Fatalf("sec: %s", nav.SecurityValue.RatString())
	}
	if nav.Total.Cmp(big.NewRat(101000, 1)) != 0 {
		t.Fatalf("total: %s", nav.Total.RatString())
	}
}

func TestComputeNAVMissingPrice(t *testing.T) {
	b := ledger.Replay("PF", []*ledger.Event{buy("t1", "AAPL", "100", "150", 2)})
	if _, err := ComputeNAV(b, "USD", day(2), map[string]*big.Rat{}); err == nil {
		t.Fatal("expected error for missing price")
	}
}

func TestAttributionIdentity(t *testing.T) {
	prior := ledger.Replay("PF", []*ledger.Event{cash("c1", "100000", 1)})
	priorNAV, _ := ComputeNAV(prior, "USD", day(1), map[string]*big.Rat{})

	period := []*ledger.Event{
		buy("t1", "AAPL", "100", "150", 2),
		dividend("ca1", "AAPL", "0.5", 3),
		cash("c2", "5000", 3), // external subscription
	}
	current := ledger.Replay("PF", append([]*ledger.Event{cash("c1", "100000", 1)}, period...))
	currentNAV, _ := ComputeNAV(current, "USD", day(3), map[string]*big.Rat{"AAPL": dec.Rat("160")})

	got := Attribute(priorNAV, currentNAV, period, nil)
	sum := AttributionTotal(got)
	want := new(big.Rat).Sub(currentNAV.Total, priorNAV.Total)
	if sum.Cmp(want) != 0 {
		t.Fatalf("attribution sum %s != NAV change %s", sum.FloatString(2), want.FloatString(2))
	}
	// The external subscription is the cash driver; the dividend is corporate_action.
	if c := componentOf(got, "cash"); c.Cmp(big.NewRat(5000, 1)) != 0 {
		t.Fatalf("cash driver: want 5000 got %s", c.RatString())
	}
	if c := componentOf(got, "corporate_action"); c.Cmp(big.NewRat(50, 1)) != 0 {
		t.Fatalf("corp-action driver: want 50 got %s", c.RatString())
	}
}

func TestAttributionPurePriceEqualsMtM(t *testing.T) {
	book := ledger.Replay("PF", []*ledger.Event{cash("c1", "100000", 1), buy("t1", "AAPL", "100", "150", 2)})
	priorNAV, _ := ComputeNAV(book, "USD", day(2), map[string]*big.Rat{"AAPL": dec.Rat("150")})
	currentNAV, _ := ComputeNAV(book, "USD", day(3), map[string]*big.Rat{"AAPL": dec.Rat("160")})

	got := Attribute(priorNAV, currentNAV, nil, nil) // no period entries
	// Only price moved: price == 100*(160-150) = 1000; everything else 0.
	if c := componentOf(got, "price"); c.Cmp(big.NewRat(1000, 1)) != 0 {
		t.Fatalf("price driver: want 1000 got %s", c.RatString())
	}
	for _, s := range []string{"cash", "fx", "corporate_action"} {
		if c := componentOf(got, s); c.Sign() != 0 {
			t.Fatalf("%s driver should be zero, got %s", s, c.RatString())
		}
	}
}

func TestStraightLineAccrual(t *testing.T) {
	amount := dec.Rat("100")
	start, end := day(1), day(11) // 10-day accrual period
	if got := StraightLineAccrual(amount, start, end, day(6)); got.Cmp(big.NewRat(50, 1)) != 0 {
		t.Fatalf("half-way accrual: want 50 got %s", got.RatString())
	}
	if got := StraightLineAccrual(amount, start, end, day(1)); got.Sign() != 0 {
		t.Fatalf("pre-start accrual: want 0 got %s", got.RatString())
	}
	if got := StraightLineAccrual(amount, start, end, day(20)); got.Cmp(amount) != 0 {
		t.Fatalf("post-end accrual: want 100 got %s", got.RatString())
	}
}

func TestAccrualEntryFoldsIntoAccrued(t *testing.T) {
	b := ledger.NewBook("PF")
	b.Apply(AccrualEntry("acc1", "PF", "BOND", "USD", dec.Rat("25"), day(5), day(5)))
	if got := b.AccruedBalance("USD"); got.Cmp(big.NewRat(25, 1)) != 0 {
		t.Fatalf("accrued: want 25 got %s", got.RatString())
	}
	// Accrual carries in NAV but not in cash.
	if got := b.CashBalance("USD"); got.Sign() != 0 {
		t.Fatalf("accrual should not touch cash, got %s", got.RatString())
	}
}

func componentOf(n NAV, source string) *big.Rat {
	for _, c := range n.Attribution {
		if c.Source == source {
			return c.Amount
		}
	}
	return new(big.Rat)
}
