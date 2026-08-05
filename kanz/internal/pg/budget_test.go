package pg_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eighred/kanz/internal/pg"
)

// The two failure modes #228 exists for, driven against a real Postgres rather
// than argued about. Both were previously unreachable by any test, because
// neither a pool size nor a statement timeout existed to exercise.
//
// The MECHANISM is what a test can prove; the NUMBERS (4 connections, 120s, 5s)
// are judgements, derived in internal/pg and re-derived from the manifests by
// test/arch/pool_budget_test.go. So these tests drive pg.Service's own Configure
// with a short bound where waiting 120s would be absurd, and separately assert
// that the SHIPPED profile puts its real numbers on a real connection.

func dsn(t *testing.T) string {
	t.Helper()
	d := os.Getenv("TEST_POSTGRES_URL")
	if d == "" {
		t.Skip("set TEST_POSTGRES_URL to drive the connection budget against a real database")
	}
	return d
}

// poolWith builds a pool from a profile, through the same Configure the
// constructors use. Bare pgxpool here is deliberate and is why
// test/arch/pool_budget_test.go excludes _test.go: the point is to vary one term
// of the shipped profile and watch what changes.
func poolWith(t *testing.T, p pg.Profile) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn(t))
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	if err := p.Configure(cfg); err != nil {
		t.Fatalf("configure %q: %v", p.Name, err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// A SLOW QUERY IS KILLED, NOT WAITED ON.
//
// With statement_timeout=0 — the estate's state before #228 — pg_sleep(5) returns
// after five seconds and a genuinely blocked query never returns at all, holding
// its backend and its pool slot for the life of the connection. That is how a
// pool gets exhausted in the first place.
func TestAStatementPastTheBoundIsKilledRatherThanHeld(t *testing.T) {
	p := pg.Service
	p.StatementTimeout = 500 * time.Millisecond // 120s is the shipped bound; see below.
	pool := poolWith(t, p)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	_, err := pool.Exec(ctx, "SELECT pg_sleep(5)")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("pg_sleep(5) COMPLETED under a 500ms statement_timeout (took %v). The bound is not "+
			"reaching the server: an unbounded statement holds its backend and its pool slot until the "+
			"connection dies, which is the exhaustion path this profile exists to close", elapsed)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("the query failed, but not as a statement timeout (want SQLSTATE 57014): %v", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("the statement timeout fired after %v, far past its 500ms bound — the bound is not "+
			"bounding anything an operator could rely on", elapsed)
	}
	t.Logf("pg_sleep(5) under statement_timeout=500ms: killed after %v with %s (SQLSTATE %s)",
		elapsed.Round(time.Millisecond), pgErr.Message, pgErr.Code)

	// The pool is still usable: pgx did not discard the connection, and the next
	// caller is unaffected. A timeout that poisoned the pool would trade one stuck
	// query for a dead service.
	var one int
	if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("the pool did not survive its own statement timeout: %v", err)
	}
}

// THE SHIPPED PROFILES PUT THEIR REAL NUMBERS ON A REAL CONNECTION.
//
// The test above proves the machinery with a bound short enough to watch. This
// one proves the machinery is carrying the numbers that actually ship — read back
// out of pg_settings, with the SOURCE, so "the client set this" is distinguished
// from "this is what the server happened to default to". Before #228 every one of
// these was `0` with source `default`.
func TestTheShippedProfilesReachTheServer(t *testing.T) {
	d := dsn(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, tc := range []struct {
		what  string
		open  func() (*pgxpool.Pool, error)
		wants map[string]string
	}{
		{
			what: "pg.NewGlobalPool (the Service profile)",
			open: func() (*pgxpool.Pool, error) {
				return pg.NewGlobalPool(ctx, d, "a test fixture, which has no tenant-scoped store")
			},
			wants: map[string]string{
				"statement_timeout":                   "90s",
				"lock_timeout":                        "5s",
				"idle_in_transaction_session_timeout": "1min",
			},
		},
		{
			what: "pg.NewMigrationPool",
			open: func() (*pgxpool.Pool, error) { return pg.NewMigrationPool(ctx, d) },
			wants: map[string]string{
				// DELIBERATELY UNBOUNDED, and set to 0 explicitly so that
				// pg_settings.source says `client`. An inherited 0 and a chosen 0
				// are the same number and completely different facts.
				"statement_timeout":                   "0",
				"lock_timeout":                        "10s",
				"idle_in_transaction_session_timeout": "0",
			},
		},
	} {
		pool, err := tc.open()
		if err != nil {
			t.Fatalf("%s: %v", tc.what, err)
		}
		for guc, want := range tc.wants {
			var got, source string
			err := pool.QueryRow(ctx,
				"SELECT setting, source FROM pg_settings WHERE name = $1", guc).Scan(&got, &source)
			if err != nil {
				t.Fatalf("%s: read %s: %v", tc.what, guc, err)
			}
			// pg_settings.setting for a time GUC is in its unit (ms); SHOW renders
			// it. Compare the rendered form, which is what an operator reads.
			var shown string
			if err := pool.QueryRow(ctx, "SHOW "+guc).Scan(&shown); err != nil {
				t.Fatalf("%s: SHOW %s: %v", tc.what, guc, err)
			}
			if shown != want {
				t.Errorf("%s: %s is %q, want %q", tc.what, guc, shown, want)
			}
			if source != "client" {
				t.Errorf("%s: %s came from %q, not from the client. A GUC this profile did not actually "+
					"set is one the next server default silently changes underneath the estate",
					tc.what, guc, source)
			}
		}
		var maxConns int
		if err := pool.QueryRow(ctx, "SELECT 1").Scan(&maxConns); err != nil {
			t.Fatalf("%s: %v", tc.what, err)
		}
		t.Logf("%s: MaxConns=%d, %v", tc.what, pool.Config().MaxConns, tc.wants)
		pool.Close()
	}
}

// AN EXHAUSTED POOL IS BOUNDED, AND THE ESTATE CAN SEE WHOSE IT IS.
//
// The failure this reproduces is the one the issue describes: every slot held,
// the next caller blocked. Two properties are asserted, because only the pair is
// worth anything.
//
// BOUNDED. The wait is the caller's context — an acquire never blocks longer than
// the caller allowed — and, more importantly, the exhaustion SELF-HEALS: the
// holders are killed by statement_timeout, so a pool that filled up recovers on
// its own within the bound instead of staying wedged until the pods are cycled.
// With statement_timeout=0 there is no such moment.
//
// LEGIBLE. `context deadline exceeded` on the client says nothing about pools, so
// legibility has to come from the server side, where a human debugging `sorry,
// too many clients already` is actually looking. Configure stamps
// application_name on every connection, so pg_stat_activity attributes each
// backend to the binary and the profile that opened it. Before #228 all of them
// were the empty string.
func TestAnExhaustedPoolIsBoundedAndAttributable(t *testing.T) {
	const holdSeconds = 3

	p := pg.Service
	p.StatementTimeout = holdSeconds * time.Second
	pool := poolWith(t, p)
	if pool.Config().MaxConns != pg.ServiceMaxConns {
		t.Fatalf("this test is about exhausting the SHIPPED size; pool has MaxConns=%d, want %d",
			pool.Config().MaxConns, pg.ServiceMaxConns)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Fill every slot with a query that will outlive the acquire attempt below.
	var wg sync.WaitGroup
	for i := 0; i < int(pg.ServiceMaxConns); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = pool.Exec(ctx, "SELECT pg_sleep($1)", holdSeconds*2)
		}()
	}
	// Wait until the pool really is full rather than guessing with a sleep.
	deadline := time.Now().Add(20 * time.Second)
	for pool.Stat().AcquiredConns() < pg.ServiceMaxConns {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d connections were acquired; the pool never filled and this test "+
				"would prove nothing", pool.Stat().AcquiredConns(), pg.ServiceMaxConns)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// --- BOUNDED (1): the acquire never outlives its caller's context ---------
	short, cancelShort := context.WithTimeout(ctx, 500*time.Millisecond)
	start := time.Now()
	_, err := pool.Exec(short, "SELECT 1")
	waited := time.Since(start)
	cancelShort()
	if err == nil {
		t.Fatal("a fifth query got a connection from a pool whose every slot was held — MaxConns is " +
			"not being enforced, so the estate's connection budget is not a budget")
	}
	if waited > 3*time.Second {
		t.Errorf("the blocked caller waited %v on a 500ms context: an exhausted pool must fail inside "+
			"the caller's deadline, not outlive it", waited)
	}
	t.Logf("pool exhausted at MaxConns=%d: the next caller failed after %v with %q",
		pg.ServiceMaxConns, waited.Round(time.Millisecond), err)

	// --- LEGIBLE: pg_stat_activity names the pool that is holding them --------
	//
	// From a SEPARATE connection, because every slot of this pool is busy — which
	// is exactly the position an operator is in.
	admin, err := unscopedConn(ctx, dsn(t))
	if err != nil {
		t.Fatalf("open an observing connection: %v", err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	var app string
	var n int
	err = admin.QueryRow(ctx, `
		SELECT application_name, count(*) FROM pg_stat_activity
		WHERE query LIKE '%pg_sleep%' AND application_name <> ''
		GROUP BY application_name ORDER BY count(*) DESC LIMIT 1`).Scan(&app, &n)
	if err != nil {
		t.Fatalf("an exhausted pool must be attributable in pg_stat_activity, and no backend carried "+
			"an application_name: %v.\n\nWithout it, `sorry, too many clients already` names no "+
			"service and the operator has to guess which pool grew", err)
	}
	if !strings.Contains(app, p.Name) {
		t.Errorf("pg_stat_activity attributes the held backends to %q, which does not name the %q "+
			"profile that opened them", app, p.Name)
	}
	t.Logf("pg_stat_activity: %d backend(s) attributed to application_name=%q", n, app)

	// --- BOUNDED (2): it self-heals, which is the property that matters -------
	//
	// statement_timeout kills the holders, so the pool comes back on its own. This
	// is the difference the bound makes: a wedge that clears in seconds versus one
	// that clears when somebody notices.
	wg.Wait()
	recover, cancelRecover := context.WithTimeout(ctx, 10*time.Second)
	defer cancelRecover()
	start = time.Now()
	var one int
	if err := pool.QueryRow(recover, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("the pool never recovered after its holders were killed: %v.\n\nAn exhausted pool that "+
			"does not come back is the state statement_timeout exists to make impossible", err)
	}
	t.Logf("pool recovered %v after the holders were killed by statement_timeout=%s",
		time.Since(start).Round(time.Millisecond), p.StatementTimeout)
}

// WithStatementTimeout is the sanctioned way past the platform bound, and the
// bound must be back in force on the connection afterwards — otherwise the escape
// hatch leaks into whatever query gets that connection next.
func TestWithStatementTimeoutRaisesTheBoundForOneTransactionOnly(t *testing.T) {
	p := pg.Service
	p.StatementTimeout = 300 * time.Millisecond
	p.MaxConns = 1 // force reuse of the same connection, which is the leak this checks for
	pool := poolWith(t, p)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Outside the hatch the platform bound applies.
	if _, err := pool.Exec(ctx, "SELECT pg_sleep(1)"); err == nil {
		t.Fatal("a 1s query survived a 300ms statement_timeout")
	}

	// Inside it, the query is allowed to run long.
	err := pg.WithStatementTimeout(ctx, pool, 5*time.Second, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT pg_sleep(1)")
		return err
	})
	if err != nil {
		t.Fatalf("WithStatementTimeout(5s) did not let a 1s query finish: %v", err)
	}

	// And the connection it used is back under the platform bound.
	if _, err := pool.Exec(ctx, "SELECT pg_sleep(1)"); err == nil {
		t.Fatal("after WithStatementTimeout the connection still had the raised bound. SET LOCAL is " +
			"transaction-scoped precisely so this cannot happen; if it is now a plain SET, every later " +
			"query on this connection silently inherits an escape hatch it did not ask for")
	}

	if err := pg.WithStatementTimeout(ctx, pool, 0, nil); err == nil {
		t.Error("WithStatementTimeout accepted a non-positive bound. It raises the platform bound for " +
			"one query; it is not a way for a service to spell Unbounded")
	}
}

// A DSN THAT SIZES ITS OWN POOL IS REFUSED RATHER THAN SILENTLY OVERWRITTEN.
//
// Configure overwrites pool_max_conns, so such a DSN is not dangerous — it is
// worse: an operator sets it in a Vault secret, the value is discarded without a
// word, and the estate's real ceiling is somewhere neither of them is looking.
func TestADSNThatSizesItsOwnPoolIsRefused(t *testing.T) {
	for _, d := range []string{
		"postgres://u:p@h:5432/db?sslmode=disable&pool_max_conns=50",
		"host=h user=u dbname=db pool_min_conns=9",
	} {
		_, err := pg.NewGlobalPool(context.Background(), d, "a reason")
		if err == nil {
			t.Errorf("%q was accepted; its pool sizing would have been silently discarded", d)
			continue
		}
		if !strings.Contains(err.Error(), "pool_") {
			t.Errorf("%q was refused, but the error does not name the offending parameter: %v", d, err)
		}
	}
}

// NewGlobalPool requires a written reason. Before #228 a store with no RLS and a
// service that forgot to scope itself were the same line of code — pgxpool.New —
// and the second is how accounting shipped a ledger that silently read and wrote
// nothing.
func TestAGlobalPoolNeedsAWrittenReason(t *testing.T) {
	_, err := pg.NewGlobalPool(context.Background(), "postgres://x/y", "   ")
	if err == nil {
		t.Fatal("an unscoped pool was built with no stated reason")
	}
	if !strings.Contains(err.Error(), "reason") {
		t.Errorf("the refusal must say what is missing; got %v", err)
	}
}
