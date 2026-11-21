package custody

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func requireRestrictedCustodyRole(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var super, bypass bool
	if err := pool.QueryRow(t.Context(), `SELECT rolsuper,rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&super, &bypass); err != nil || super || bypass {
		t.Fatalf("custody atomicity requires NOSUPERUSER NOBYPASSRLS: super=%v bypass=%v err=%v", super, bypass, err)
	}
}

func TestPostgresCustodyRunFailuresRollBackLifecycle(t *testing.T) {
	for _, failure := range []string{"run_insert", "outbox_insert"} {
		for _, transition := range []string{"resolve", "redetect"} {
			t.Run(failure+"/"+transition, func(t *testing.T) {
				pool := newCustodyPool(t, "atomic-custody")
				requireRestrictedCustodyRole(t, pool)
				applyCustodySchema(t, pool)
				st := NewPostgres(pool)
				ctx := bus.WithTenantID(t.Context(), "atomic-custody")
				seed := detectedQuantityBreak(t0, 100, 90)
				if _, err := st.UpsertBreaks(ctx, subject(), seed, t0); err != nil {
					t.Fatal(err)
				}
				quantity := int64(100)
				if transition == "redetect" {
					quantity = 80
				}
				if err := st.SaveStatement(ctx, statement("S1", map[string]int64{"AAPL": quantity}, nil)); err != nil {
					t.Fatal(err)
				}
				// Test-only constraints fail the real SQL write after lifecycle SQL
				// has executed. No production failure hook or privileged role is used.
				constraint := `ALTER TABLE custody_runs ADD CONSTRAINT custody_test_refuse_run CHECK (false) NOT VALID`
				if failure == "outbox_insert" {
					constraint = `ALTER TABLE outbox ADD CONSTRAINT custody_test_refuse_fact CHECK (event_type <> 'accounting.custody.reconciled') NOT VALID`
				}
				if _, err := pool.Exec(ctx, constraint); err != nil {
					t.Fatal(err)
				}
				pub := &capturePublisher{}
				r, err := NewReconciler(st, loaderFor(bookWith(map[string]int64{"AAPL": 100}, nil)), pub, nil, nil, testLogger(), func() time.Time { return t0.Add(time.Hour) })
				if err != nil {
					t.Fatal(err)
				}
				if _, err := r.Reconcile(ctx, subject()); err == nil || errors.Is(err, ErrPublicationPending) {
					t.Fatalf("SQL refusal confused with committed/pending result: %v", err)
				}
				b, err := st.LoadBreak(ctx, seed[0].BreakID)
				if err != nil || b.Status != BreakOpen || b.Revision != 1 || b.Diff.Cmp(big.NewRat(10, 1)) != 0 || !b.LastSeenAt.Equal(t0) {
					t.Fatalf("failed run left lifecycle changes: %+v err=%v", b, err)
				}
				var runs, facts int
				if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM custody_runs),(SELECT count(*) FROM outbox WHERE event_type=$1)`, SubjectRun).Scan(&runs, &facts); err != nil || runs != 0 || facts != 0 || len(pub.events) != 0 {
					t.Fatalf("partial commit: runs=%d facts=%d published=%d err=%v", runs, facts, len(pub.events), err)
				}
			})
		}
	}
}

func TestPostgresCustodyRunRetryPreservesOriginalCompletionAndTenant(t *testing.T) {
	pool := newCustodyPool(t, "custody-a")
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	st := NewPostgres(pool)
	ctx := bus.WithTenantID(t.Context(), "custody-a")
	run := Run{RunID: "same-run", Subject: subject(), Outcome: OutcomeBreaks, Breaks: detectedQuantityBreak(t0, 100, 90), CompletedAt: t0, Tolerance: new(big.Rat)}
	first, err := st.CommitRun(ctx, run)
	if err != nil || len(first.Breaks) != 1 {
		t.Fatalf("first commit: %+v %v", first, err)
	}
	// A later run changes the current lifecycle. Retrying the first must still
	// return its original wire evidence, without another transition or FACT.
	later := run
	later.RunID, later.CompletedAt, later.Breaks = "later-run", t0.Add(time.Hour), detectedQuantityBreak(t0, 100, 80)
	if _, err := st.CommitRun(ctx, later); err != nil {
		t.Fatal(err)
	}
	again, err := st.CommitRun(ctx, run)
	if err != nil || len(again.Breaks) != 1 || again.Breaks[0].Diff.Cmp(big.NewRat(10, 1)) != 0 || !again.CompletedAt.Equal(t0) {
		t.Fatalf("retry did not return original completion: %+v %v", again, err)
	}
	b, err := st.LoadBreak(ctx, first.Breaks[0].BreakID)
	if err != nil || b.Revision != 2 || b.Diff.Cmp(big.NewRat(20, 1)) != 0 {
		t.Fatalf("retry changed lifecycle: %+v %v", b, err)
	}
	rows, err := st.Outbox().Pending(ctx, pairKey("PF1", "CUST-A"), 10)
	if err != nil || len(rows) != 2 || rows[0].Record.TenantID != "custody-a" || rows[0].Record.CorrelationID != run.RunID {
		t.Fatalf("retry lost/duplicated completion: %+v %v", rows, err)
	}
	if err := st.Outbox().MarkPublished(ctx, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM outbox WHERE id=$1 AND published_at IS NOT NULL", rows[0].ID); err != nil {
		t.Fatal(err)
	}
	afterRetention, err := NewPostgres(pool).CommitRun(ctx, run)
	if err != nil || len(afterRetention.Breaks) != 1 || afterRetention.Breaks[0].Diff.Cmp(big.NewRat(10, 1)) != 0 || !afterRetention.CompletedAt.Equal(t0) {
		t.Fatalf("original scheduled comparison after relay retention: %+v %v", afterRetention, err)
	}
	rows, err = st.Outbox().Pending(ctx, pairKey("PF1", "CUST-A"), 10)
	if err != nil || len(rows) != 1 || rows[0].Record.CorrelationID != later.RunID {
		t.Fatalf("receipt replay republished a completion: %+v %v", rows, err)
	}
	conflict := run
	conflict.Subject.CustodianID = "OTHER"
	if _, err := st.CommitRun(ctx, conflict); !errors.Is(err, ErrRunIdentityConflict) {
		t.Fatalf("run ID reused across subjects: %v", err)
	}
	if _, err := st.CommitRun(bus.WithTenantID(t.Context(), "custody-b"), later); err == nil {
		t.Fatal("dedicated store accepted a foreign resolved tenant")
	}
	other := NewPostgres(newCustodyPool(t, "custody-b"))
	if _, err := other.LatestRun(t.Context(), "PF1", "CUST-A"); !errors.Is(err, ErrNoRun) {
		t.Fatalf("cross-tenant run disclosure: %v", err)
	}
	if _, err := other.LoadBreak(t.Context(), first.Breaks[0].BreakID); !errors.Is(err, ErrNoBreak) {
		t.Fatalf("cross-tenant lifecycle disclosure: %v", err)
	}
	if rows, err := other.Outbox().Pending(t.Context(), pairKey("PF1", "CUST-A"), 10); err != nil || len(rows) != 0 {
		t.Fatalf("cross-tenant completion disclosure: %+v %v", rows, err)
	}
}

func TestPostgresCustodyRunLegacyCompletionRetention(t *testing.T) {
	const tenant = "legacy-custody"
	pool := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	ctx := bus.WithTenantID(t.Context(), tenant)
	store := NewPostgres(pool)
	run := Run{RunID: "scheduled-before-upgrade", Subject: subject(), Outcome: OutcomeNoStatement, CompletedAt: t0, Tolerance: new(big.Rat)}
	for _, manual := range []bool{false, true} {
		if manual {
			run.RequestID = "4e13195f-83c4-4258-b4f8-519126590200"
			run.RunID, run.Actor = "manual:"+run.RequestID, "original-operator"
		}
		if err := store.SaveRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		record, err := runRecord(ctx, tenant, run)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, "UPDATE custody_runs SET manual_receipt=$1 WHERE run_id=$2", record.Payload, run.RunID); err == nil {
			t.Fatal("missing original FACT admitted a legacy completion")
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if manual {
			// Legacy manual writers set the receipt before enqueuing their FACT.
			if _, err := tx.Exec(ctx, "UPDATE custody_runs SET manual_receipt=$1 WHERE run_id=$2", record.Payload, run.RunID); err != nil {
				t.Fatal(err)
			}
		}
		if err := outbox.Enqueue(ctx, tx, outbox.ServiceAccounting, record); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		replayed, err := store.CommitRun(ctx, run)
		if err != nil || replayed.RunID != run.RunID || replayed.Outcome != OutcomeNoStatement || replayed.Actor != run.Actor {
			t.Fatalf("legacy completion: %+v %v", replayed, err)
		}
		queue := store.Outbox()
		rows, err := queue.Pending(ctx, pairKey("PF1", "CUST-A"), 10)
		if err != nil || len(rows) != 1 {
			t.Fatalf("legacy relay: %v %v", rows, err)
		}
		if err := queue.MarkPublished(ctx, rows[0].ID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, "DELETE FROM outbox WHERE id=$1 AND published_at IS NOT NULL", rows[0].ID); err != nil {
			t.Fatal(err)
		}
		_, err = store.CommitRun(ctx, run)
		if (manual && err != nil) || (!manual && !errors.Is(err, ErrRunCompletionMissing)) {
			t.Fatalf("manual=%t after relay retention: %v", manual, err)
		}
	}
	for _, query := range []string{
		"UPDATE custody_runs SET manual_receipt=NULL WHERE run_id=$1",
		"UPDATE custody_runs SET book_basis='{}'::jsonb WHERE run_id=$1",
		"DELETE FROM custody_runs WHERE run_id=$1",
	} {
		_, err := pool.Exec(ctx, query, run.RunID)
		var dbErr *pgconn.PgError
		if !errors.As(err, &dbErr) || dbErr.Code != "P0001" {
			t.Fatalf("retained completion mutation: expected immutability refusal, got %v", err)
		}
	}
	_, err := pool.Exec(ctx, "TRUNCATE custody_runs CASCADE")
	var dbErr *pgconn.PgError
	if !errors.As(err, &dbErr) || dbErr.Code != "P0001" {
		t.Fatalf("retained completion truncate: expected immutability refusal, got %v", err)
	}
}

func TestPostgresCustodyRunKeepsResolvedEnvelopeTenant(t *testing.T) {
	pool := newCustodyPool(t, bus.SystemTenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	st := NewPostgres(pool)
	ctx := bus.WithTenantID(t.Context(), "resolved-customer")
	if _, err := st.CommitRun(ctx, Run{RunID: "customer-run", Subject: subject(), Outcome: OutcomeNoStatement, CompletedAt: t0, Tolerance: new(big.Rat)}); err != nil {
		t.Fatal(err)
	}
	rows, err := outbox.NewPostgres(pool, outbox.ServiceAccounting).Pending(ctx, pairKey("PF1", "CUST-A"), 10)
	if err != nil || len(rows) != 1 || rows[0].Record.TenantID != "resolved-customer" {
		t.Fatalf("resolved customer was replaced with storage scope: %+v %v", rows, err)
	}
}

func TestPostgresCustodyRunBusyRefusesWithoutPartialCommit(t *testing.T) {
	pool := newCustodyPool(t, "custody-busy")
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(app_current_tenant() || ':custody-run:' || $1,0))`, pairKey("PF1", "CUST-A")); err != nil {
		t.Fatal(err)
	}
	run := Run{RunID: "busy", Subject: subject(), Outcome: OutcomeBreaks, Breaks: detectedQuantityBreak(t0, 100, 90), CompletedAt: t0, Tolerance: new(big.Rat)}
	st := NewPostgres(pool)
	if _, err := st.CommitRun(ctx, run); !errors.Is(err, ErrRunBusy) {
		t.Fatalf("pair contention did not refuse immediately: %v", err)
	}
	if _, err := st.LatestRun(ctx, "PF1", "CUST-A"); !errors.Is(err, ErrNoRun) {
		t.Fatalf("busy run committed: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CommitRun(ctx, run); err != nil {
		t.Fatalf("released pair did not commit: %v", err)
	}
}
