package recon

import (
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

// THE DEFECT, IN ONE TEST (#1049).
//
// Twelve fills on one instrument. The custodian settled eleven of them, never saw
// fill-7 (+50), and settled an execution the book has never heard of (X9, +50, the
// same instrument, the same size). The NETTED position and the NETTED cash agree
// EXACTLY, so the four balance-grain break kinds report the line as fully correct
// — one wrongly-booked execution, invisible.
//
// This is the third of the three costs #1049 names: "a wrongly-booked execution
// from a missing one, when both touch the same instrument". It is the honest
// demonstration, and it is worth being precise about why: eleven correct fills do
// NOT "net away" a twelfth that simply never settled — that one moves the total
// and the netted pass does catch it, as a QUANTITY break naming the instrument
// rather than the fill. What the netted pass cannot see is a difference that
// CANCELS, and one execution replaced by another of the same size on the same
// instrument is exactly that.

// twelveFills builds the book, its executions, and the netted totals the custodian
// would report. fill-7 is the one the custodian never settled.
func twelveFills(t *testing.T) (*ledger.Book, []ledger.Execution, *big.Rat, *big.Rat) {
	t.Helper()
	quantities := []string{"100", "-40", "60", "-20", "80", "-10", "50", "30", "-70", "90", "-25", "15"}
	var (
		events     []*ledger.Event
		executions []ledger.Execution
		net        = new(big.Rat)
		cash       = new(big.Rat)
	)
	for i, q := range quantities {
		id := "fill-" + string(rune('1'+i))
		ev := buy(id, "AAPL", q, "150")
		ev.SourceRef = id
		ev.SettlementBasis = ledger.SettlementSettled
		ev.SettlementDate = day(1)
		events = append(events, ev)
		executions = append(executions, ledger.Execution{
			Ref: id, EntryID: id, InstrumentID: "AAPL", Quantity: dec.Rat(q), Price: dec.Rat("150"),
			Cash: ev.Cash, CashCurrency: "USD", VenueAccountID: "okx-sub-1",
			TradeDate: day(1), SettlementBasis: ledger.SettlementSettled, SettlementDate: day(1),
		})
		net.Add(net, dec.Rat(q))
		cash.Add(cash, ev.Cash)
	}
	return ledger.Replay("PF", events), executions, net, cash
}

// custodianLines is the custodian's trade lines: every fill except fill-7, plus
// X9 which the book does not hold.
func custodianLines() []Transaction {
	var out []Transaction
	for i := 0; i < 12; i++ {
		id := "fill-" + string(rune('1'+i))
		if id == "fill-7" {
			continue
		}
		out = append(out, Transaction{ExternalRef: id, InstrumentID: "AAPL", SettlementDate: day(1)})
	}
	return append(out, Transaction{
		ExternalRef: "X9", InstrumentID: "AAPL", Quantity: dec.Rat("50"), SettlementDate: day(1),
	})
}

// TestTransactionReconIsBlindAtTheNettedGrain is the PRE-FIX behaviour, kept as a
// test rather than a comment: with no transaction lines the comparison is CLEAN,
// and that is exactly what it was before this issue.
func TestTransactionReconIsBlindAtTheNettedGrain(t *testing.T) {
	book, executions, net, cash := twelveFills(t)

	breaks, leg := Reconcile(book, executions, Statement{
		PortfolioID:  "PF",
		BusinessDate: day(1),
		Positions:    map[string]*big.Rat{"AAPL": net},
		Cash:         map[string]*big.Rat{"USD": cash},
		// No grain asserted — the state every statement written before #1049 is in.
	}, nil)

	if len(breaks) != 0 {
		t.Fatalf("the netted comparison found %v, want NONE — this test exists to hold the "+
			"demonstration that the totals agree", breaks)
	}
	if leg.Ran() {
		t.Fatalf("the transaction pass reported that it RAN over a statement that declared no grain "+
			"(%s). An empty transaction list decides nothing: reading it as 'no transactions occurred' "+
			"reports every execution in the window as one the custodian never saw", leg)
	}
	if leg != LegGrainUnknown {
		t.Errorf("leg = %s, want %s. 'Nobody said what this feed supplies' and 'this feed supplies "+
			"balances only' are different answers and the run must not collapse them", leg, LegGrainUnknown)
	}
}

// TestTransactionReconNamesTheExecutionTheCustodianNeverSaw is the fix: the same
// book, the same agreeing totals, and the transaction pass names both sides of the
// mis-booking by REFERENCE.
func TestTransactionReconNamesTheExecutionTheCustodianNeverSaw(t *testing.T) {
	book, executions, net, cash := twelveFills(t)

	breaks, leg := Reconcile(book, executions, Statement{
		PortfolioID:  "PF",
		BusinessDate: day(1),
		Positions:    map[string]*big.Rat{"AAPL": net},
		Cash:         map[string]*big.Rat{"USD": cash},
		Transactions: custodianLines(),
		Grain:        GrainTransactions,
	}, nil)

	if !leg.Ran() {
		t.Fatalf("the transaction pass did not run (%s) over a statement declaring %s", leg, GrainTransactions)
	}
	byKey := map[string]Break{}
	for _, b := range breaks {
		byKey[b.Key] = b
	}
	missingAtCustodian, ok := byKey["fill-7"]
	if !ok {
		t.Fatalf("no break names fill-7. The book holds it, the custodian's trade lines do not, and the "+
			"netted totals agree — so this is the ONLY signal that an execution was booked against "+
			"something the custodian never settled. Got %d break(s): %v", len(breaks), breaks)
	}
	if missingAtCustodian.Kind != BreakExecutionMissingAtCustodian {
		t.Errorf("fill-7 broke as %s, want %s", missingAtCustodian.Kind, BreakExecutionMissingAtCustodian)
	}
	if missingAtCustodian.IBOR.Cmp(dec.Rat("50")) != 0 {
		t.Errorf("fill-7 IBOR figure = %s, want 50 — the break must name an amount an operator can act on",
			missingAtCustodian.IBOR.FloatString(2))
	}
	missingInBook, ok := byKey["X9"]
	if !ok {
		t.Fatalf("no break names X9. The custodian settled it and the book has no entry for it: THE "+
			"DIRECTION THAT MEANS A FILL NEVER REACHED THE BOOK, and the one this platform can detect "+
			"no other way. Got %d break(s): %v", len(breaks), breaks)
	}
	if missingInBook.Kind != BreakExecutionMissingInIBOR {
		t.Errorf("X9 broke as %s, want %s", missingInBook.Kind, BreakExecutionMissingInIBOR)
	}
	// The netted arms must be unchanged: the totals still agree, and inventing a
	// position break here would be the transaction pass double-reporting one
	// difference under two kinds, two ages and two owners.
	for _, b := range breaks {
		switch b.Kind {
		case BreakQuantity, BreakMissingAtCustodian, BreakMissingInIBOR, BreakCash:
			t.Errorf("the netted arms reported %s for %s, and the totals agree exactly", b.Kind, b.Key)
		}
	}
}

// TestTransactionReconEmptyIsNotNone is the UNKNOWN-honest half: an empty
// transaction list means three different things and only one of them is a
// comparison.
func TestTransactionReconEmptyIsNotNone(t *testing.T) {
	book, executions, net, cash := twelveFills(t)
	base := Statement{
		PortfolioID:  "PF",
		BusinessDate: day(1),
		Positions:    map[string]*big.Rat{"AAPL": net},
		Cash:         map[string]*big.Rat{"USD": cash},
	}

	for _, tc := range []struct {
		name       string
		grain      Grain
		wantLeg    LegStatus
		wantBreaks int
	}{
		{"nobody asserted a grain", GrainUnknown, LegGrainUnknown, 0},
		{"the custodian sends balances only", GrainBalancesOnly, LegBalancesOnly, 0},
		// The ONLY reading under which an empty list is a claim: the custodian
		// asserts its trade lines are complete, and it reported none — so every
		// execution the book holds in the window is one it never saw.
		{"the custodian asserts a complete and empty set", GrainTransactions, LegRan, 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stmt := base
			stmt.Grain = tc.grain
			breaks, leg := Reconcile(book, executions, stmt, nil)
			if leg != tc.wantLeg {
				t.Errorf("leg = %s, want %s", leg, tc.wantLeg)
			}
			if len(breaks) != tc.wantBreaks {
				t.Fatalf("got %d break(s), want %d.\n\nAn empty transaction list is not self-describing: "+
					"under %s it means 'this feed sends no trade lines' and under %s it means 'the "+
					"custodian settled nothing that day'. Collapsing the two either fabricates a break "+
					"for every execution the book holds, or silently stops comparing executions at all.",
					len(breaks), tc.wantBreaks, GrainBalancesOnly, GrainTransactions)
			}
		})
	}
}

// TestTransactionReconWindowsTheBookByBusinessDate: the book side is the WHOLE
// comparison basis and a statement covers ONE date. Without the window the first
// run would report the fund's entire trading history as executions the custodian
// never saw.
func TestTransactionReconWindowsTheBookByBusinessDate(t *testing.T) {
	book, executions, net, cash := twelveFills(t)
	// One more execution, settled the day BEFORE the statement's business date and
	// therefore on no trade line the custodian sent for day 1.
	executions = append(executions, ledger.Execution{
		Ref: "fill-yesterday", EntryID: "fill-yesterday", InstrumentID: "AAPL",
		Quantity: dec.Rat("10"), VenueAccountID: "okx-sub-1",
		TradeDate: day(0), SettlementBasis: ledger.SettlementSettled, SettlementDate: day(0),
	})

	breaks, leg := Reconcile(book, executions, Statement{
		PortfolioID:  "PF",
		BusinessDate: day(1),
		Positions:    map[string]*big.Rat{"AAPL": net},
		Cash:         map[string]*big.Rat{"USD": cash},
		Transactions: custodianLines(),
		Grain:        GrainTransactions,
	}, nil)
	if !leg.Ran() {
		t.Fatalf("leg = %s, want it to run", leg)
	}
	for _, b := range breaks {
		if b.Key == "fill-yesterday" {
			t.Fatalf("an execution that settled on %s was reported against a statement for %s.\n\n"+
				"The book side is the whole comparison basis and a statement covers ONE business date. "+
				"Unwindowed, the first run reports every execution the fund has ever made as one the "+
				"custodian never saw — the real break buried under the entire trading history.",
				day(0).Format("2006-01-02"), day(1).Format("2006-01-02"))
		}
	}
}

// TestTransactionReconRefusesAWindowlessStatement: the ad-hoc reconcile route's
// statement comes from a request body and names no business date. The pass must
// not run rather than compare a whole journal against it.
func TestTransactionReconRefusesAWindowlessStatement(t *testing.T) {
	book, executions, net, cash := twelveFills(t)
	breaks, leg := Reconcile(book, executions, Statement{
		PortfolioID:  "PF",
		Positions:    map[string]*big.Rat{"AAPL": net},
		Cash:         map[string]*big.Rat{"USD": cash},
		Transactions: custodianLines(),
		Grain:        GrainTransactions,
	}, nil)
	if leg != LegNoBusinessDate {
		t.Fatalf("leg = %s, want %s — a statement with no business date has no window, and matching a "+
			"whole journal against it reports every execution as missing at the custodian", leg, LegNoBusinessDate)
	}
	if len(breaks) != 0 {
		t.Fatalf("got %d break(s) from a windowless statement, want none", len(breaks))
	}
}

// TestTransactionReconUsesTheTradeDateWhenNoSettlementDateIsAsserted holds the
// #1043 fallback: an entry whose producer asserted no settlement date is windowed
// by its trade date rather than dropped out of every comparison.
func TestTransactionReconUsesTheTradeDateWhenNoSettlementDateIsAsserted(t *testing.T) {
	unstated := ledger.Execution{
		Ref: "fill-unstated", EntryID: "fill-unstated", InstrumentID: "AAPL",
		Quantity: dec.Rat("10"), VenueAccountID: "okx-sub-1",
		TradeDate: day(1).Add(9 * time.Hour), // no settlement basis, no settlement date
	}
	if got := unstated.CustodyDate(); !got.Equal(day(1)) {
		t.Fatalf("CustodyDate = %s, want %s. An entry whose producer asserted no settlement date has "+
			"exactly one economic day anybody stated about it, and windowing on a date nobody set "+
			"would silently drop it out of every transaction comparison",
			got.Format("2006-01-02"), day(1).Format("2006-01-02"))
	}
	breaks, leg := Reconcile(ledger.NewBook("PF"), []ledger.Execution{unstated}, Statement{
		PortfolioID: "PF", BusinessDate: day(1), Grain: GrainTransactions,
	}, nil)
	if !leg.Ran() || len(breaks) != 1 || breaks[0].Key != "fill-unstated" {
		t.Fatalf("leg = %s, breaks = %v — the trade-date fallback must put the execution IN the window", leg, breaks)
	}
}

// TestBreakKindsCoversTheTransactionGrain: the metric labels, the stored kind
// column and the wire enum all derive from BreakKinds(), so a kind omitted from it
// exports NO series and round-trips through storage as an unknown.
func TestBreakKindsCoversTheTransactionGrain(t *testing.T) {
	have := map[BreakKind]bool{}
	for _, k := range BreakKinds() {
		have[k] = true
	}
	for _, k := range []BreakKind{BreakExecutionMissingAtCustodian, BreakExecutionMissingInIBOR} {
		if !have[k] {
			t.Errorf("BreakKinds() omits %s. Metrics.SeedCustodian seeds the open-breaks gauge from it, "+
				"so the kind would export no series until its first occurrence — and a rule over an "+
				"absent series never fires, which means the alert is silent in exactly the state it "+
				"exists to detect.", k)
		}
		if k.String() == "unknown" {
			t.Errorf("break kind %d renders as \"unknown\" — that string is the stored kind column and "+
				"every metric label", int(k))
		}
	}
}
