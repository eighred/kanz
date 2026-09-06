package ledger

import (
	"math/big"
	"testing"
	"time"
)

// THE EXECUTIONS COME OUT OF THE SAME FILTER AS THE BOOK (#1049).
//
// foldForAccounts is the ONE answer to "which entries are in a custodian's
// comparison basis". These pin that the transaction grain inherits it rather than
// deriving a second answer beside it — which is the shape #1073 was filed on.

func execEvent(id, account, instrument, ref string, qty int64, at time.Time) *Event {
	return &Event{
		EntryID: id, PortfolioID: "PF", VenueAccountID: account, Type: EntryTrade,
		InstrumentID: instrument, Quantity: big.NewRat(qty, 1), Price: big.NewRat(1, 1),
		Cash: big.NewRat(-qty, 1), CashCurrency: "USD",
		Effective: at, Knowledge: at,
		SettlementBasis: SettlementSettled, SettlementDate: at,
		SourceRef: ref,
	}
}

func TestCustodyBasisExecutionsInheritTheAccountScope(t *testing.T) {
	at := time.Date(2026, 3, 4, 15, 0, 0, 0, time.UTC)
	events := []*Event{
		execEvent("e1", "okx-sub-1", "AAPL", "fill-a", 10, at),
		execEvent("e2", "bin-main", "MSFT", "fill-b", 20, at),
		// Settled against NO exchange account: in no custodian's basis, so in no
		// custodian's transaction pass either.
		execEvent("e3", "", "PRIVATE-CO", "sub-1", 5, at),
	}
	okx, err := NewAccountScope("okx-sub-1")
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := NewAccountScope("okx-sub-1", "bin-main")
	if err != nil {
		t.Fatal(err)
	}
	basis := foldForAccounts("PF", events, okx, claimed)

	if len(basis.Executions) != 1 || basis.Executions[0].Ref != "fill-a" {
		t.Fatalf("executions = %v, want exactly fill-a.\n\nThe transaction pass must be over the SAME "+
			"slice of the journal the position pass is. An execution from another custodian's account "+
			"would be reported as one THIS custodian never settled, on every run.", basis.Executions)
	}
	if basis.Executions[0].InstrumentID != "AAPL" || basis.Executions[0].Quantity.Cmp(big.NewRat(10, 1)) != 0 {
		t.Errorf("execution = %+v, want the AAPL +10 leg", basis.Executions[0])
	}
	if !basis.Executions[0].CustodyDate().Equal(time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("CustodyDate = %s, want the UTC midnight of the settlement date",
			basis.Executions[0].CustodyDate())
	}
}

// ONLY A TRADE IS AN EXECUTION, and an unreferenced trade is COUNTED rather than
// dropped: a shrinking transaction pass reports fewer breaks, which reads exactly
// like a book coming into agreement.
func TestCustodyBasisCountsTradesWithNoReference(t *testing.T) {
	at := time.Date(2026, 3, 4, 15, 0, 0, 0, time.UTC)
	unreferenced := execEvent("e2", "okx-sub-1", "MSFT", "", 20, at)
	cash := &Event{
		EntryID: "e3", PortfolioID: "PF", VenueAccountID: "okx-sub-1", Type: EntryCash,
		Cash: big.NewRat(5000, 1), CashCurrency: "USD", Effective: at, Knowledge: at,
		SourceRef: "transfer-agent/42",
	}
	scope, err := NewAccountScope("okx-sub-1")
	if err != nil {
		t.Fatal(err)
	}
	basis := foldForAccounts("PF", []*Event{
		execEvent("e1", "okx-sub-1", "AAPL", "fill-a", 10, at), unreferenced, cash,
	}, scope, scope)

	if len(basis.Executions) != 1 || basis.Executions[0].Ref != "fill-a" {
		t.Fatalf("executions = %v, want only fill-a.\n\nA custodian's transaction lines are TRADE "+
			"lines: putting a cash movement into this pass reports every transfer as an execution the "+
			"custodian never settled, on every run, in the queue that exists to surface the one break "+
			"meaning a fill never reached the ledger.", basis.Executions)
	}
	if basis.Unreferenced != 1 {
		t.Fatalf("Unreferenced = %d, want 1. A trade with no SourceRef cannot be matched against a "+
			"custodian trade line, so it is in the netted pass and in no transaction pass — and a "+
			"producer that stopped stamping the reference would silently move executions out of the "+
			"finer control into the coarser one with every signal staying green.", basis.Unreferenced)
	}
}

// A RESTATEMENT MUST NOT BECOME A SECOND EXECUTION. The journal legitimately
// returns a restated entry alongside the original; two rows for one economic event
// would put a reference into the pass that no custodian statement will ever match.
func TestCustodyBasisDedupesARestatedEntry(t *testing.T) {
	at := time.Date(2026, 3, 4, 15, 0, 0, 0, time.UTC)
	original := execEvent("e1", "okx-sub-1", "AAPL", "fill-a", 10, at)
	restated := execEvent("e1", "okx-sub-1", "AAPL", "fill-a", 12, at)
	restated.Knowledge = at.Add(time.Hour)

	scope, err := NewAccountScope("okx-sub-1")
	if err != nil {
		t.Fatal(err)
	}
	basis := foldForAccounts("PF", []*Event{original, restated}, scope, scope)
	if len(basis.Executions) != 1 {
		t.Fatalf("executions = %v, want 1 — a restatement is one execution seen twice, and the second "+
			"copy would be reported as one the custodian never settled on every run forever",
			basis.Executions)
	}
	// The copy kept must be the one the FOLD applied, or the break's figure
	// disagrees with the book it was computed against.
	if basis.Executions[0].Quantity.Cmp(big.NewRat(10, 1)) != 0 {
		t.Errorf("kept quantity = %s, want the copy Book.Apply folded (10)",
			basis.Executions[0].Quantity.RatString())
	}
	if p := basis.Book.Positions["AAPL"]; p == nil || p.Qty.Cmp(big.NewRat(10, 1)) != 0 {
		t.Errorf("book AAPL = %v, want 10 — the fold and the execution list disagree about the same entry", p)
	}
}
