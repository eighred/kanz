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
	"strings"
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
			book, unmapped, _, err := MaterializeForAccounts(ctx, st, "PORT-CUST", tc.scope, claimed)
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
	// the bank entry. NAV, exposure and every non-reconciliation reader depend on
	// this — what changed in #1073 is which fold a CUSTODY COMPARISON uses, not
	// what the portfolio holds.
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
	_, unmapped, _, err := MaterializeForAccounts(ctx, st, "PORT-CUST", claimed, claimed)
	if err != nil {
		t.Fatalf("MaterializeForAccounts: %v", err)
	}
	if len(unmapped) != 1 || unmapped[0] != "okx-sub-9" {
		t.Errorf("unmapped = %v, want exactly [okx-sub-9]. okx-sub-8 moves nothing and must not be "+
			"reported; okx-sub-9 holds TSLA that no custodian's statement would ever cover", unmapped)
	}
}

// THE DERIVED BASIS, OVER A REAL JOURNAL (#1073).
//
// A portfolio with ONE custodian declares no accounts, so its comparison basis is
// derived from the venue_account_id column itself. That makes the round trip
// load-bearing in a way the declared path is not: a column that came back empty
// would derive an EMPTY scope, fold a book of nothing, and report every position
// the custodian holds as MISSING_IN_IBOR — the whole book as breaks, from a
// deployment that is correctly configured. The in-memory store cannot see that,
// because it never serialises the column at all.
func TestPostgresMaterializeAttributedDerivesTheScopeFromTheJournal(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	ctx := context.Background()
	at := time.Unix(1_700_000_000, 0).UTC()

	for _, e := range []*Event{
		accountEntry("d1", "okx-sub-1", "AAPL", 100, -100, at),
		accountEntry("d2", "bin-main", "MSFT", 250, -250, at),
		// Settled against NO exchange account — an investor subscription into the
		// fund's own bank. In no comparison basis, under either scope.
		{
			EntryID: "d3", PortfolioID: "PORT-CUST", Type: EntryTrade,
			Cash: big.NewRat(5000, 1), CashCurrency: "USD", Effective: at, Knowledge: at, SourceRef: "d3",
		},
		// A second one carrying a POSITION rather than cash: a holding no exchange
		// custodian will ever confirm, which the residue names separately.
		{
			EntryID: "d4", PortfolioID: "PORT-CUST", Type: EntryTrade, InstrumentID: "PRIVATE-CO",
			Quantity: big.NewRat(3, 1), Price: big.NewRat(1, 1), Cash: new(big.Rat), CashCurrency: "USD",
			Effective: at, Knowledge: at, SourceRef: "d4",
		},
	} {
		if err := st.Append(ctx, e, nil); err != nil {
			t.Fatalf("append %s: %v", e.EntryID, err)
		}
	}

	book, scope, residue, err := MaterializeAttributed(ctx, st, "PORT-CUST")
	if err != nil {
		t.Fatalf("MaterializeAttributed: %v", err)
	}
	if len(scope) != 2 || !scope.Has("okx-sub-1") || !scope.Has("bin-main") {
		t.Fatalf("derived scope = %v, want both accounts this journal touched. An EMPTY scope here "+
			"means venue_account_id did not survive the round trip, and the basis would fold to nothing "+
			"while every position the custodian holds broke as MISSING_IN_IBOR", scope)
	}
	if scope.Has("") {
		t.Error("the derived scope admits the EMPTY account id — '' is the positive declaration that " +
			"an entry settled against no exchange account, so admitting it puts the fund's own bank " +
			"cash into a custodian's book by derivation")
	}
	for _, inst := range []string{"AAPL", "MSFT"} {
		if p := book.Positions[inst]; p == nil || p.Qty.Sign() == 0 {
			t.Errorf("the derived basis lost %s, which settled against an exchange account", inst)
		}
	}
	if p := book.Positions["PRIVATE-CO"]; p != nil && p.Qty.Sign() != 0 {
		t.Errorf("the derived basis carries PRIVATE-CO (%v), which settled against no exchange account "+
			"— no custodian statement can list it, so comparing it reports a break every run", p.Qty)
	}
	// -350, not 4650: the +5000 bank entry is in no basis.
	if got := book.CashBalance("USD"); got.Cmp(big.NewRat(-350, 1)) != 0 {
		t.Errorf("basis cash = %v, want -350 (-100 -250). Carrying the un-attributed +5000 compares "+
			"4650 against a statement that can only report -350 and manufactures a 5000 cash break", got)
	}

	if residue.Entries != 2 {
		t.Errorf("residue entries = %d, want 2 — both un-attributed entries", residue.Entries)
	}
	if got := residue.Cash["USD"]; got == nil || got.Cmp(big.NewRat(5000, 1)) != 0 {
		t.Errorf("residue USD = %v, want 5000. This is the unreconciled bucket: real money in the book "+
			"of record that no custodian statement can confirm, and it has to be stated rather than "+
			"silently dropped", got)
	}
	if len(residue.Instruments) != 1 || residue.Instruments[0] != "PRIVATE-CO" {
		t.Errorf("residue instruments = %v, want [PRIVATE-CO]", residue.Instruments)
	}
	if residue.Empty() {
		t.Error("a residue holding 5000 USD and a position reports itself as empty")
	}
	if desc := residue.Describe(); !strings.Contains(desc, "USD 5000") || !strings.Contains(desc, "PRIVATE-CO") {
		t.Errorf("residue.Describe() = %q, which does not name what an operator has to look at", desc)
	}

	// The whole-portfolio read is untouched and still carries everything.
	whole, _, err := MaterializeCurrent(ctx, st, "PORT-CUST")
	if err != nil {
		t.Fatalf("MaterializeCurrent: %v", err)
	}
	if got := whole.CashBalance("USD"); got.Cmp(big.NewRat(4650, 1)) != 0 {
		t.Errorf("whole-book cash = %v, want 4650 (-100 -250 +5000) — #1073 changed which fold a "+
			"custody comparison uses, not what the portfolio holds", got)
	}
}

// A JOURNAL THAT NAMES NO EXCHANGE ACCOUNT DERIVES AN EMPTY SCOPE, AND SAYS SO.
//
// venue_account_id is optional on the cash surface and never defaulted, so this is
// a state a real deployment can be in. The ledger returns the evidence — an empty
// scope beside a non-empty residue — and custody.LedgerBookLoader is where that
// becomes a refused run rather than a book of nothing compared against a
// custodian's holdings.
func TestPostgresMaterializeAttributedReportsAnEmptyDerivedScope(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	ctx := context.Background()
	at := time.Unix(1_700_000_000, 0).UTC()

	if err := st.Append(ctx, &Event{
		EntryID: "n1", PortfolioID: "PORT-CUST", Type: EntryCash,
		Cash: big.NewRat(100000, 1), CashCurrency: "USD", Effective: at, Knowledge: at, SourceRef: "n1",
	}, nil); err != nil {
		t.Fatalf("append: %v", err)
	}

	book, scope, residue, err := MaterializeAttributed(ctx, st, "PORT-CUST")
	if err != nil {
		t.Fatalf("MaterializeAttributed: %v", err)
	}
	if len(scope) != 0 {
		t.Fatalf("derived scope = %v, want empty — no entry in this journal names an exchange account", scope)
	}
	if residue.Empty() {
		t.Fatal("the residue reports EMPTY while the journal holds 100000 USD. A caller reading it " +
			"would conclude there is nothing outside the basis and reconcile a book of nothing " +
			"against the custodian's holdings")
	}
	if got := book.CashBalance("USD"); got.Sign() != 0 {
		t.Errorf("basis cash = %v, want 0 — nothing here settled against an exchange account", got)
	}
}
