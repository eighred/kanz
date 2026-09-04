package ledger

// THE CUSTODIAN-SCOPED FOLD, OVER A REAL JOURNAL (#1006).
//
// The in-memory store proves the filtering logic. It cannot prove the thing the
// fold actually depends on: that venue_account_id round-trips through Postgres —
// the column migration 0003 added, under the RLS the estate runs with. A scoped
// fold over a column that came back empty would return an EMPTY book for every
// custodian and read as "the custodian holds everything the book does not",
// which is the defect this change removes, wearing the opposite sign.
//
// Gated on TEST_POSTGRES_URL, and the role must be NOSUPERUSER or RLS is bypassed.

import (
	"context"
	"math/big"
	"testing"
	"time"
)

func accountEntry(id, account, instrument string, qty, cash int64, at time.Time) *Event {
	return &Event{
		EntryID:        id,
		PortfolioID:    "PORT-CUST",
		VenueAccountID: account,
		Type:           EntryTrade,
		InstrumentID:   instrument,
		Quantity:       big.NewRat(qty, 1),
		Price:          big.NewRat(1, 1),
		Cash:           big.NewRat(cash, 1),
		CashCurrency:   "USD",
		Effective:      at,
		Knowledge:      at,
		SourceRef:      id,
	}
}

func TestPostgresMaterializeForAccountsScopesTheBookToOneCustodian(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	ctx := context.Background()
	at := time.Unix(1_700_000_000, 0).UTC()

	for _, e := range []*Event{
		accountEntry("c1", "okx-sub-1", "AAPL", 100, -100, at),
		accountEntry("c2", "bin-main", "MSFT", 250, -250, at),
		accountEntry("c3", "okx-sub-1", "AAPL", 20, -20, at.Add(time.Hour)),
		// Settled against NO exchange account — an investor subscription into the
		// fund's own bank. It belongs to no custodian's statement.
		{
			EntryID: "c4", PortfolioID: "PORT-CUST", Type: EntryTrade,
			Cash: big.NewRat(5000, 1), CashCurrency: "USD", Effective: at, Knowledge: at, SourceRef: "c4",
		},
	} {
		if err := st.Append(ctx, e, nil); err != nil {
			t.Fatalf("append %s: %v", e.EntryID, err)
		}
	}

	custA, err := NewAccountScope("okx-sub-1")
	if err != nil {
		t.Fatal(err)
	}
	custB, err := NewAccountScope("bin-main")
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := NewAccountScope("okx-sub-1", "bin-main")
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		scope   AccountScope
		holds   string
		qty     int64
		cash    int64
		absent  string
		custody string
	}{
		{"CUST-A", custA, "AAPL", 120, -120, "MSFT", "okx-sub-1"},
		{"CUST-B", custB, "MSFT", 250, -250, "AAPL", "bin-main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			book, unmapped, err := MaterializeForAccounts(ctx, st, "PORT-CUST", tc.scope, claimed)
			if err != nil {
				t.Fatalf("MaterializeForAccounts: %v", err)
			}
			if len(unmapped) != 0 {
				t.Errorf("unmapped = %v, want none — every account in this journal is claimed", unmapped)
			}
			p := book.Positions[tc.holds]
			if p == nil || p.Qty.Cmp(big.NewRat(tc.qty, 1)) != 0 {
				t.Errorf("%s = %v, want %d. An empty or short book here means venue_account_id did not "+
					"survive the round trip, and every custodian would reconcile against nothing",
					tc.holds, p, tc.qty)
			}
			if p := book.Positions[tc.absent]; p != nil && p.Qty.Sign() != 0 {
				t.Errorf("the scoped book holds %s, which sits at the other custodian", tc.absent)
			}
			// The un-attributed bank cash is in NEITHER custodian's book — the same
			// rule VenueAccountCash applies, and the reason the scoped books do not
			// sum to the portfolio total.
			if got := book.CashBalance("USD"); got.Cmp(big.NewRat(tc.cash, 1)) != 0 {
				t.Errorf("cash = %v, want %d — a bank entry that settled against no exchange account "+
					"must not be attributed to an exchange custodian", got, tc.cash)
			}
		})
	}

	// The whole-portfolio read is untouched: it still carries every account AND
	// the bank entry. Single-custodian portfolios depend on this.
	whole, _, err := MaterializeCurrent(ctx, st, "PORT-CUST")
	if err != nil {
		t.Fatalf("MaterializeCurrent: %v", err)
	}
	if got := whole.CashBalance("USD"); got.Cmp(big.NewRat(4630, 1)) != 0 {
		t.Errorf("whole-book cash = %v, want 4630 (−100 −250 −20 +5000)", got)
	}
	if whole.Positions["AAPL"] == nil || whole.Positions["MSFT"] == nil {
		t.Error("the whole-book path lost a holding")
	}
}

// AN ACCOUNT NO CUSTODIAN CLAIMS IS REPORTED, NOT FOLDED AWAY.
//
// Silently scoping to the claimed subset produces a book missing real holdings,
// and accounting.proto names that outcome on the statement side of the same
// comparison: "a partial level read as complete manufactures a break for every
// position it omitted, which is worse than no reconciliation at all."
func TestPostgresMaterializeForAccountsReportsAnUnclaimedAccount(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	ctx := context.Background()
	at := time.Unix(1_700_000_000, 0).UTC()

	for _, e := range []*Event{
		accountEntry("u1", "okx-sub-1", "AAPL", 100, -100, at),
		accountEntry("u2", "okx-sub-9", "TSLA", 7, -7, at),
		// An entry on a third unclaimed account that MOVES NOTHING is a
		// bookkeeping artefact, not an unattributed holding, and must not fail a
		// run on its own.
		{
			EntryID: "u3", PortfolioID: "PORT-CUST", VenueAccountID: "okx-sub-8",
			Type: EntryTrade, Effective: at, Knowledge: at, SourceRef: "u3",
		},
	} {
		if err := st.Append(ctx, e, nil); err != nil {
			t.Fatalf("append %s: %v", e.EntryID, err)
		}
	}

	claimed, err := NewAccountScope("okx-sub-1")
	if err != nil {
		t.Fatal(err)
	}
	_, unmapped, err := MaterializeForAccounts(ctx, st, "PORT-CUST", claimed, claimed)
	if err != nil {
		t.Fatalf("MaterializeForAccounts: %v", err)
	}
	if len(unmapped) != 1 || unmapped[0] != "okx-sub-9" {
		t.Errorf("unmapped = %v, want exactly [okx-sub-9]. okx-sub-8 moves nothing and must not be "+
			"reported; okx-sub-9 holds TSLA that no custodian's statement would ever cover", unmapped)
	}
}
