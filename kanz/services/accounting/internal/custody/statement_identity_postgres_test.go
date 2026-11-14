package custody

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestPostgresStatementIdentityRefusalRestartAndIsolation(t *testing.T) {
	const tenant = "custody-statement-identity"
	pool := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	store := NewPostgres(pool)
	for _, change := range statementSubjectChanges() {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reverse=%t", change.name, reverse), func(t *testing.T) {
				assertStatementIdentityRefusal(t, store, change, reverse)
			})
		}
	}
	assertStatementIdentityRestatement(t, store)
	original := statement("restart-source", map[string]int64{"AAPL": 90}, nil)
	original.PortfolioID = "RESTART"
	if err := store.SaveStatement(t.Context(), original); err != nil {
		t.Fatal(err)
	}
	var before []byte
	if err := pool.QueryRow(t.Context(), `SELECT to_jsonb(s) FROM custody_statements s WHERE statement_id=$1`, original.StatementID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	pool.Close()
	reconnected := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, reconnected)
	restarted := NewPostgres(reconnected)
	for _, change := range statementSubjectChanges() {
		conflicting := cloneStatement(original)
		change.change(&conflicting)
		if err := restarted.SaveStatement(t.Context(), conflicting); !errors.Is(err, ErrStatementIdentityConflict) {
			t.Fatalf("restart forgot the source identity: %s %v", change.name, err)
		}
	}
	var after []byte
	if err := reconnected.QueryRow(t.Context(), `SELECT to_jsonb(s) FROM custody_statements s WHERE statement_id=$1`, original.StatementID).Scan(&after); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("conflict changed stored source columns: %v", err)
	}
	other := newCustodyPool(t, tenant+"-other")
	requireRestrictedCustodyRole(t, other)
	foreignStore := NewPostgres(other)
	if _, err := foreignStore.LatestStatement(t.Context(), original.Subject()); !errors.Is(err, ErrNoStatement) {
		t.Fatalf("foreign tenant discovered source statement: %v", err)
	}
	foreign := cloneStatement(original)
	foreign.PortfolioID = "FOREIGN"
	if err := foreignStore.SaveStatement(t.Context(), foreign); err != nil {
		t.Fatalf("another tenant's same ID was treated as a conflict: %v", err)
	}
	if _, err := restarted.LatestStatement(t.Context(), foreign.Subject()); !errors.Is(err, ErrNoStatement) {
		t.Fatalf("foreign same-ID row leaked back to the original tenant: %v", err)
	}
	for _, own := range []*Postgres{restarted, foreignStore} {
		var count int
		if err := own.pool.QueryRow(t.Context(), `SELECT count(*) FROM custody_statements WHERE statement_id=$1`, original.StatementID).Scan(&count); err != nil || count != 1 {
			t.Fatalf("RLS exposed or merged tenant identities: count=%d err=%v", count, err)
		}
	}
}

func TestPostgresStatementIdentityArbitratesConcurrentWriters(t *testing.T) {
	pool := newCustodyPool(t, "custody-statement-identity-race")
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	// Pause the first writer AFTER its unique-index insertion but before commit.
	// The second must wait on that transaction, not merely execute after a Go
	// goroutine happens to finish. Production SQL still arbitrates the conflict.
	if _, err := pool.Exec(t.Context(), `
CREATE FUNCTION custody_identity_pause() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.statement_id LIKE 'identity-race-%' THEN
    PERFORM pg_advisory_xact_lock(1698,1);
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER custody_identity_pause AFTER INSERT ON custody_statements
FOR EACH ROW EXECUTE FUNCTION custody_identity_pause()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DROP TRIGGER custody_identity_pause ON custody_statements; DROP FUNCTION custody_identity_pause()`); err != nil {
			t.Error(err)
		}
	})
	store := NewPostgres(pool)
	for _, change := range statementSubjectChanges() {
		t.Run(change.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			controller, err := pool.Acquire(ctx)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			if _, err := controller.Exec(ctx, `SELECT pg_advisory_lock(1698,1)`); err != nil {
				controller.Release()
				cancel()
				t.Fatal(err)
			}
			type result struct {
				first bool
				err   error
			}
			results := make(chan result, 2)
			locked, started, completed := true, 0, 0
			defer func() {
				if locked {
					_, _ = controller.Exec(context.Background(), `SELECT pg_advisory_unlock(1698,1)`)
				}
				controller.Release()
				cancel()
				for completed < started {
					select {
					case <-results:
						completed++
					case <-time.After(5 * time.Second):
						t.Error("statement writer did not join")
						return
					}
				}
			}()
			waitFor := func(query string) {
				t.Helper()
				for {
					var waiting int
					if err := pool.QueryRow(ctx, query).Scan(&waiting); err != nil {
						t.Fatal(err)
					}
					if waiting > 0 {
						return
					}
					select {
					case <-ctx.Done():
						t.Fatal("concurrent writer did not reach its database lock wait")
					case <-time.After(10 * time.Millisecond):
					}
				}
			}
			first := statement("identity-race-"+change.name, map[string]int64{"AAPL": 90}, nil)
			first.PortfolioID = "RACE-" + change.name
			second := cloneStatement(first)
			change.change(&second)
			started++
			go func() { results <- result{first: true, err: store.SaveStatement(ctx, first)} }()
			waitFor(`SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND classid=1698 AND objid=1 AND NOT granted`)
			started++
			go func() { results <- result{err: store.SaveStatement(ctx, second)} }()
			waitFor(`SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND usename=current_user AND wait_event_type='Lock' AND wait_event='transactionid' AND query LIKE '%INSERT INTO custody_statements%'`)
			if _, err := controller.Exec(ctx, `SELECT pg_advisory_unlock(1698,1)`); err != nil {
				t.Fatal(err)
			}
			locked = false
			for completed < started {
				select {
				case got := <-results:
					completed++
					if (got.first && got.err != nil) || (!got.first && !errors.Is(got.err, ErrStatementIdentityConflict)) {
						t.Fatalf("concurrent identity arbitration failed: first=%t err=%v", got.first, got.err)
					}
				case <-ctx.Done():
					t.Fatal("concurrent statements did not finish")
				}
			}
			if got, err := store.LatestStatement(ctx, first.Subject()); err != nil || got.StatementID != first.StatementID {
				t.Fatalf("first committed identity was lost: %+v %v", got, err)
			}
			if _, err := store.LatestStatement(ctx, second.Subject()); !errors.Is(err, ErrNoStatement) {
				t.Fatalf("losing concurrent subject gained source evidence: %v", err)
			}
		})
	}
}
