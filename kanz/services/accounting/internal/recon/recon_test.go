package recon

import (
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

func day(d int) time.Time { return time.Date(2026, 1, d, 0, 0, 0, 0, time.UTC) }

func buy(id, inst, qty, price string) *ledger.Event {
	q := dec.Rat(qty)
	p := dec.Rat(price)
	return &ledger.Event{
		EntryID: id, PortfolioID: "PF", Type: ledger.EntryTrade, InstrumentID: inst,
		Quantity: q, Price: p, Cash: new(big.Rat).Neg(new(big.Rat).Mul(q, p)), CashCurrency: "USD",
		Effective: day(1), Knowledge: day(1),
	}
}

func book() *ledger.Book {
	return ledger.Replay("PF", []*ledger.Event{
		buy("t1", "AAPL", "100", "150"),
		buy("t2", "MSFT", "200", "300"),
		{EntryID: "c1", PortfolioID: "PF", Type: ledger.EntryCash, Cash: dec.Rat("50000"), CashCurrency: "USD", Effective: day(1), Knowledge: day(1)},
	})
}

func TestReconcileClean(t *testing.T) {
	b := book()
	stmt := Statement{
		PortfolioID: "PF",
		Positions:   map[string]*big.Rat{"AAPL": dec.Rat("100"), "MSFT": dec.Rat("200")},
		Cash:        map[string]*big.Rat{"USD": b.CashBalance("USD")},
	}
	if breaks, _ := Reconcile(b, nil, stmt, nil); len(breaks) != 0 {
		t.Fatalf("expected no breaks, got %v", breaks)
	}
}

func TestReconcileDetectsBreaks(t *testing.T) {
	b := book()
	stmt := Statement{
		PortfolioID: "PF",
		Positions: map[string]*big.Rat{
			"AAPL": dec.Rat("90"), // quantity break: IBOR 100 vs 90
			"TSLA": dec.Rat("10"), // custodian holds, IBOR does not
			// MSFT absent: IBOR holds, custodian does not
		},
		Cash: map[string]*big.Rat{"USD": dec.Rat("49000")}, // cash break
	}
	breaks, _ := Reconcile(b, nil, stmt, nil)
	byKey := map[string]Break{}
	for _, br := range breaks {
		byKey[br.Key] = br
	}
	if br, ok := byKey["AAPL"]; !ok || br.Kind != BreakQuantity || br.Diff.Cmp(big.NewRat(10, 1)) != 0 {
		t.Fatalf("AAPL quantity break missing/incorrect: %+v", br)
	}
	if br, ok := byKey["MSFT"]; !ok || br.Kind != BreakMissingAtCustodian {
		t.Fatalf("MSFT missing-at-custodian break missing: %+v", br)
	}
	if br, ok := byKey["TSLA"]; !ok || br.Kind != BreakMissingInIBOR {
		t.Fatalf("TSLA missing-in-IBOR break missing: %+v", br)
	}
	if br, ok := byKey["USD"]; !ok || br.Kind != BreakCash {
		t.Fatalf("USD cash break missing: %+v", br)
	}
}

func TestReconcileTolerance(t *testing.T) {
	b := book()
	stmt := Statement{
		PortfolioID: "PF",
		Positions:   map[string]*big.Rat{"AAPL": dec.Rat("100.5"), "MSFT": dec.Rat("200")},
		Cash:        map[string]*big.Rat{"USD": b.CashBalance("USD")},
	}
	// Within a tolerance of 1 share ⇒ no break.
	if breaks, _ := Reconcile(b, nil, stmt, dec.Rat("1")); len(breaks) != 0 {
		t.Fatalf("within tolerance should not break, got %v", breaks)
	}
	// Exact match required ⇒ the 0.5 difference breaks.
	if breaks, _ := Reconcile(b, nil, stmt, nil); len(breaks) != 1 || breaks[0].Kind != BreakQuantity {
		t.Fatalf("exact match should detect the 0.5 break, got %v", breaks)
	}
}
