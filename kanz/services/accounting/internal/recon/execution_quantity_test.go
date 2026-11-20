package recon

import (
	"errors"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"math/big"
	"testing"
	"time"
)

func TestExactMatchedQuantitiesRequireComparableIndependentFigures(t *testing.T) {
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	book := []ledger.Execution{}
	stmt := Statement{Grain: GrainTransactions, BusinessDate: day}
	add := func(ref, signedInstrument string, ibor, signed *big.Rat, units bool) {
		book = append(book, ledger.Execution{EntryID: "book-" + ref, Ref: ref, TradeDate: day, InstrumentID: "A", Quantity: ibor})
		stmt.Transactions = append(stmt.Transactions, Transaction{ExternalRef: ref, InstrumentID: signedInstrument, Quantity: signed, QuantityInInstrumentUnits: units})
	}
	half := big.NewRat(1, 2)
	zero := new(big.Rat)
	add("zero", "A", zero, zero, true)
	add("fraction", "A", zero, half, true)
	add("no-source", "A", half, nil, true)
	add("no-book", "A", nil, half, true)
	add("no-unit", "A", half, zero, false)
	add("wrong-instrument", "B", half, zero, true)
	add("out-of-date", "A", half, zero, true)
	book[len(book)-1].TradeDate = day.AddDate(0, 0, -1)
	result, err := AssessExecutionQuantities(book, stmt)
	if err != nil || result.Compared != 2 || result.Unassessed != 4 || len(result.Differences) != 1 {
		t.Fatalf("comparison %+v: %v", result, err)
	}
	d := result.Differences[0]
	if d.ExternalRef != "fraction" || d.InstrumentID != "A" || d.Book.Sign() != 0 || d.Statement.Cmp(half) != 0 || d.Difference.Cmp(big.NewRat(-1, 2)) != 0 {
		t.Fatal(d)
	}
	clash := book[0]
	clash.EntryID = "distinct-book"
	if _, err := AssessExecutionQuantities(append(book, clash), stmt); !errors.Is(err, ErrAmbiguousExecutionReference) {
		t.Fatal("ambiguous book", err)
	}
	for i := range stmt.Transactions {
		stmt.Transactions[i].QuantityInInstrumentUnits = false
	}
	if q, err := AssessExecutionQuantities(book, stmt); q != nil || err != nil {
		t.Fatal("legacy asserted quantity coverage", q, err)
	}
}
