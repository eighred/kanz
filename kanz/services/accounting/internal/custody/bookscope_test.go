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
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

// captureLogger records what the loader said, so the residue statement is
// asserted rather than assumed. A test that only checks the numbers cannot tell a
// correct exclusion from a silent one, and "nothing reconciles this money" is the
// half of #1073 that neither of the two disagreeing rules said out loud.
type captureLogger struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *captureLogger) Enabled(context.Context, slog.Level) bool { return true }
func (c *captureLogger) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r)
	return nil
}
func (c *captureLogger) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *captureLogger) WithGroup(string) slog.Handler      { return c }

// sawAt reports whether a record at level contains needle in its message or in
// any of its attribute values.
func (c *captureLogger) sawAt(level slog.Level, needle string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.records {
		if r.Level != level {
			continue
		}
		if strings.Contains(r.Message, needle) {
			return true
		}
		found := false
		r.Attrs(func(a slog.Attr) bool {
			if strings.Contains(a.Value.String(), needle) {
				found = true
			}
			return !found
		})
		if found {
			return true
		}
	}
	return false
}

func (c *captureLogger) logger() *slog.Logger { return slog.New(c) }

func testLogger() *slog.Logger { return slog.New(&captureLogger{}) }

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

	load := LedgerBookLoader(st, twoCustodianScope(t), testLogger())

	for _, tc := range []struct {
		custodian string
		want      map[string]int64
		absent    string
	}{
		{"CUST-A", map[string]int64{"AAPL": 100}, "MSFT"},
		{"CUST-B", map[string]int64{"MSFT": 250}, "AAPL"},
	} {
		book, _, err := load(ctx, Subject{PortfolioID: "PF1", CustodianID: tc.custodian, BusinessDate: t0})
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

	r, err := NewReconciler(store, LedgerBookLoader(ledgerStore, twoCustodianScope(t), testLogger()),
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

	r, err := NewReconciler(store, LedgerBookLoader(ledgerStore, twoCustodianScope(t), testLogger()),
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

// THE COMPARISON BASIS EXCLUDES AN ENTRY THAT SETTLED AGAINST NO EXCHANGE
// ACCOUNT, AT ONE CUSTODIAN EXACTLY AS AT SEVERAL (#1073).
//
// This test replaces TestASingleCustodianPortfolioStillComparesTheWholeBook,
// which asserted the opposite and was the SPECIFICATION for the defect. That test
// seeded the same +1000 USD entry, called it "an investor subscription into the
// fund's own bank" in its own comment, and required the single-custodian book to
// carry it. Both claims cannot be true: an exchange custodian's statement
// describes what is at that exchange, so a subscription sitting in the fund's own
// bank is not on it, and a basis that carries it reports the whole 1000 as a cash
// break every run. The rule kept is ledger.MaterializeForAccounts's — un-attributed
// entries are in NO custodian's basis — and the single-custodian path now derives
// its scope from the accounts the journal touched rather than taking the whole book.
//
// What has NOT changed, and is asserted here so the repair cannot quietly shrink
// the basis further: every entry that DID settle against an exchange account is
// still in the one custodian's book, with no declaration required.
func TestASingleCustodianBookExcludesAnUnattributedEntry(t *testing.T) {
	ctx := context.Background()
	st := ledger.NewMemoryStore()
	seedEntry(t, st, "e-aapl", "PF1", "okx-sub-1", "AAPL", 100, -100)
	seedEntry(t, st, "e-msft", "PF1", "bin-main", "MSFT", 250, -250)
	// An entry settled against NO exchange account — an investor subscription into
	// the fund's own bank. No exchange statement can report it, so no custodian's
	// comparison basis may contain it.
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
	if scope.Declared("PF1") {
		t.Fatal("a portfolio with one custodian and no declaration reports a DECLARED scope — the " +
			"loader would then ask for accounts nobody declared and refuse the run")
	}
	book, _, err := LedgerBookLoader(st, scope, testLogger())(ctx, Subject{PortfolioID: "PF1", CustodianID: "CUST-A"})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, inst := range []string{"AAPL", "MSFT"} {
		if p := book.Positions[inst]; p == nil || p.Qty.Sign() == 0 {
			t.Errorf("the single-custodian basis lost %s, which DID settle against an exchange account "+
				"— its one custodian holds every account the journal touched, so dropping it would "+
				"report the position MISSING_IN_IBOR against a statement that lists it", inst)
		}
	}
	if got := book.CashBalance("USD"); got.Cmp(big.NewRat(-350, 1)) != 0 {
		t.Errorf("cash = %v, want -350 (-100 -250) — the +1000 that settled against NO exchange account "+
			"is in no custodian's basis. Carrying it compares 650 against a statement that can only "+
			"report -350 and manufactures a 1000 cash break on the DEFAULT configuration", got)
	}
}

// THE FABRICATED BREAK, END TO END, THROUGH THE REAL RECONCILER (#1073).
//
// One custodian, no declaration — the default configuration and the shape of
// every existing deployment. The custodian states exactly what it holds. The only
// disagreement is an entry the custodian cannot see, and before this issue the
// run reported it as a cash break for the full amount, every run, forever.
//
// An ops team that learns the break queue carries routine false positives stops
// reading it, and the true break — a real position discrepancy at the custodian —
// then arrives in a queue nobody trusts. That is the failure mode custody
// reconciliation exists to prevent, so a clean run here is the whole point.
func TestAnUnattributedEntryFabricatesNoBreakAtASingleCustodian(t *testing.T) {
	ctx := context.Background()
	ledgerStore := ledger.NewMemoryStore()
	seedEntry(t, ledgerStore, "e-aapl", "PF1", "okx-sub-1", "AAPL", 100, -100)
	seedEntry(t, ledgerStore, "e-msft", "PF1", "okx-sub-1", "MSFT", 250, -250)
	if err := ledgerStore.Append(ctx, &ledger.Event{
		EntryID: "e-sub", PortfolioID: "PF1", Type: ledger.EntryTrade,
		Cash: big.NewRat(1000, 1), CashCurrency: "USD", Effective: t0, Knowledge: t0,
	}, nil); err != nil {
		t.Fatalf("Append: %v", err)
	}

	store := NewMemoryStore()
	// What CUST-A can state: the holdings in the exchange account it custodies,
	// and the cash that settled there. It cannot report the fund's bank balance.
	if err := store.SaveStatement(ctx, statement("S-A",
		map[string]int64{"AAPL": 100, "MSFT": 250}, map[string]int64{"USD": -350})); err != nil {
		t.Fatalf("SaveStatement: %v", err)
	}

	scope, err := NewBookScope([]Subject{{PortfolioID: "PF1", CustodianID: "CUST-A"}}, nil)
	if err != nil {
		t.Fatalf("NewBookScope: %v", err)
	}
	r, err := NewReconciler(store, LedgerBookLoader(ledgerStore, scope, testLogger()),
		&capturePublisher{}, new(big.Rat), nil, nil, func() time.Time { return t0 })
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	run, err := r.Reconcile(ctx, Subject{PortfolioID: "PF1", CustodianID: "CUST-A", BusinessDate: t0})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, b := range run.Breaks {
		t.Errorf("a %s break was reported on key %q (ibor=%v custodian=%v diff=%v).\n\n"+
			"The only disagreement between the book and the statement is an entry that settled "+
			"against NO exchange account, and no exchange custodian can report it. A break here is "+
			"manufactured on the DEFAULT configuration, and it ages, pages via CustodyBreakAgeing "+
			"and trains an operator to ignore the queue that exists to surface the one break meaning "+
			"a fill never reached the ledger.",
			b.Kind, b.Key, b.IBOR, b.Custodian, b.Diff)
	}
	if run.Outcome != OutcomeClean {
		t.Errorf("outcome = %v, want CLEAN", run.Outcome)
	}
}

// THE RESIDUE IS SAID OUT LOUD, WITH ITS AMOUNT (#1073).
//
// Excluding the entry is only half the repair. Cash held away from every custodian
// is real money in the book of record that no statement can confirm, so a run that
// silently left it out would answer "clean" about a comparison that did not cover
// everything in the book — and nothing anywhere would say which part it skipped.
// Book.CashBalance still carries the portfolio total for NAV and every other
// reader; what this asserts is that the reconciliation says what it did not
// compare, and how much.
func TestTheUnattributedEntryIsStatedRatherThanDroppedInSilence(t *testing.T) {
	ctx := context.Background()
	st := ledger.NewMemoryStore()
	seedEntry(t, st, "e-aapl", "PF1", "okx-sub-1", "AAPL", 100, -100)
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
	stated := &captureLogger{}
	if _, _, err := LedgerBookLoader(st, scope, stated.logger())(ctx,
		Subject{PortfolioID: "PF1", CustodianID: "CUST-A"}); err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, want := range []string{"NOTHING RECONCILES THEM", "USD 1000.00000000", BasisDerived, "PF1"} {
		if !stated.sawAt(slog.LevelWarn, want) {
			t.Errorf("the excluded entry was dropped without saying %q — a clean run then reports a "+
				"verdict about a comparison that skipped part of the book, and nothing says which part",
				want)
		}
	}

	// A book with nothing to exclude must be QUIET. A warning that fires on every
	// run is one nobody reads, which is the same failure the break queue is being
	// protected from.
	clean := ledger.NewMemoryStore()
	seedEntry(t, clean, "e-aapl", "PF1", "okx-sub-1", "AAPL", 100, -100)
	quiet := &captureLogger{}
	if _, _, err := LedgerBookLoader(clean, scope, quiet.logger())(ctx,
		Subject{PortfolioID: "PF1", CustodianID: "CUST-A"}); err != nil {
		t.Fatalf("load: %v", err)
	}
	if quiet.sawAt(slog.LevelWarn, "NOTHING RECONCILES THEM") {
		t.Error("a book whose every entry names an exchange account was warned about anyway — the " +
			"warning fires always, so it says nothing")
	}
}

// THE MIRROR-IMAGE DEFECT MUST BE REFUSED, NOT SERVED (#1073).
//
// venue_account_id is optional on the cash surface and is never defaulted, so a
// deployment can hold a whole journal of entries that name no exchange account. Its
// derived basis then folds to NOTHING, and every position the custodian reports
// comes back as MISSING_IN_IBOR — the whole book as breaks, in the opposite
// direction from the cash break this issue was filed on, and just as fabricated.
//
// NewBookScope already refuses an operator who DECLARES an empty account set, in
// these words and for this reason. A basis derived to the same emptiness must not
// be quieter than one declared, so the run FAILS and names what to stamp.
func TestAnEmptyDerivedBasisRefusesRatherThanBreakingTheWholeBook(t *testing.T) {
	ctx := context.Background()
	ledgerStore := ledger.NewMemoryStore()
	// Every entry settled against no exchange account — the shape a deployment
	// that never stamps venue_account_id is in.
	for _, e := range []*ledger.Event{
		{EntryID: "c1", PortfolioID: "PF1", Type: ledger.EntryCash,
			Cash: big.NewRat(100000, 1), CashCurrency: "USD", Effective: t0, Knowledge: t0},
		{EntryID: "t1", PortfolioID: "PF1", Type: ledger.EntryTrade, InstrumentID: "AAPL",
			Quantity: big.NewRat(100, 1), Price: big.NewRat(150, 1), Cash: big.NewRat(-15000, 1),
			CashCurrency: "USD", Effective: t0, Knowledge: t0},
	} {
		if err := ledgerStore.Append(ctx, e, nil); err != nil {
			t.Fatalf("Append %s: %v", e.EntryID, err)
		}
	}

	store := NewMemoryStore()
	if err := store.SaveStatement(ctx, statement("S-A",
		map[string]int64{"AAPL": 100}, map[string]int64{"USD": 85000})); err != nil {
		t.Fatalf("SaveStatement: %v", err)
	}
	scope, err := NewBookScope([]Subject{{PortfolioID: "PF1", CustodianID: "CUST-A"}}, nil)
	if err != nil {
		t.Fatalf("NewBookScope: %v", err)
	}
	r, err := NewReconciler(store, LedgerBookLoader(ledgerStore, scope, testLogger()),
		&capturePublisher{}, new(big.Rat), nil, nil, func() time.Time { return t0 })
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	run, err := r.Reconcile(ctx, Subject{PortfolioID: "PF1", CustodianID: "CUST-A", BusinessDate: t0})
	if err == nil {
		t.Fatalf("the run succeeded over an EMPTY comparison basis and reported %d break(s): %+v.\n\n"+
			"Nothing in this journal names an exchange account, so the basis folds to nothing and every "+
			"holding the custodian reports breaks as MISSING_IN_IBOR. That is the whole book as breaks, "+
			"which is what the repair was supposed to stop, arriving from the other side",
			len(run.Breaks), run.Breaks)
	}
	if run.Outcome != OutcomeFailed {
		t.Errorf("outcome = %v, want FAILED — a run that could not be trusted must be recorded as one, "+
			"or it is indistinguishable from a run that never came due", run.Outcome)
	}
	// The refusal has to be actionable: an operator needs the portfolio, the
	// amount at stake and the field to stamp.
	for _, want := range []string{"PF1", "venue_account_id", "MISSING_IN_IBOR"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so an operator cannot act on it: %v", want, err)
		}
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
	// THE MULTI-ACCOUNT CUSTODIAN IS THE CASE #1006 EXISTS FOR, and until #1029 it
	// could not be expressed at all: config split this value with env.SplitList,
	// which splits on the same comma that separates one custodian's accounts, so
	// the entry was cut in half and the pod exited 2 on the fragment "okx-sub-2".
	// Entries are separated by WHITESPACE, and the value arrives here whole.
	got, err := ParseCustodyAccounts("PF1:CUST-A:okx-sub-1,okx-sub-2 PF1:CUST-B:bin-main")
	if err != nil {
		t.Fatalf("ParseCustodyAccounts: %v", err)
	}
	if len(got["PF1"]["CUST-A"]) != 2 || got["PF1"]["CUST-A"][1] != "okx-sub-2" {
		t.Errorf("CUST-A accounts = %v, want both accounts of a two-account custodian", got["PF1"]["CUST-A"])
	}
	if len(got["PF1"]["CUST-B"]) != 1 || got["PF1"]["CUST-B"][0] != "bin-main" {
		t.Errorf("CUST-B accounts = %v", got["PF1"]["CUST-B"])
	}

	// A manifest writes this across lines in a YAML block scalar, so the entry
	// separator has to survive newlines and indentation rather than turning them
	// into empty entries.
	multiline, err := ParseCustodyAccounts("\n  PF1:CUST-A:okx-sub-1,okx-sub-2\n  PF1:CUST-B:bin-main\n")
	if err != nil {
		t.Fatalf("a multi-line declaration was refused: %v", err)
	}
	if len(multiline["PF1"]) != 2 {
		t.Errorf("multi-line declaration parsed %d custodians, want 2: %v", len(multiline["PF1"]), multiline)
	}

	// A MALFORMED ENTRY IS AN ERROR AND NOT A SKIP: dropping one would un-scope a
	// custodian while the service reported a healthy start.
	for _, bad := range []string{
		"PF1:CUST-A",             // no accounts
		"PF1:CUST-A:okx:extra",   // too many segments
		":CUST-A:okx-sub-1",      // no portfolio
		"PF1::okx-sub-1",         // no custodian
		"PF1:CUST-A:okx-sub-1,,", // an empty account id
		"okx-sub-2",              // the fragment #1029's splitter produced
		"PF1:CUST-A:okx-sub-1,PF1:CUST-B:bin-main", // comma between ENTRIES, not accounts
	} {
		if _, err := ParseCustodyAccounts(bad); err == nil {
			t.Errorf("ParseCustodyAccounts(%q) was accepted", bad)
		}
	}

	// The two separator mistakes #1029 makes likely are invisible in the fragment
	// the operator is shown — they never typed "okx-sub-2", the splitter did — so
	// the refusal has to name the SEPARATOR rather than only the fragment.
	for _, tc := range []struct{ decl, wantIn string }{
		{"okx-sub-2", "no ':' at all"},
		{"PF1:CUST-A:okx-sub-1,PF1:CUST-B:bin-main", "two entries ran together"},
	} {
		_, err := ParseCustodyAccounts(tc.decl)
		if err == nil || !strings.Contains(err.Error(), tc.wantIn) {
			t.Errorf("ParseCustodyAccounts(%q) does not diagnose the separator (want %q): %v",
				tc.decl, tc.wantIn, err)
		}
	}

	if _, err := ParseCustodyAccounts("PF1:CUST-A:a PF1:CUST-A:b"); err == nil {
		t.Error("a duplicated (portfolio, custodian) was accepted — one account set would win silently")
	}
}
