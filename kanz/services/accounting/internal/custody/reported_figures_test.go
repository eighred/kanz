package custody

import (
	"math/big"
	"testing"

	"github.com/eighred/kanz/services/accounting/internal/recon"
)

func TestReportedFiguresSeparateAbsentZeroAndUnavailable(t *testing.T) {
	tx := recon.Transaction{ExternalRef: "fill", InstrumentID: "A", CurrencyCode: "USD", Cash: new(big.Rat)}
	got, ok := (DatedResult{reportedTransactions: []recon.Transaction{tx}}).ReportedExecutionFigures()
	if !ok || len(got) != 1 || got[0].Price != "" || got[0].Cash != "0" {
		t.Fatalf("reported fields: %+v", got)
	}
	if rows, ok := (DatedResult{reportedTransactions: []recon.Transaction{}}).ReportedExecutionFigures(); !ok || rows == nil || len(rows) != 0 {
		t.Fatal("complete zero trades lost")
	}
	for _, incomplete := range []DatedResult{{}, {reportedTransactions: make([]recon.Transaction, historicalBreakLimit+1)}} {
		if _, ok := incomplete.ReportedExecutionFigures(); ok {
			t.Fatal("unproved or oversized source presented")
		}
	}
}
