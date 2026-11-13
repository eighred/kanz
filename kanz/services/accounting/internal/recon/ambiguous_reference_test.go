package recon

import (
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

func reconcileForTest(t *testing.T, book *ledger.Book, executions []ledger.Execution, stmt Statement, tolerance *big.Rat) ([]Break, LegStatus) {
	t.Helper()
	breaks, leg, err := Reconcile(book, executions, stmt, tolerance)
	if err != nil {
		t.Fatal(err)
	}
	return breaks, leg
}

func referenceExecution(id, ref string, at time.Time) ledger.Execution {
	return ledger.Execution{EntryID: id, Ref: ref, VenueAccountID: "account", InstrumentID: "BTC-USD",
		Quantity: big.NewRat(1, 1), Price: big.NewRat(100, 1), Cash: big.NewRat(-100, 1), CashCurrency: "USD", TradeDate: at}
}

func TestTransactionReferenceIdentityMustBeUnambiguous(t *testing.T) {
	a := referenceExecution("entry-a", "ref", day(1))
	b := referenceExecution("entry-b", "ref", day(1))
	unknown := a
	unknown.EntryID = ""
	tomorrow := b
	tomorrow.TradeDate = day(2)
	settledTomorrow := b
	settledTomorrow.SettlementDate = day(2)
	settledToday := tomorrow
	settledToday.SettlementDate = day(1)
	local := b
	local.TradeDate = day(1).Add(5 * time.Hour).In(time.FixedZone("local", -5*60*60))
	for _, tc := range []struct {
		name       string
		executions []ledger.Execution
		refused    bool
	}{
		{"distinct-identities-equal-economics", []ledger.Execution{a, b}, true},
		{"permuted-identities", []ledger.Execution{b, a}, true},
		{"same-immutable-identity", []ledger.Execution{a, a}, false},
		{"unknown-first", []ledger.Execution{unknown, a}, true},
		{"unknown-second", []ledger.Execution{a, unknown}, true},
		{"both-identities-unknown", []ledger.Execution{unknown, unknown}, true},
		{"unique-reference", []ledger.Execution{a}, false},
		{"reuse-outside-window", []ledger.Execution{a, tomorrow}, false},
		{"settlement-date-outside-window", []ledger.Execution{a, settledTomorrow}, false},
		{"settlement-date-inside-window", []ledger.Execution{a, settledToday}, true},
		{"same-UTC-day-different-zone", []ledger.Execution{a, local}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			breaks, leg, err := Reconcile(ledger.NewBook("PF"), tc.executions, Statement{
				PortfolioID: "PF", BusinessDate: day(1), Grain: GrainTransactions,
				Transactions: []Transaction{{ExternalRef: "ref"}},
			}, nil)
			if tc.refused {
				if !errors.Is(err, ErrAmbiguousExecutionReference) || leg != LegRefused || leg.Ran() || breaks != nil {
					t.Fatalf("ambiguity was assessed: leg=%s breaks=%v err=%v", leg, breaks, err)
				}
			} else if err != nil || leg != LegRan || len(breaks) != 0 {
				t.Fatalf("valid identity/window changed: leg=%s breaks=%v err=%v", leg, breaks, err)
			}
		})
	}
}

func TestAmbiguousReferenceRefusesTheWholeComparison(t *testing.T) {
	book := ledger.NewBook("PF")
	book.Apply(&ledger.Event{EntryID: "trade", PortfolioID: "PF", Type: ledger.EntryTrade,
		InstrumentID: "BTC-USD", Quantity: big.NewRat(20, 1), Price: big.NewRat(1, 1), Cash: new(big.Rat)})
	book.Apply(&ledger.Event{EntryID: "cash", PortfolioID: "PF", Type: ledger.EntryCash,
		CashCurrency: "USD", Cash: big.NewRat(50, 1)})
	breaks, leg, err := Reconcile(book, []ledger.Execution{
		referenceExecution("a", "ref", day(1)), referenceExecution("b", "ref", day(1)),
	}, Statement{PortfolioID: "PF", BusinessDate: day(1), Grain: GrainTransactions}, nil)
	if !errors.Is(err, ErrAmbiguousExecutionReference) || leg.Ran() || breaks != nil {
		t.Fatalf("partial position/cash verdict escaped: %v %s %v", breaks, leg, err)
	}
	if book.Positions["BTC-USD"].Qty.Cmp(big.NewRat(20, 1)) != 0 || book.Cash["USD"].Cmp(big.NewRat(50, 1)) != 0 {
		t.Fatal("refusal mutated the book")
	}
}

func TestReferenceRefusalPreservesUnassessedGrains(t *testing.T) {
	executions := []ledger.Execution{referenceExecution("a", "ref", day(1)), referenceExecution("b", "ref", day(1))}
	for _, tc := range []struct {
		name string
		stmt Statement
		leg  LegStatus
	}{
		{"unknown", Statement{BusinessDate: day(1)}, LegGrainUnknown},
		{"balances-only", Statement{BusinessDate: day(1), Grain: GrainBalancesOnly}, LegBalancesOnly},
		{"no-business-date", Statement{Grain: GrainTransactions}, LegNoBusinessDate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			breaks, leg, err := Reconcile(ledger.NewBook("PF"), executions, tc.stmt, nil)
			if err != nil || leg != tc.leg || len(breaks) != 0 {
				t.Fatalf("unassessed grain changed: %s %v %v", leg, breaks, err)
			}
		})
	}
}

func TestReferenceDiagnosticIsBoundedWithoutChangingIdentity(t *testing.T) {
	prefix := strings.Repeat("r", 200)
	a, b := referenceExecution("a", prefix+"a", day(1)), referenceExecution("b", prefix+"b", day(1))
	stmt := Statement{PortfolioID: "PF", BusinessDate: day(1), Grain: GrainTransactions,
		Transactions: []Transaction{{ExternalRef: a.Ref}, {ExternalRef: b.Ref}}}
	if breaks, _, err := Reconcile(ledger.NewBook("PF"), []ledger.Execution{a, b}, stmt, nil); err != nil || len(breaks) != 0 {
		t.Fatalf("diagnostic prefix was used as identity: %v %v", breaks, err)
	}
	b.Ref = a.Ref
	_, _, err := Reconcile(ledger.NewBook("PF"), []ledger.Execution{a, b}, stmt, nil)
	if !errors.Is(err, ErrAmbiguousExecutionReference) || len(err.Error()) > 200 {
		t.Fatalf("unbounded/untyped diagnostic: %v", err)
	}
}
