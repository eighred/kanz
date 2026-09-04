package custody

// THE BOOK SIDE MUST BE SCOPED TO THE CUSTODIAN IT IS COMPARED AGAINST (#1006).
//
// custody.Subject is (portfolio, custodian, business date). Every part of this
// control was custodian-aware — the break id, the run id, the partition key, the
// staleness gauge, the scheduler's pair list, the wire message. The book side was
// not: it loaded the WHOLE portfolio and handed it to recon.Reconcile against ONE
// custodian's statement.
//
// For a portfolio custodied in two places — which ACCOUNTING_CUSTODY_PAIRS accepts
// and the Scheduler is built to iterate — custodian A's run reports every position
// held at B as MISSING_AT_CUSTODIAN, and B's run reports every position held at A
// the same way. Every position in the book becomes a break, twice, and the one
// break that means a fill never reached the ledger is buried in it.
//
// There was NO test with two custodians on one portfolio before this file.

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

// seedEntry appends one position-and-cash entry settled against an exchange account.
func seedEntry(t *testing.T, st ledger.Store, id, portfolio, account, instrument string, qty, cash int64) {
	t.Helper()
	e := &ledger.Event{
		EntryID:        id,
		PortfolioID:    portfolio,
		VenueAccountID: account,
		Type:           ledger.EntryTrade,
		InstrumentID:   instrument,
		Quantity:       big.NewRat(qty, 1),
		Price:          big.NewRat(1, 1),
		Cash:           big.NewRat(cash, 1),
		CashCurrency:   "USD",
		Effective:      t0,
		Knowledge:      t0,
	}
	if err := st.Append(context.Background(), e, nil); err != nil {
		t.Fatalf("Append %s: %v", id, err)
	}
}

// twoCustodianScope is the configuration the defect lives in: one portfolio, two
// custodians, each holding its own exchange account.
func twoCustodianScope(t *testing.T) *BookScope {
	t.Helper()
	scope, err := NewBookScope(
		[]Subject{
			{PortfolioID: "PF1", CustodianID: "CUST-A"},
			{PortfolioID: "PF1", CustodianID: "CUST-B"},
		},
		map[string]map[string][]string{
			"PF1": {"CUST-A": {"okx-sub-1"}, "CUST-B": {"bin-main"}},
		})
	if err != nil {
		t.Fatalf("NewBookScope: %v", err)
	}
	return scope
}

// THE TEST THE DEFECT FAILS.
//
// PF1 holds AAPL at CUST-A (okx-sub-1) and MSFT at CUST-B (bin-main). Each
// custodian's statement lists exactly what that custodian holds — which is the
// truth. A correct control finds NO breaks on either side. The pre-#1006 loader
// compares the whole book each time and reports MSFT missing at A and AAPL
// missing at B.
func TestEachCustodiansBookHoldsOnlyItsOwnAccounts(t *testing.T) {
	ctx := context.Background()
	st := ledger.NewMemoryStore()
	seedEntry(t, st, "e-aapl", "PF1", "okx-sub-1", "AAPL", 100, -100)
	seedEntry(t, st, "e-msft", "PF1", "bin-main", "MSFT", 250, -250)

	load := LedgerBookLoader(st, twoCustodianScope(t))

	for _, tc := range []struct {
		custodian string
		want      map[string]int64
		absent    string
	}{
		{"CUST-A", map[string]int64{"AAPL": 100}, "MSFT"},
		{"CUST-B", map[string]int64{"MSFT": 250}, "AAPL"},
	} {
		book, err := load(ctx, Subject{PortfolioID: "PF1", CustodianID: tc.custodian, BusinessDate: t0})
		if err != nil {
			t.Fatalf("%s: load: %v", tc.custodian, err)
		}
		for inst, qty := range tc.want {
			p := book.Positions[inst]
			if p == nil || p.Qty.Cmp(big.NewRat(qty, 1)) != 0 {
				t.Errorf("%s: book holds %v for %s, want %d", tc.custodian, p, inst, qty)
			}
		}
		if p := book.Positions[tc.absent]; p != nil && p.Qty.Sign() != 0 {
			t.Errorf("%s: book holds %s (%v), which is custodied at the OTHER custodian — comparing it "+
				"against this custodian's statement reports it MISSING_AT_CUSTODIAN, and every position "+
				"in the book becomes a break",
				tc.custodian, tc.absent, p.Qty)
		}
	}
}

// The end-to-end claim, through the real Reconciler: two custodians, two true
// statements, ZERO breaks.
func TestTwoCustodiansOnOnePortfolioReconcileClean(t *testing.T) {
	ctx := context.Background()
	ledgerStore := ledger.NewMemoryStore()
	seedEntry(t, ledgerStore, "e-aapl", "PF1", "okx-sub-1", "AAPL", 100, -100)
	seedEntry(t, ledgerStore, "e-msft", "PF1", "bin-main", "MSFT", 250, -250)

	store := NewMemoryStore()
	// Each custodian states exactly what it holds — cash included, since the
	// scoped fold carries the cash leg of the entries it kept.
	for _, s := range []struct {
		id        string
		custodian string
		positions map[string]int64
		cash      map[string]int64
	}{
		{"S-A", "CUST-A", map[string]int64{"AAPL": 100}, map[string]int64{"USD": -100}},
		{"S-B", "CUST-B", map[string]int64{"MSFT": 250}, map[string]int64{"USD": -250}},
	} {
		stmt := statement(s.id, s.positions, s.cash)
		stmt.CustodianID = s.custodian
		if err := store.SaveStatement(ctx, stmt); err != nil {
			t.Fatalf("SaveStatement %s: %v", s.id, err)
		}
	}

	r, err := NewReconciler(store, LedgerBookLoader(ledgerStore, twoCustodianScope(t)),
		&capturePublisher{}, new(big.Rat), nil, nil, func() time.Time { return t0 })
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}

	for _, custodian := range []string{"CUST-A", "CUST-B"} {
		run, err := r.Reconcile(ctx, Subject{PortfolioID: "PF1", CustodianID: custodian, BusinessDate: t0})
		if err != nil {
			t.Fatalf("%s: Reconcile: %v", custodian, err)
		}
		if run.Outcome != OutcomeClean {
			t.Errorf("%s: outcome = %v with %d break(s), want CLEAN. Each custodian's statement is the "+
				"truth about what IT holds; a break here means the book side was not scoped to it, so the "+
				"other custodian's holdings are being reported missing.\n  breaks: %+v",
				custodian, run.Outcome, len(run.Breaks), run.Breaks)
		}
	}
}

// AN ACCOUNT NOBODY CLAIMS FAILS THE RUN, LOUDLY.
//
// accounting.proto says it on the statement side of this same comparison: "a
// partial level read as complete manufactures a break for every position it
// omitted, which is worse than no reconciliation at all because it buries the
// real breaks in noise." An unclaimed account is that on the book side.
func TestAnUnclaimedExchangeAccountFailsTheRun(t *testing.T) {
	ctx := context.Background()
	ledgerStore := ledger.NewMemoryStore()
	seedEntry(t, ledgerStore, "e-aapl", "PF1", "okx-sub-1", "AAPL", 100, -100)
	seedEntry(t, ledgerStore, "e-msft", "PF1", "bin-main", "MSFT", 250, -250)
	// A third account the declaration never mentions — a sub-account opened and
	// not added to the config.
	seedEntry(t, ledgerStore, "e-tsla", "PF1", "okx-sub-9", "TSLA", 7, -7)

	store := NewMemoryStore()
	stmt := statement("S-A", map[string]int64{"AAPL": 100}, map[string]int64{"USD": -100})
	if err := store.SaveStatement(ctx, stmt); err != nil {
		t.Fatalf("SaveStatement: %v", err)
	}

	r, err := NewReconciler(store, LedgerBookLoader(ledgerStore, twoCustodianScope(t)),
		&capturePublisher{}, new(big.Rat), nil, nil, func() time.Time { return t0 })
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	run, err := r.Reconcile(ctx, Subject{PortfolioID: "PF1", CustodianID: "CUST-A", BusinessDate: t0})
	if err == nil {
		t.Fatal("Reconcile succeeded over a book missing an unclaimed account's holdings — it would " +
			"break every position that account holds, at every custodian")
	}
	if run.Outcome != OutcomeFailed {
		t.Errorf("outcome = %v, want FAILED — a run that could not be trusted must be recorded as one, "+
			"or it is indistinguishable from a run that never came due", run.Outcome)
	}
	if !strings.Contains(err.Error(), "okx-sub-9") {
		t.Errorf("the failure does not name the unclaimed account, so an operator cannot act on it: %v", err)
	}
	if strings.Contains(err.Error(), "okx-sub-1") || strings.Contains(err.Error(), "bin-main") {
		t.Errorf("the failure names a CLAIMED account, which sends the operator to the wrong config line: %v", err)
	}
}

// A single-custodian portfolio does not move: the whole book against its one
// custodian is correct, needs no declaration, and is what every existing
// deployment gets. Only the configuration that was wrong changes.
func TestASingleCustodianPortfolioStillComparesTheWholeBook(t *testing.T) {
	ctx := context.Background()
	st := ledger.NewMemoryStore()
	seedEntry(t, st, "e-aapl", "PF1", "okx-sub-1", "AAPL", 100, -100)
	seedEntry(t, st, "e-msft", "PF1", "bin-main", "MSFT", 250, -250)
	// An entry settled against NO exchange account — an investor subscription
	// into the fund's own bank. The whole-book path must still carry it.
	if err := st.Append(ctx, &ledger.Event{
		EntryID: "e-sub", PortfolioID: "PF1", Type: ledger.EntryTrade,
		Cash: big.NewRat(1000, 1), CashCurrency: "USD", Effective: t0, Knowledge: t0,
	}, nil); err != nil {
		t.Fatalf("Append: %v", err)
	}

	scope, err := NewBookScope([]Subject{{PortfolioID: "PF1", CustodianID: "CUST-A"}}, nil)
	if err != nil {
		t.Fatalf("NewBookScope: %v", err)
	}
	if scope.Scoped("PF1") {
		t.Fatal("a portfolio with one custodian and no declaration is scoped — its book would shrink " +
			"to a subset nobody asked for")
	}
	book, err := LedgerBookLoader(st, scope)(ctx, Subject{PortfolioID: "PF1", CustodianID: "CUST-A"})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, inst := range []string{"AAPL", "MSFT"} {
		if p := book.Positions[inst]; p == nil || p.Qty.Sign() == 0 {
			t.Errorf("the whole-book path lost %s", inst)
		}
	}
	if got := book.CashBalance("USD"); got.Cmp(big.NewRat(650, 1)) != 0 {
		t.Errorf("cash = %v, want 650 (−100 −250 +1000) — the un-attributed bank entry belongs in the "+
			"whole-book path", got)
	}
}

// THE CONFIGURATIONS THAT MUST NOT START.
//
// Each is a state that would otherwise produce a confident wrong answer at the
// first tick. A misconfiguration must surface on the first event, not as a break
// queue somebody has to interpret.
func TestNewBookScopeRefusesAConfigurationItCannotJustify(t *testing.T) {
	twoPairs := []Subject{
		{PortfolioID: "PF1", CustodianID: "CUST-A"},
		{PortfolioID: "PF1", CustodianID: "CUST-B"},
	}
	for _, tc := range []struct {
		name     string
		pairs    []Subject
		accounts map[string]map[string][]string
		wantIn   string
	}{
		{
			name:   "two custodians, nothing declared — the defect itself, arriving as config",
			pairs:  twoPairs,
			wantIn: "declares no exchange accounts",
		},
		{
			name:     "two custodians, only one declared",
			pairs:    twoPairs,
			accounts: map[string]map[string][]string{"PF1": {"CUST-A": {"okx-sub-1"}}},
			wantIn:   "declares no exchange accounts",
		},
		{
			name:  "one account claimed by both — the holding counts twice, so neither can break",
			pairs: twoPairs,
			accounts: map[string]map[string][]string{
				"PF1": {"CUST-A": {"okx-sub-1"}, "CUST-B": {"okx-sub-1"}},
			},
			wantIn: "claimed by more than one custodian",
		},
		{
			name:     "a declaration for a portfolio that reconciles against nothing",
			pairs:    []Subject{{PortfolioID: "PF1", CustodianID: "CUST-A"}},
			accounts: map[string]map[string][]string{"PF2": {"CUST-A": {"okx-sub-1"}}},
			wantIn:   "not in ACCOUNTING_CUSTODY_PAIRS",
		},
		{
			name:     "a declaration for a custodian that is not a configured pair",
			pairs:    []Subject{{PortfolioID: "PF1", CustodianID: "CUST-A"}},
			accounts: map[string]map[string][]string{"PF1": {"CUST-Z": {"okx-sub-1"}}},
			wantIn:   "not a configured pair",
		},
		{
			name:     "an empty account id would claim the fund's own bank cash",
			pairs:    []Subject{{PortfolioID: "PF1", CustodianID: "CUST-A"}},
			accounts: map[string]map[string][]string{"PF1": {"CUST-A": {""}}},
			wantIn:   "settled against no exchange account",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewBookScope(tc.pairs, tc.accounts)
			if err == nil {
				t.Fatal("NewBookScope accepted it — the service starts and the control asserts a wrong answer")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("refusal does not say why (want %q): %v", tc.wantIn, err)
			}
		})
	}
}

func TestParseCustodyAccounts(t *testing.T) {
	got, err := ParseCustodyAccounts([]string{"PF1:CUST-A:okx-sub-1,okx-sub-2", " PF1:CUST-B:bin-main "})
	if err != nil {
		t.Fatalf("ParseCustodyAccounts: %v", err)
	}
	if len(got["PF1"]["CUST-A"]) != 2 || got["PF1"]["CUST-A"][1] != "okx-sub-2" {
		t.Errorf("CUST-A accounts = %v", got["PF1"]["CUST-A"])
	}
	if len(got["PF1"]["CUST-B"]) != 1 || got["PF1"]["CUST-B"][0] != "bin-main" {
		t.Errorf("CUST-B accounts = %v", got["PF1"]["CUST-B"])
	}

	// A MALFORMED ENTRY IS AN ERROR AND NOT A SKIP: dropping one would un-scope a
	// custodian while the service reported a healthy start.
	for _, bad := range []string{
		"PF1:CUST-A",             // no accounts
		"PF1:CUST-A:okx:extra",   // too many segments
		":CUST-A:okx-sub-1",      // no portfolio
		"PF1::okx-sub-1",         // no custodian
		"PF1:CUST-A:okx-sub-1,,", // an empty account id
	} {
		if _, err := ParseCustodyAccounts([]string{bad}); err == nil {
			t.Errorf("ParseCustodyAccounts(%q) was accepted", bad)
		}
	}
	if _, err := ParseCustodyAccounts([]string{"PF1:CUST-A:a", "PF1:CUST-A:b"}); err == nil {
		t.Error("a duplicated (portfolio, custodian) was accepted — one account set would win silently")
	}
}
