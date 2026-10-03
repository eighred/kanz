package capital

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const schema = "oms_capital_test"

func tenantPool(t *testing.T, tenant string) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_URL required for real capital transactions")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, `SELECT set_config('app.tenant_id',$1,false)`, tenant)
		return err
	}
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	var bypass bool
	if err = p.QueryRow(context.Background(), `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&bypass); err != nil {
		t.Fatal(err)
	}
	if bypass {
		t.Fatal("capital tests require NOSUPERUSER NOBYPASSRLS")
	}
	return p
}

func database(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return databaseBefore(t, "")
}

func databaseBefore(t *testing.T, before string) *pgxpool.Pool {
	t.Helper()
	p := tenantPool(t, "tenant-a")
	ctx := context.Background()
	if _, err := p.Exec(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatal("migration inventory", err)
	}
	sort.Strings(files)
	for _, f := range files {
		if filepath.Base(f) == before {
			break
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = p.Exec(ctx, string(data)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	return p
}

func transaction(p *pgxpool.Pool, run func(pgx.Tx) error) error {
	ctx := context.Background()
	tx, err := p.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = run(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func snapshot(rev int64, total string) CashEvent {
	return CashEvent{Complete: true, PortfolioID: "fund", Currency: "USD", Revision: rev, Total: dec.Exact(total), ObservedAt: time.Date(2026, 10, 2, 10, 0, 0, 123, time.UTC)}
}

func TestIncompleteCoverageImmediatelyStopsNewSpending(t *testing.T) {
	p := database(t)
	ctx := context.Background()
	opening := snapshot(1, "250")
	if err := Apply(ctx, p, opening); err != nil {
		t.Fatal(err)
	}
	reserve := func(id string, amount dec.Exact) error {
		return transaction(p, func(tx pgx.Tx) error {
			return Reserve(ctx, tx, "fund", "USD", id, amount, opening.ObservedAt, time.Minute)
		})
	}
	if err := reserve("existing", "100"); err != nil {
		t.Fatal(err)
	}
	incomplete := snapshot(2, "250")
	incomplete.Complete = false
	if err := Apply(ctx, p, incomplete); err != nil {
		t.Fatal(err)
	}
	if err := reserve("new", "1"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("old complete cash remained usable: %v", err)
	}
	if err := transaction(p, func(tx pgx.Tx) error {
		return Change(ctx, tx, "fund", "USD", "existing", 1, "101", opening.ObservedAt, time.Minute)
	}); !errors.Is(err, ErrUnknown) {
		t.Fatalf("increased commitment on incomplete source: %v", err)
	}
	// A confirmed exposure reduction remains possible during a source outage.
	if err := transaction(p, func(tx pgx.Tx) error {
		return Change(ctx, tx, "fund", "USD", "existing", 1, "50", opening.ObservedAt, time.Minute)
	}); err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, p, snapshot(3, "250")); err != nil {
		t.Fatal(err)
	}
	if err := reserve("new", "200"); err != nil {
		t.Fatalf("complete source failed to restore exact remaining capacity: %v", err)
	}
}

func TestCashCoverageIdentityIncludesTheWholeSourceFact(t *testing.T) {
	p := database(t)
	ctx := context.Background()
	msg := coverageBalance()
	msg.CashCommit.Applied = nil
	event, err := FromBalance(msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, p, event); err != nil {
		t.Fatal(err)
	}
	// Same revision and financial totals, contradictory source-feed posture.
	msg.Completeness.UnproducedEntryTypes = []string{"fee"}
	changed, err := FromBalance(msg)
	if err != nil {
		t.Fatal(err)
	}
	if changed.SourceDigest == event.SourceDigest {
		t.Fatal("source posture omitted from receipt identity")
	}
	if err := Apply(ctx, p, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting source revision accepted: %v", err)
	}
	if err := Apply(ctx, p, event); err != nil {
		t.Fatal(err)
	}
	if err := transaction(p, func(tx pgx.Tx) error {
		return Reserve(ctx, tx, "fund", "USD", "order", "1", event.ObservedAt, time.Minute)
	}); !errors.Is(err, ErrUnknown) {
		t.Fatalf("replay cleared conflict quarantine: %v", err)
	}
}

func reserve(p *pgxpool.Pool, id, amount string) error {
	return transaction(p, func(tx pgx.Tx) error {
		return Reserve(context.Background(), tx, "fund", "USD", id, dec.Exact(amount), snapshot(1, "0").ObservedAt, time.Minute)
	})
}

func assertReserved(t *testing.T, p *pgxpool.Pool, want string) {
	t.Helper()
	var got string
	if err := p.QueryRow(context.Background(), `SELECT reserved FROM capital_balances WHERE portfolio_id='fund' AND currency='USD'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("reserved=%s want %s", got, want)
	}
}

func TestPostgresCapitalCompetingAdmissionsAndRollback(t *testing.T) {
	p := database(t)
	ctx := context.Background()
	if err := Apply(ctx, p, snapshot(1, "250")); err != nil {
		t.Fatal(err)
	}
	const racers = 32
	results := make(chan error, racers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; results <- reserve(p, fmt.Sprintf("order-%d", i), "200") }()
	}
	close(start)
	wg.Wait()
	close(results)
	admitted := 0
	for err := range results {
		if err == nil {
			admitted++
		} else if !errors.Is(err, ErrInsufficient) {
			t.Fatal(err)
		}
	}
	if admitted != 1 {
		t.Fatalf("%d competing 200-unit orders admitted against 250", admitted)
	}
	assertReserved(t, p, "200")
	rollback := errors.New("order transaction rejected")
	err := transaction(p, func(tx pgx.Tx) error {
		if err := Reserve(ctx, tx, "fund", "USD", "rolled-back", "40", snapshot(1, "0").ObservedAt, time.Minute); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO orders(order_id,state,portfolio_id,parent_order_id) VALUES('rolled-back',$1,'fund','')`, []byte{}); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	assertReserved(t, p, "200")
	var count int
	if err = p.QueryRow(ctx, `SELECT count(*) FROM orders WHERE order_id='rolled-back'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("order escaped rollback %d %v", count, err)
	}
	if err = reserve(p, "rolled-back", "50"); err != nil {
		t.Fatal("reservation escaped rollback", err)
	}
}

func TestPostgresCapitalDelayedAccountingAndPartialCancellation(t *testing.T) {
	p := database(t)
	ctx := context.Background()
	now := snapshot(1, "0").ObservedAt
	if err := Apply(ctx, p, snapshot(1, "250")); err != nil {
		t.Fatal(err)
	}
	if err := reserve(p, "order", "200"); err != nil {
		t.Fatal(err)
	}
	if err := transaction(p, func(tx pgx.Tx) error { return ObserveExecution(ctx, tx, "fund", "USD", "order", "100") }); err != nil {
		t.Fatal(err)
	}
	assertReserved(t, p, "200")
	if err := transaction(p, func(tx pgx.Tx) error { return Change(ctx, tx, "fund", "USD", "order", 2, "0", now, time.Minute) }); !errors.Is(err, ErrInvalid) {
		t.Fatal("cancel discarded executed liability", err)
	}
	if err := transaction(p, func(tx pgx.Tx) error { return Change(ctx, tx, "fund", "USD", "order", 2, "100", now, time.Minute) }); err != nil {
		t.Fatal(err)
	}
	assertReserved(t, p, "100")
	if err := reserve(p, "too-large", "151"); !errors.Is(err, ErrInsufficient) {
		t.Fatal(err)
	}
	e := snapshot(2, "150")
	e.Applied = []Applied{{OrderID: "order", Debit: "100"}}
	if err := Apply(ctx, p, e); err != nil {
		t.Fatal(err)
	}
	assertReserved(t, p, "0")
	if err := Apply(ctx, p, e); err != nil {
		t.Fatal("duplicate proof", err)
	}
	// A new connection pool sees the same commitments after process restart.
	restarted := tenantPool(t, "tenant-a")
	if err := reserve(restarted, "next", "150"); err != nil {
		t.Fatal(err)
	}
	assertReserved(t, p, "150")
	if err := transaction(p, func(tx pgx.Tx) error { return Change(ctx, tx, "fund", "USD", "order", 2, "100", now, time.Minute) }); !errors.Is(err, ErrConflict) {
		t.Fatal("stale revision admitted", err)
	}
}

func TestPostgresCapitalGapConflictAndTenantIsolation(t *testing.T) {
	p := database(t)
	ctx := context.Background()
	if err := Apply(ctx, p, snapshot(1, "100")); err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, p, snapshot(3, "50")); !errors.Is(err, ErrUnknown) {
		t.Fatal(err)
	}
	if err := reserve(p, "gap", "1"); !errors.Is(err, ErrUnknown) {
		t.Fatal("known gap admitted", err)
	}
	if err := Apply(ctx, p, snapshot(2, "75")); err != nil {
		t.Fatal(err)
	}
	if err := reserve(p, "gap", "1"); !errors.Is(err, ErrUnknown) {
		t.Fatal("gap cleared before full prefix", err)
	}
	if err := Apply(ctx, p, snapshot(3, "50")); err != nil {
		t.Fatal(err)
	}
	if err := reserve(p, "same-id", "50"); err != nil {
		t.Fatal(err)
	}
	other := tenantPool(t, "tenant-b")
	if err := reserve(other, "same-id", "1"); !errors.Is(err, ErrUnknown) {
		t.Fatal("cross-tenant cash", err)
	}
	if err := Apply(ctx, other, snapshot(1, "200")); err != nil {
		t.Fatal(err)
	}
	if err := reserve(other, "same-id", "200"); err != nil {
		t.Fatal(err)
	}
	assertReserved(t, p, "50")
	assertReserved(t, other, "200")
	if err := Apply(ctx, p, snapshot(3, "500")); !errors.Is(err, ErrConflict) {
		t.Fatal("conflicting duplicate admitted", err)
	}
	if err := reserve(p, "conflicted", "0"); !errors.Is(err, ErrUnknown) {
		t.Fatal("contradictory accounting evidence did not quarantine admission", err)
	}
}

func TestPostgresCapitalPendingIdentityCannotBeReplaced(t *testing.T) {
	p := database(t)
	ctx := context.Background()
	if err := Apply(ctx, p, snapshot(3, "50")); !errors.Is(err, ErrUnknown) {
		t.Fatal(err)
	}
	if err := Apply(ctx, p, snapshot(3, "500")); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	for _, e := range []CashEvent{snapshot(1, "100"), snapshot(2, "75"), snapshot(3, "50")} {
		if err := Apply(ctx, p, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := reserve(p, "must-investigate", "1"); !errors.Is(err, ErrUnknown) {
		t.Fatal("replay silently cleared evidence conflict", err)
	}
}

func TestPostgresCapitalFreshnessAndAccountingReversal(t *testing.T) {
	p := database(t)
	ctx := context.Background()
	now := snapshot(1, "0").ObservedAt
	if err := Apply(ctx, p, snapshot(1, "9007199254740993")); err != nil {
		t.Fatal(err)
	}
	if err := reserve(p, "precise", "9007199254740992.000000001"); err != nil {
		t.Fatal(err)
	}
	assertReserved(t, p, "9007199254740992000000001/1000000000")
	if err := transaction(p, func(tx pgx.Tx) error {
		return Reserve(ctx, tx, "fund", "USD", "stale", "0", now.Add(2*time.Minute), time.Minute)
	}); !errors.Is(err, ErrUnknown) {
		t.Fatal(err)
	}
	e := snapshot(2, "9007199254740893")
	e.Applied = []Applied{{"precise", "100"}}
	if err := Apply(ctx, p, e); err != nil {
		t.Fatal(err)
	}
	e = snapshot(3, "9007199254740943")
	e.Applied = []Applied{{"precise", "50"}}
	if err := Apply(ctx, p, e); err != nil {
		t.Fatal(err)
	}
	assertReserved(t, p, "9007199254740942000000001/1000000000")
}

func TestPostgresCapitalProofFailureRollsBackEveryApplication(t *testing.T) {
	p := database(t)
	ctx := context.Background()
	if err := Apply(ctx, p, snapshot(1, "250")); err != nil {
		t.Fatal(err)
	}
	if err := reserve(p, "order", "200"); err != nil {
		t.Fatal(err)
	}
	e := snapshot(2, "100")
	e.Applied = []Applied{{"order", "100"}, {"unknown-order", "50"}}
	if err := Apply(ctx, p, e); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	assertReserved(t, p, "200")
	var revision int64
	if err := p.QueryRow(ctx, `SELECT revision FROM capital_balances WHERE portfolio_id='fund'`).Scan(&revision); err != nil || revision != 1 {
		t.Fatal("partial accounting event committed", revision, err)
	}
	if err := reserve(p, "unknown-coverage", "1"); !errors.Is(err, ErrUnknown) {
		t.Fatal("unknown accounting coverage did not block admission", err)
	}
	var booked string
	if err := p.QueryRow(ctx, `SELECT booked_debit FROM capital_commitments WHERE order_id='order'`).Scan(&booked); err != nil || booked != "0" {
		t.Fatal("partial proof escaped rollback", booked, err)
	}
}

func TestPostgresCapitalCommitFailureCannotLeaveReservation(t *testing.T) {
	p := database(t)
	ctx := context.Background()
	if err := Apply(ctx, p, snapshot(1, "250")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `CREATE FUNCTION reject_capital_order_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected deferred failure'; END $$;
		CREATE CONSTRAINT TRIGGER reject_capital_order_commit AFTER INSERT ON orders DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_capital_order_commit()`); err != nil {
		t.Fatal(err)
	}
	err := transaction(p, func(tx pgx.Tx) error {
		if err := Reserve(ctx, tx, "fund", "USD", "order", "200", snapshot(1, "0").ObservedAt, time.Minute); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO orders(order_id,state,portfolio_id,parent_order_id) VALUES('order',$1,'fund','')`, []byte{})
		return err
	})
	if err == nil {
		t.Fatal("deferred failure did not abort commit")
	}
	assertReserved(t, p, "0")
	if err := reserve(p, "order", "250"); err != nil {
		t.Fatal("failed order left a commitment", err)
	}
}

func TestPostgresCapitalActualOverrunIsRetainedAndBlocksFurtherSpend(t *testing.T) {
	p := database(t)
	ctx := context.Background()
	if err := Apply(ctx, p, snapshot(1, "250")); err != nil {
		t.Fatal(err)
	}
	if err := reserve(p, "order", "200"); err != nil {
		t.Fatal(err)
	}
	if err := reserve(p, "other", "50"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := transaction(p, func(tx pgx.Tx) error { return ObserveExecution(ctx, tx, "fund", "USD", "order", "260") }); err != nil {
			t.Fatal("real liability was discarded", err)
		}
	}
	assertReserved(t, p, "310")
	var version int64
	if err := p.QueryRow(ctx, `SELECT version FROM capital_commitments WHERE order_id='order'`).Scan(&version); err != nil || version != 2 {
		t.Fatal("duplicate observation changed revision", version, err)
	}
	if err := reserve(p, "new-order", "1"); !errors.Is(err, ErrInsufficient) {
		t.Fatal("overrun did not block more spend", err)
	}
}
