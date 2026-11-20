package capital

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var releaseCurrencies = []string{"BNB", "USD", "ZERO"}
var releaseDebits = []string{"5/4", "100", "0"}

func seedRelease(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := t.Context()
	for i, currency := range releaseCurrencies {
		event := snapshot(1, "1000")
		event.Currency = currency
		if err := Apply(ctx, pool, event); err != nil {
			t.Fatal(err)
		}
		err := transaction(pool, func(tx pgx.Tx) error {
			if err := Reserve(ctx, tx, "fund", currency, "other", "1/7", event.ObservedAt, time.Minute); err != nil {
				return err
			}
			return Reserve(ctx, tx, "fund", currency, "cancel", dec.Exact(releaseDebits[i]), event.ObservedAt, time.Minute)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

const releaseStatement = `SELECT capital_release_unexecuted('fund','cancel',$1,$2)`

type releaseReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func releaseLedger(t *testing.T, r releaseReader) string {
	t.Helper()
	var s string
	// Read all cash, commitments and members independently of the write kernel.
	err := r.QueryRow(t.Context(), `SELECT jsonb_build_array(
        (SELECT jsonb_agg(to_jsonb(b)-'tenant_id' ORDER BY currency) FROM capital_balances b),
        (SELECT jsonb_agg(to_jsonb(c)-'tenant_id' ORDER BY order_id,currency) FROM capital_commitments c),
        (SELECT jsonb_agg(to_jsonb(m)-'tenant_id' ORDER BY order_id,currency) FROM capital_members m)
    )::text`).Scan(&s)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPostgresUnexecutedReleaseAgreesWithGoAndReplaysOnce(t *testing.T) {
	native := database(t)
	canonical := tenantPool(t, "go-reference")
	ctx := t.Context()
	for _, p := range []*pgxpool.Pool{native, canonical} {
		seedRelease(t, p)
		if err := invalidateSource(ctx, p, [32]byte{1}); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Exec(ctx, `UPDATE capital_balances SET coverage_complete=false`); err != nil {
			t.Fatal(err)
		}
	}
	if err := transaction(canonical, func(tx pgx.Tx) error {
		if err := LockBalances(ctx, tx, "fund", releaseCurrencies); err != nil {
			return err
		}
		for _, ccy := range releaseCurrencies {
			if err := FinishExecution(ctx, tx, "fund", ccy, "cancel", snapshot(1, "0").ObservedAt.Add(time.Hour)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	type result struct {
		verdict string
		err     error
	}
	results := make(chan result, 8)
	start := make(chan struct{})
	for range 8 {
		go func() {
			<-start
			var got result
			got.err = transaction(native, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, releaseStatement, releaseCurrencies, releaseDebits).Scan(&got.verdict)
			})
			results <- got
		}()
	}
	close(start)
	released := 0
	for range 8 {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		switch r.verdict {
		case "RELEASED":
			released++
		case "UNKNOWN":
		default:
			t.Fatalf("release verdict %q", r.verdict)
		}
	}
	if released != 1 {
		t.Fatalf("concurrent release succeeded %d times", released)
	}
	if got, want := releaseLedger(t, native), releaseLedger(t, canonical); got != want {
		t.Fatalf("release diverged from Go\ngot %s\nwant %s", got, want)
	}
	var unchanged, closed int
	if err := native.QueryRow(ctx, `SELECT
        (SELECT count(*) FROM capital_balances WHERE reserved='1/7'),
        (SELECT count(*) FROM capital_commitments WHERE order_id='cancel' AND version=2 AND required_debit='0')`).Scan(&unchanged, &closed); err != nil || unchanged != 3 || closed != 3 {
		t.Fatalf("remaining cash heads=%d, terminal heads=%d: %v", unchanged, closed, err)
	}
}

func TestPostgresUnexecutedReleaseRefusesAmbiguousOwnershipAndPartialDebits(t *testing.T) {
	p := database(t)
	seedRelease(t, p)
	ctx := t.Context()
	for _, tc := range []struct {
		name, sql           string
		currencies, amounts []string
		want                string
	}{
		{"missing-leg", "", releaseCurrencies[:2], releaseDebits[:2], "UNKNOWN"},
		{"unsorted", "", []string{"USD", "BNB", "ZERO"}, []string{"100", "5/4", "0"}, "INVALID"},
		{"duplicate", "", []string{"BNB", "BNB", "USD"}, releaseDebits, "INVALID"},
		{"unknown-debit", "", releaseCurrencies, []string{"5/4", "100", ""}, "INVALID"},
		{"authorization-mismatch", "", releaseCurrencies, []string{"5/4", "99", "0"}, "UNKNOWN"},
		{"version-changed", `UPDATE capital_commitments SET version=2 WHERE order_id='cancel' AND currency='ZERO'`, releaseCurrencies, releaseDebits, "UNKNOWN"},
		{"required-changed", `UPDATE capital_commitments SET required_debit='0' WHERE order_id='cancel' AND currency='USD'`, releaseCurrencies, releaseDebits, "UNKNOWN"},
		{"legacy-unknown", `UPDATE capital_commitments SET admission_debit=NULL WHERE order_id='cancel' AND currency='ZERO'`, releaseCurrencies, releaseDebits, "UNKNOWN"},
		{"booked", `UPDATE capital_commitments SET booked_debit='1/1000000' WHERE order_id='cancel' AND currency='USD'`, releaseCurrencies, releaseDebits, "UNKNOWN"},
		{"executed", `UPDATE capital_commitments SET executed_debit='1' WHERE order_id='cancel' AND currency='USD'`, releaseCurrencies, releaseDebits, "UNKNOWN"},
		{"member-executed", `UPDATE capital_members SET executed_debit='1' WHERE order_id='cancel' AND currency='USD'`, releaseCurrencies, releaseDebits, "UNKNOWN"},
		{"allocated", `UPDATE capital_commitments SET allocated_debit='1' WHERE order_id='cancel' AND currency='BNB'`, releaseCurrencies, releaseDebits, "UNKNOWN"},
		{"child", `INSERT INTO capital_members(order_id,owner_order_id,currency) VALUES ('child','cancel','USD')`, releaseCurrencies, releaseDebits, "UNKNOWN"},
		{"aggregate-short", `UPDATE capital_balances SET reserved='199/2' WHERE currency='USD'`, releaseCurrencies, releaseDebits, "UNKNOWN"},
		{"bad-cash", `UPDATE capital_balances SET total='?' WHERE currency='ZERO'`, releaseCurrencies, releaseDebits, "UNKNOWN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := p.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if tc.sql != "" {
				if _, err = tx.Exec(ctx, tc.sql); err != nil {
					t.Fatal(err)
				}
			}
			before := releaseLedger(t, tx)
			var outcome string
			if err = tx.QueryRow(ctx, releaseStatement, tc.currencies, tc.amounts).Scan(&outcome); err != nil || outcome != tc.want {
				t.Fatalf("release=%q want %q: %v", outcome, tc.want, err)
			}
			if after := releaseLedger(t, tx); after != before {
				t.Fatal("refusal changed the ledger")
			}
		})
	}
	before := releaseLedger(t, p)
	if _, err := p.Exec(ctx, `CREATE FUNCTION suppress_one_release() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
        IF NEW.currency='ZERO' THEN RETURN NULL; END IF; RETURN NEW; END $$;
        CREATE TRIGGER suppress_one_release BEFORE UPDATE ON capital_balances FOR EACH ROW EXECUTE FUNCTION suppress_one_release()`); err != nil {
		t.Fatal(err)
	}
	var verdict string
	if err := p.QueryRow(ctx, releaseStatement, releaseCurrencies, releaseDebits).Scan(&verdict); err == nil {
		t.Fatal("partial terminal write succeeded")
	}
	if after := releaseLedger(t, p); after != before {
		t.Fatal("failed terminal update left a partial release")
	}
}

func TestPostgresUnexecutedReleaseTakesCashBeforeCommitments(t *testing.T) {
	p := database(t)
	seedRelease(t, p)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	blocker, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	var blockPID int
	if err = blocker.QueryRow(ctx, `SELECT pg_backend_pid() FROM capital_balances WHERE currency='USD' FOR UPDATE`).Scan(&blockPID); err != nil {
		t.Fatal(err)
	}
	release, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release.Rollback(ctx)
	var pid int
	if err = release.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		var outcome string
		e := release.QueryRow(ctx, releaseStatement, releaseCurrencies, releaseDebits).Scan(&outcome)
		if e == nil && outcome != "RELEASED" {
			e = errors.New("terminal release refused")
		}
		done <- e
	}()
	for {
		var blocked bool
		if err = p.QueryRow(ctx, `SELECT $1::int=ANY(pg_blocking_pids($2::int))`, blockPID, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	// The preceding BNB balance is locked. The commitment is still available
	// while the terminal write waits for the next cash leg.
	var pgerr *pgconn.PgError
	if _, err = p.Exec(ctx, `SELECT FROM capital_balances WHERE currency='BNB' FOR UPDATE NOWAIT`); !errors.As(err, &pgerr) || pgerr.Code != "55P03" {
		t.Fatalf("first cash lock: %v", err)
	}
	if _, err = p.Exec(ctx, `SELECT FROM capital_commitments WHERE order_id='cancel' AND currency='BNB' FOR UPDATE NOWAIT`); err != nil {
		t.Fatalf("commitment locked before all cash: %v", err)
	}
	if err = blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
