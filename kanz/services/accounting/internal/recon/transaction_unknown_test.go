package recon

import (
	"testing"

	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

func TestMissingExecutionQuantityRemainsUnknown(t *testing.T) {
	book := ledger.NewBook("PF")
	breaks, leg := Reconcile(book, nil, Statement{PortfolioID: "PF", BusinessDate: day(1), Grain: GrainTransactions, Transactions: []Transaction{{ExternalRef: "unknown-quantity"}}}, nil)
	if !leg.Ran() || len(breaks) != 1 {
		t.Fatalf("reference comparison disappeared: %v %v", leg, breaks)
	}
	if breaks[0].Custodian != nil || breaks[0].Diff != nil {
		t.Fatal("unknown execution quantity became a measured zero")
	}
}
