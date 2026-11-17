package custody

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

// A later backdated append and a changed declaration must not alter the book
// slice recoverable for an earlier completed run.
func TestRunRecordsItsBasis(t *testing.T) {
	pool := newCustodyPool(t, "custody-basis")
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	ctx := bus.WithTenantID(t.Context(), "custody-basis")
	st, journal := NewPostgres(pool), ledger.NewPostgres(pool)
	seedEntry(t, journal, "a1", "PF1", "okx-sub-1", "AAPL", 10, -10)
	seedEntry(t, journal, "b1", "PF1", "bin-main", "AAPL", 20, -20)
	seedEntry(t, journal, "other", "PF2", "okx-sub-1", "AAPL", 100, -100)
	if err := st.SaveStatement(ctx, statement("statement", map[string]int64{"AAPL": 9}, nil)); err != nil {
		t.Fatal(err)
	}
	reconcile := func(scope *BookScope, when time.Time) Run {
		t.Helper()
		r, err := NewReconciler(st, LedgerBookLoader(journal, scope, testLogger()), nil, nil, nil, testLogger(), func() time.Time { return when })
		if err != nil {
			t.Fatal(err)
		}
		run, err := r.Reconcile(ctx, subject())
		if err != nil || run.Outcome != OutcomeBreaks {
			t.Fatalf("reconcile: %+v, %v", run, err)
		}
		return run
	}
	now := t0.Add(48 * time.Hour)
	first := reconcile(twoCustodianScope(t), now)
	fresh := NewPostgres(pool)
	read, err := fresh.LatestRun(ctx, "PF1", "CUST-A")
	want := &RunBasis{Mode: BasisDeclared, Accounts: map[string][]string{"CUST-A": {"okx-sub-1"}, "CUST-B": {"bin-main"}}, JournalPosition: 2}
	if err != nil || read.RunID != first.RunID || !reflect.DeepEqual(read.Basis, want) {
		t.Fatalf("stored comparison basis: got %+v, want %+v, err=%v", read.Basis, want, err)
	}
	seedEntry(t, journal, "late-a2", "PF1", "okx-sub-1", "AAPL", 7, -7)
	// Accounting's WORM append index begins after the immutable legacy prefix.
	// Select from the persisted run only; the process declaration may be gone.
	var ids []string
	err = pool.QueryRow(ctx, `
	SELECT array_agg(e.entry_id ORDER BY e.entry_id)
	FROM custody_runs r JOIN ledger_entries e USING (tenant_id, portfolio_id)
	LEFT JOIN ledger_append_positions p USING (tenant_id, portfolio_id, entry_id)
	WHERE r.run_id=$1 AND (r.book_basis->'accounts'->r.custodian_id) ? e.venue_account_id
	  AND (p.position IS NULL OR p.position <= (r.book_basis->>'journal_position')::bigint)
	  AND e.effective_time < ((r.business_date + 1)::timestamp AT TIME ZONE 'UTC')`, first.RunID).Scan(&ids)
	if err != nil || !reflect.DeepEqual(ids, []string{"a1"}) {
		t.Fatalf("original book slice after late append: %v, %v", ids, err)
	}
	unchanged := reconcile(twoCustodianScope(t), now.Add(time.Hour))
	for _, b := range unchanged.Breaks {
		if !b.FirstSeenAt.Equal(now) {
			t.Fatalf("same account assignment lost break age: %+v", b)
		}
	}
	swapped, err := NewBookScope([]Subject{{PortfolioID: "PF1", CustodianID: "CUST-A"}, {PortfolioID: "PF1", CustodianID: "CUST-B"}}, map[string]map[string][]string{"PF1": {"CUST-A": {"bin-main"}, "CUST-B": {"okx-sub-1"}}})
	if err != nil {
		t.Fatal(err)
	}
	changed := reconcile(swapped, now.Add(2*time.Hour))
	if len(changed.Breaks) == 0 {
		t.Fatal("missing redetected breaks")
	}
	for _, b := range changed.Breaks {
		if !b.FirstSeenAt.Equal(changed.CompletedAt) || b.Status != BreakOpen {
			t.Fatalf("changed scope inherited prior break: %+v", b)
		}
	}
	retry, err := fresh.CommitRun(ctx, first)
	if err != nil || !reflect.DeepEqual(retry.Basis, want) {
		t.Fatalf("immutable first basis on retry: %+v, %v", retry.Basis, err)
	}
	if _, err := NewPostgres(newCustodyPool(t, "another-tenant")).LatestRun(t.Context(), "PF1", "CUST-A"); !errors.Is(err, ErrNoRun) {
		t.Fatalf("cross-tenant basis disclosure: %v", err)
	}
	// No pre-migration record can prove its account set. An older writer still
	// active during rollout also omits the new column.
	if _, err := pool.Exec(ctx, `INSERT INTO custody_runs (run_id,portfolio_id,custodian_id,business_date,outcome,tolerance,completed_at,values_verified)
		SELECT 'legacy','PF1','CUST-A',$1,'clean','0',$2,true FROM set_config('app.custody_exact_writer','v1',true)`, BusinessDay(t0), now.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	legacy, err := fresh.LatestRun(ctx, "PF1", "CUST-A")
	if err != nil || legacy.RunID != "legacy" || legacy.Basis != nil {
		t.Fatalf("legacy run acquired a basis: %+v, %v", legacy, err)
	}
}
