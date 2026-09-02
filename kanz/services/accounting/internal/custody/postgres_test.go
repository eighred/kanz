package custody

// Postgres custody-reconciliation integration tests (#962). Gated on
// TEST_POSTGRES_URL and skipped otherwise.
//
// WHY THESE EXIST SEPARATELY FROM THE MemoryStore TESTS. The in-memory store
// ignores ctx entirely and does its resolve-on-absence sweep with a Go map under
// a mutex; Postgres honours ctx and does it in one SQL transaction. A green
// in-memory suite therefore proves nothing about the two things most likely to be
// wrong here: that a redetection's ON CONFLICT clause updates exactly the columns
// it should and no others, and that the sweep is scoped to the run's own pair.
// Both are expressed in SQL that no in-memory test executes.

import (
	"context"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eighred/kanz/services/accounting/internal/recon"
)

const custodyMigrationDir = "../../migrations"

func newCustodyPool(t *testing.T, tenant string) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL to run custody Postgres integration tests")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", tenant)
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// applyCustodySchema drops and reapplies the accounting migrations.
func applyCustodySchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`DROP TABLE IF EXISTS custody_statements, custody_runs, custody_breaks, ledger_entries, ledger_snapshots, outbox CASCADE`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	files, err := filepath.Glob(filepath.Join(custodyMigrationDir, "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob migrations %s: %v (found %d)", custodyMigrationDir, err, len(files))
	}
	sort.Strings(files)
	for _, f := range files {
		ddl, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read migration %s: %v", f, err)
		}
		if _, err := pool.Exec(ctx, string(ddl)); err != nil {
			t.Fatalf("apply migration %s: %v", filepath.Base(f), err)
		}
	}
}

func newCustodyStore(t *testing.T) (*Postgres, context.Context) {
	t.Helper()
	pool := newCustodyPool(t, "__system__")
	applyCustodySchema(t, pool)
	return NewPostgres(pool), context.Background()
}

func detectedQuantityBreak(now time.Time, ibor, custodian int64) []Break {
	return FromRecon(subject(), []recon.Break{{
		Kind: recon.BreakQuantity, Key: "AAPL",
		IBOR:      big.NewRat(ibor, 1),
		Custodian: big.NewRat(custodian, 1),
		Diff:      big.NewRat(ibor-custodian, 1),
	}}, now)
}

func TestPostgresStatementRoundTripsExactly(t *testing.T) {
	st, ctx := newCustodyStore(t)

	// A quantity whose scaled coefficient exceeds float64's exact integer range,
	// so a store that round-tripped it through a JSON number would alter it.
	exact := new(big.Rat)
	exact.SetString("123456789.01234567")
	in := Statement{
		StatementID: "S1", CustodianID: "CUST-A", PortfolioID: "PF1",
		BusinessDate: BusinessDay(t0), ReceivedAt: t0,
		Positions: map[string]*big.Rat{"AAPL": exact},
		Cash:      map[string]*big.Rat{"USD": big.NewRat(-2500, 1)},
	}
	if err := st.SaveStatement(ctx, in); err != nil {
		t.Fatalf("SaveStatement: %v", err)
	}
	got, err := st.LatestStatement(ctx, subject())
	if err != nil {
		t.Fatalf("LatestStatement: %v", err)
	}
	if got.Positions["AAPL"].Cmp(exact) != 0 {
		t.Fatalf("quantity = %s, want %s — precision lost through storage",
			got.Positions["AAPL"].RatString(), exact.RatString())
	}
	// A negative cash balance is legitimate (an overdrawn account) and must not
	// be normalized away.
	if got.Cash["USD"].Cmp(big.NewRat(-2500, 1)) != 0 {
		t.Fatalf("cash = %s, want -2500", got.Cash["USD"].RatString())
	}
}

// A LATER STATEMENT FOR ONE SUBJECT WINS, and an unchanged redelivery does not
// accumulate rows.
func TestPostgresLatestStatementPrefersTheNewestReceipt(t *testing.T) {
	st, ctx := newCustodyStore(t)
	older := statement("S1", map[string]int64{"AAPL": 90}, nil)
	newer := statement("S2", map[string]int64{"AAPL": 100}, nil)
	newer.ReceivedAt = t0.Add(time.Hour)

	for _, s := range []Statement{older, newer, older} { // older redelivered last
		if err := st.SaveStatement(ctx, s); err != nil {
			t.Fatalf("SaveStatement %s: %v", s.StatementID, err)
		}
	}
	got, err := st.LatestStatement(ctx, subject())
	if err != nil {
		t.Fatalf("LatestStatement: %v", err)
	}
	if got.StatementID != "S2" {
		t.Fatalf("latest = %s, want S2 — a redelivered older statement won on arrival order", got.StatementID)
	}
}

func TestPostgresNoStatementIsDistinctFromAnEmptyOne(t *testing.T) {
	st, ctx := newCustodyStore(t)
	if _, err := st.LatestStatement(ctx, subject()); err == nil {
		t.Fatal("LatestStatement returned a statement for a subject that has none")
	} else if err.Error() != ErrNoStatement.Error() {
		t.Fatalf("err = %v, want ErrNoStatement — an empty statement would manufacture a break per held position", err)
	}
}

// THE ON CONFLICT CLAUSE MUST UPDATE THE FIGURES AND NOTHING THE OPERATOR OWNS.
// This is the SQL no in-memory test executes.
func TestPostgresRedetectionPreservesTheOperatorsWork(t *testing.T) {
	st, ctx := newCustodyStore(t)

	if _, err := st.UpsertBreaks(ctx, subject(), detectedQuantityBreak(t0, 100, 90), t0); err != nil {
		t.Fatalf("UpsertBreaks: %v", err)
	}
	id := BreakID("PF1", "CUST-A", recon.BreakQuantity, "AAPL")
	b, err := st.LoadBreak(ctx, id)
	if err != nil {
		t.Fatalf("LoadBreak: %v", err)
	}
	if err := b.Assign("alice", t0.Add(time.Hour)); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if err := b.Explain("late settlement, clears T+2", t0.Add(2*time.Hour)); err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if err := st.SaveBreak(ctx, b); err != nil {
		t.Fatalf("SaveBreak: %v", err)
	}

	// The next day's run finds the same break, with a MOVED figure.
	day2 := t0.Add(24 * time.Hour)
	if _, err := st.UpsertBreaks(ctx, subject(), detectedQuantityBreak(day2, 100, 80), day2); err != nil {
		t.Fatalf("UpsertBreaks day 2: %v", err)
	}
	got, err := st.LoadBreak(ctx, id)
	if err != nil {
		t.Fatalf("LoadBreak day 2: %v", err)
	}
	if got.Status != BreakExplained {
		t.Fatalf("status = %s, want explained — the redetection reset the operator's work", got.Status)
	}
	if got.Assignee != "alice" || got.Explanation == "" {
		t.Fatalf("assignee=%q explanation=%q — the operator's fields were overwritten", got.Assignee, got.Explanation)
	}
	if !got.FirstSeenAt.Equal(t0) {
		t.Fatalf("first_seen_at = %s, want %s — the age was reset", got.FirstSeenAt, t0)
	}
	if !got.LastSeenAt.Equal(day2) {
		t.Fatalf("last_seen_at = %s, want %s", got.LastSeenAt, day2)
	}
	// The FIGURES, by contrast, must advance.
	if got.Diff.Cmp(big.NewRat(20, 1)) != 0 {
		t.Fatalf("difference = %s, want 20 — the redetected figure did not advance", got.Diff.RatString())
	}
}

// THE SWEEP IS SCOPED TO THE RUN'S PAIR. An unscoped UPDATE would resolve every
// other custodian's breaks on every run — an estate-wide loss of the control
// produced by one pair reconciling cleanly.
func TestPostgresResolveOnAbsenceIsScopedToThePair(t *testing.T) {
	st, ctx := newCustodyStore(t)
	other := Subject{PortfolioID: "PF1", CustodianID: "CUST-B", BusinessDate: t0}
	otherDetected := FromRecon(other, []recon.Break{{
		Kind: recon.BreakCash, Key: "USD", IBOR: big.NewRat(5, 1), Custodian: new(big.Rat), Diff: big.NewRat(5, 1),
	}}, t0)
	if _, err := st.UpsertBreaks(ctx, other, otherDetected, t0); err != nil {
		t.Fatalf("UpsertBreaks other: %v", err)
	}
	if _, err := st.UpsertBreaks(ctx, subject(), detectedQuantityBreak(t0, 100, 90), t0); err != nil {
		t.Fatalf("UpsertBreaks subject: %v", err)
	}

	// CUST-A now reconciles clean. CUST-B's break must survive.
	outstanding, err := st.UpsertBreaks(ctx, subject(), nil, t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("UpsertBreaks clean: %v", err)
	}
	if len(outstanding) != 1 {
		t.Fatalf("%d outstanding, want 1 — the sweep crossed into another pair", len(outstanding))
	}
	if got := custodianOf(outstanding[0].BreakID); got != "CUST-B" {
		t.Fatalf("surviving break belongs to %q, want CUST-B", got)
	}
}

func TestPostgresAReturningBreakDoesNotInheritTheStaleExplanation(t *testing.T) {
	st, ctx := newCustodyStore(t)
	id := BreakID("PF1", "CUST-A", recon.BreakQuantity, "AAPL")

	if _, err := st.UpsertBreaks(ctx, subject(), detectedQuantityBreak(t0, 100, 90), t0); err != nil {
		t.Fatalf("UpsertBreaks: %v", err)
	}
	b, err := st.LoadBreak(ctx, id)
	if err != nil {
		t.Fatalf("LoadBreak: %v", err)
	}
	if err := b.Explain("clears T+2", t0); err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if err := st.SaveBreak(ctx, b); err != nil {
		t.Fatalf("SaveBreak: %v", err)
	}
	// It clears...
	if _, err := st.UpsertBreaks(ctx, subject(), nil, t0.Add(24*time.Hour)); err != nil {
		t.Fatalf("UpsertBreaks clear: %v", err)
	}
	// ...and returns.
	day3 := t0.Add(48 * time.Hour)
	if _, err := st.UpsertBreaks(ctx, subject(), detectedQuantityBreak(day3, 100, 90), day3); err != nil {
		t.Fatalf("UpsertBreaks return: %v", err)
	}
	got, err := st.LoadBreak(ctx, id)
	if err != nil {
		t.Fatalf("LoadBreak day 3: %v", err)
	}
	if got.Status != BreakOpen {
		t.Fatalf("status = %s, want open — a returning break resumed a stale lifecycle", got.Status)
	}
	if got.Explanation != "" {
		t.Fatalf("explanation = %q, want empty — an explanation that demonstrably did not hold was inherited", got.Explanation)
	}
	if !got.FirstSeenAt.Equal(day3) {
		t.Fatalf("first_seen_at = %s, want %s — a returning break is new work", got.FirstSeenAt, day3)
	}
}

func TestPostgresSaveBreakRefusesToInventOne(t *testing.T) {
	st, ctx := newCustodyStore(t)
	err := st.SaveBreak(ctx, Break{
		BreakID: "PF1|CUST-A|quantity|NEVER-DETECTED", Status: BreakAssigned, StatusChangedAt: t0,
	})
	if err == nil {
		t.Fatal("SaveBreak created a break nothing ever detected — a typo in an id would manufacture a working item")
	}
}

// RUNS ARE APPEND-ONLY EVIDENCE, and the newest one for a pair is what the
// staleness gauge ages — across all business dates.
func TestPostgresLatestRunIgnoresTheBusinessDate(t *testing.T) {
	st, ctx := newCustodyStore(t)
	old := Run{
		RunID: "r1", Subject: Subject{PortfolioID: "PF1", CustodianID: "CUST-A", BusinessDate: t0},
		Outcome: OutcomeClean, Tolerance: new(big.Rat), CompletedAt: t0,
	}
	// A run performed LATER that reconciled an EARLIER business date: recent
	// evidence that the control is alive, whatever date it covered.
	backfill := Run{
		RunID: "r2", Subject: Subject{PortfolioID: "PF1", CustodianID: "CUST-A", BusinessDate: t0.AddDate(0, 0, -7)},
		Outcome: OutcomeBreaks, Tolerance: new(big.Rat), CompletedAt: t0.Add(time.Hour),
	}
	for _, r := range []Run{old, backfill} {
		if err := st.SaveRun(ctx, r); err != nil {
			t.Fatalf("SaveRun %s: %v", r.RunID, err)
		}
	}
	got, err := st.LatestRun(ctx, "PF1", "CUST-A")
	if err != nil {
		t.Fatalf("LatestRun: %v", err)
	}
	if got.RunID != "r2" {
		t.Fatalf("latest run = %s, want r2", got.RunID)
	}
	if got.Outcome != OutcomeBreaks {
		t.Fatalf("outcome = %s, want breaks — the outcome did not survive storage", got.Outcome)
	}
}

func TestPostgresSaveRunIsIdempotent(t *testing.T) {
	st, ctx := newCustodyStore(t)
	r := Run{
		RunID: "r1", Subject: subject(), Outcome: OutcomeClean,
		Tolerance: new(big.Rat), CompletedAt: t0,
	}
	for i := 0; i < 3; i++ {
		if err := st.SaveRun(ctx, r); err != nil {
			t.Fatalf("SaveRun %d: %v", i, err)
		}
	}
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM custody_runs WHERE run_id = $1`, "r1").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("%d rows for one run_id, want 1", n)
	}
}

// A NO_STATEMENT RUN IS STILL A ROW. This is the record that separates "the
// custodian stopped sending" from "nothing was scheduled".
func TestPostgresNoStatementRunIsPersisted(t *testing.T) {
	st, ctx := newCustodyStore(t)
	r := Run{
		RunID: "r1", Subject: subject(), Outcome: OutcomeNoStatement,
		Tolerance: new(big.Rat), CompletedAt: t0,
	}
	if err := st.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	got, err := st.LatestRun(ctx, "PF1", "CUST-A")
	if err != nil {
		t.Fatalf("LatestRun: %v", err)
	}
	if got.Outcome != OutcomeNoStatement {
		t.Fatalf("outcome = %s, want no_statement", got.Outcome)
	}
	if got.StatementID != "" {
		t.Fatalf("statement_id = %q, want empty", got.StatementID)
	}
}

// TENANT ISOLATION IS DENY-BY-DEFAULT (MT-01d/e). These tables carry one fund's
// custodian holdings and its unresolved control failures; another tenant must not
// be able to DISCOVER them, let alone read them.
func TestPostgresTenantIsolation(t *testing.T) {
	acme := newCustodyPool(t, "acme")
	applyCustodySchema(t, acme)
	globex := newCustodyPool(t, "globex")
	ctx := context.Background()

	acmeStore, globexStore := NewPostgres(acme), NewPostgres(globex)
	if err := acmeStore.SaveStatement(ctx, statement("S-ACME", map[string]int64{"AAPL": 100}, nil)); err != nil {
		t.Fatalf("acme SaveStatement: %v", err)
	}
	if _, err := acmeStore.UpsertBreaks(ctx, subject(), detectedQuantityBreak(t0, 100, 90), t0); err != nil {
		t.Fatalf("acme UpsertBreaks: %v", err)
	}

	if _, err := globexStore.LatestStatement(ctx, subject()); err == nil {
		t.Fatal("globex read acme's custodian statement")
	}
	breaks, err := globexStore.OutstandingBreaks(ctx)
	if err != nil {
		t.Fatalf("globex OutstandingBreaks: %v", err)
	}
	if len(breaks) != 0 {
		t.Fatalf("globex sees %d of acme's breaks — one fund can discover another's unresolved control failures", len(breaks))
	}
	subs, err := globexStore.Subjects(ctx)
	if err != nil {
		t.Fatalf("globex Subjects: %v", err)
	}
	if len(subs) != 0 {
		t.Fatalf("globex enumerated %d of acme's (portfolio, custodian) pairs", len(subs))
	}
	// acme still sees its own.
	if own, err := acmeStore.OutstandingBreaks(ctx); err != nil || len(own) != 1 {
		t.Fatalf("acme sees %d of its own breaks (err=%v), want 1 — isolation broke the owner's read too", len(own), err)
	}
}

// AN UNSCOPED SESSION MUST ERROR, NEVER RETURN EMPTY (MT-01e). app_current_tenant()
// raises, so a pool that forgot the GUC fails loudly instead of reading zero rows
// — which is indistinguishable from a tenant that genuinely has no breaks.
func TestPostgresUnscopedSessionRaises(t *testing.T) {
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL to run custody Postgres integration tests")
	}
	scoped := newCustodyPool(t, "__system__")
	applyCustodySchema(t, scoped)

	// A pool with NO AfterConnect: app.tenant_id is never set.
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := NewPostgres(pool).OutstandingBreaks(context.Background()); err == nil {
		t.Fatal("an unscoped session returned rows rather than raising — an unscoped read and an empty tenant are the same observable event")
	}
}

// A CANCELLED CONTEXT MUST REACH THE DATABASE. MemoryStore ignores ctx entirely,
// so this behaviour is unobservable through the in-memory seam.
func TestPostgresHonoursContextCancellation(t *testing.T) {
	st, _ := newCustodyStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st.OutstandingBreaks(ctx); err == nil {
		t.Fatal("a cancelled context was ignored by the store")
	}
}
