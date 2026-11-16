package ledger

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestCustodyScopeUsesOneDatedJournalSlice(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, e := range []*Event{
		{EntryID: "current", PortfolioID: "PF1", VenueAccountID: "ACC1", Type: EntryTrade, InstrumentID: "AAPL", Quantity: big.NewRat(10, 1), Cash: big.NewRat(-100, 1), CashCurrency: "USD", SourceRef: "buy", Effective: day, Knowledge: day},
		{EntryID: "future-account", PortfolioID: "PF1", VenueAccountID: "ACC2", Type: EntryTrade, InstrumentID: "MSFT", Quantity: big.NewRat(4, 1), Effective: day.AddDate(0, 0, 1), Knowledge: day},
		{EntryID: "current-bank", PortfolioID: "PF1", Type: EntryCash, Cash: big.NewRat(50, 1), CashCurrency: "USD", Effective: day, Knowledge: day},
		{EntryID: "future-bank", PortfolioID: "PF1", Type: EntryCash, Cash: big.NewRat(25, 1), CashCurrency: "USD", Effective: day.AddDate(0, 0, 1), Knowledge: day},
		{EntryID: "late-fee", PortfolioID: "PF1", VenueAccountID: "ACC1", Type: EntryFee, Cash: big.NewRat(-2, 1), CashCurrency: "USD", Effective: day.Add(time.Hour), Knowledge: day.AddDate(0, 0, 3)},
	} {
		if err := st.Append(ctx, e, nil); err != nil {
			t.Fatal(err)
		}
	}
	accounts := AccountScope{"ACC1": true}
	cutoff := day.AddDate(0, 0, 1).Add(-time.Nanosecond)
	for _, derived := range []bool{false, true} {
		name := "declared"
		if derived {
			name = "derived"
		}
		t.Run(name, func(t *testing.T) {
			var basis CustodyBasis
			var err error
			if derived {
				basis, err = MaterializeAttributed(ctx, st, "PF1", cutoff)
			} else {
				basis, err = MaterializeForAccounts(ctx, st, "PF1", accounts, accounts, cutoff)
			}
			if err != nil {
				t.Fatal(err)
			}
			if basis.Book.Positions["AAPL"].Qty.Cmp(big.NewRat(10, 1)) != 0 || basis.Book.CashBalance("USD").Cmp(big.NewRat(-102, 1)) != 0 || len(basis.Executions) != 1 || basis.Executions[0].Ref != "buy" {
				t.Fatalf("book and executions disagree with the restated D1 slice: %+v", basis)
			}
			if len(basis.Accounts) != 1 || !basis.Accounts["ACC1"] || len(basis.Unmapped) != 0 || basis.Residue.Entries != 1 || basis.Residue.Cash["USD"].Cmp(big.NewRat(50, 1)) != 0 {
				t.Fatalf("future accounts/residue leaked into the D1 slice: %+v", basis)
			}
		})
	}
	// The next date must still expose genuinely effective unclaimed holdings;
	// the cutoff cannot become a way to suppress the existing scope refusal.
	basis, err := MaterializeForAccounts(ctx, st, "PF1", accounts, accounts, day.AddDate(0, 0, 2))
	if err != nil || len(basis.Unmapped) != 1 || basis.Unmapped[0] != "ACC2" {
		t.Fatalf("effective unclaimed account was hidden: basis=%+v err=%v", basis, err)
	}
}

func TestCustodyMaterializersRefuseAMissingEffectiveCutoff(t *testing.T) {
	ctx := context.Background()
	if _, err := MaterializeAttributed(ctx, nil, "PF1", time.Time{}); err == nil || !strings.Contains(err.Error(), "cutoff") {
		t.Fatalf("derived missing cutoff=%v, want refusal before reading a store", err)
	}
	if _, err := MaterializeForAccounts(ctx, nil, "PF1", nil, nil, time.Time{}); err == nil || !strings.Contains(err.Error(), "cutoff") {
		t.Fatalf("declared missing cutoff=%v, want refusal before reading a store", err)
	}
}
