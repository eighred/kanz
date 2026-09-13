package custody

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/services/accounting/internal/recon"
)

func TestPostgresCustodyActionConcurrentRetriesAndIsolation(t *testing.T) {
	st, ctx := newCustodyStore(t)
	var privileged bool
	if err := st.pool.QueryRow(ctx, `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&privileged); err != nil || privileged {
		t.Fatalf("requires nonprivileged RLS role: privileged=%v err=%v", privileged, err)
	}
	if _, err := st.UpsertBreaks(ctx, subject(), detectedQuantityBreak(t0, 100, 90), t0); err != nil {
		t.Fatal(err)
	}
	id := BreakID("PF1", "CUST-A", recon.BreakQuantity, "AAPL")
	a := Action{Tenant: "__system__", Actor: "alice", RequestID: "claim-1", BreakID: id, Kind: "claim", ExpectedRevision: 1}
	const n = 16
	results := make([]ActionEvidence, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i], errs[i] = st.ApplyAction(ctx, a) }(i)
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil || !reflect.DeepEqual(results[0], results[i]) {
			t.Fatalf("retry %d: %+v %v", i, results[i], errs[i])
		}
	}
	e := results[0]
	if e.Actor != "alice" || e.Before.Revision != 1 || e.After.Revision != 2 || e.After.Assignee != "alice" {
		t.Fatalf("evidence: %+v", e)
	}
	replay, err := NewPostgres(st.pool).ApplyAction(ctx, a)
	if err != nil || !reflect.DeepEqual(e, replay) {
		t.Fatalf("replica replay: %+v %v", replay, err)
	}
	changed := a
	changed.Actor = "bob"
	if _, err := st.ApplyAction(ctx, changed); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("actor conflict: %v", err)
	}
	changed = a
	changed.Kind = "explain"
	changed.Explanation = "different decision"
	if _, err := st.ApplyAction(ctx, changed); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("payload conflict: %v", err)
	}
	changed = a
	changed.RequestID = "new-stale"
	if _, err := st.ApplyAction(ctx, changed); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("stale: %v", err)
	}
	var count int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM custody_actions`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("evidence count %d: %v", count, err)
	}
	records, err := outbox.NewPostgres(st.pool, "accounting").Pending(ctx, id, 100)
	if err != nil || len(records) != 1 || records[0].Record.Subject != SubjectActionRecorded || records[0].Record.TenantID != a.Tenant {
		t.Fatalf("outbox: %+v %v", records, err)
	}
	if _, err := records[0].Record.Event(); err != nil {
		t.Fatalf("FACT cannot be decoded: %v", err)
	}
	for _, sql := range []string{`UPDATE custody_actions SET actor='mallory'`, `DELETE FROM custody_actions`, `TRUNCATE custody_actions`} {
		if _, err := st.pool.Exec(ctx, sql); err == nil {
			t.Fatalf("evidence mutation accepted: %s", sql)
		}
	}
	other := NewPostgres(newCustodyPool(t, "other-tenant"))
	if err := other.pool.QueryRow(ctx, `SELECT count(*) FROM custody_actions`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("RLS evidence count %d: %v", count, err)
	}
	changed = a
	changed.Tenant = "other-tenant"
	if _, err := other.ApplyAction(ctx, changed); !errors.Is(err, ErrNoBreak) {
		t.Fatalf("foreign break: %v", err)
	}
	if _, err := st.ApplyAction(ctx, changed); !errors.Is(err, ErrNoBreak) {
		t.Fatalf("tenant mismatch: %v", err)
	}
}

func TestPostgresCustodyActionOutboxFailureRollsBackEverything(t *testing.T) {
	st, ctx := newCustodyStore(t)
	if _, err := st.UpsertBreaks(ctx, subject(), detectedQuantityBreak(t0, 100, 90), t0); err != nil {
		t.Fatal(err)
	}
	id := BreakID("PF1", "CUST-A", recon.BreakQuantity, "AAPL")
	if _, err := st.pool.Exec(ctx, `CREATE OR REPLACE FUNCTION reject_test_custody_fact() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected outbox failure'; END; $$; CREATE TRIGGER reject_test_custody_fact BEFORE INSERT ON outbox FOR EACH ROW EXECUTE FUNCTION reject_test_custody_fact()`); err != nil {
		t.Fatal(err)
	}
	a := Action{Tenant: "__system__", Actor: "alice", RequestID: "rollback", BreakID: id, Kind: "claim", ExpectedRevision: 1}
	if _, err := st.ApplyAction(ctx, a); err == nil {
		t.Fatal("accepted without FACT")
	}
	b, err := st.LoadBreak(ctx, id)
	if err != nil || b.Status != BreakOpen || b.Revision != 1 || b.Assignee != "" {
		t.Fatalf("partial state: %+v %v", b, err)
	}
	var count int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM custody_actions`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial evidence %d: %v", count, err)
	}
	if _, err := st.pool.Exec(ctx, `DROP TRIGGER reject_test_custody_fact ON outbox`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyAction(ctx, a); err != nil {
		t.Fatalf("retry after repair: %v", err)
	}
}

func TestPostgresCustodyActionCannotResurrectReconciledBreak(t *testing.T) {
	st, ctx := newCustodyStore(t)
	id := BreakID("PF1", "CUST-A", recon.BreakQuantity, "AAPL")
	for i := 0; i < 16; i++ {
		now := t0.Add(time.Duration(i) * time.Hour)
		if _, err := st.UpsertBreaks(ctx, subject(), detectedQuantityBreak(now, 100, 90), now); err != nil {
			t.Fatal(err)
		}
		before, err := st.LoadBreak(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		a := Action{Tenant: "__system__", Actor: "alice", RequestID: fmt.Sprintf("race-%d", i), BreakID: id, Kind: "claim", ExpectedRevision: before.Revision}
		start := make(chan struct{})
		finished := make(chan error, 1)
		go func() { <-start; _, err := st.ApplyAction(ctx, a); finished <- err }()
		close(start)
		if _, err := st.UpsertBreaks(ctx, subject(), nil, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		actionErr := <-finished
		if actionErr != nil && !errors.Is(actionErr, ErrStaleRevision) {
			t.Fatalf("action race: %v", actionErr)
		}
		after, err := st.LoadBreak(ctx, id)
		if err != nil || after.Status != BreakResolved || after.Revision <= before.Revision {
			t.Fatalf("resurrected: %+v %v", after, err)
		}
		if actionErr == nil {
			if _, err := st.ApplyAction(ctx, a); err != nil {
				t.Fatalf("historical replay: %v", err)
			}
			replayed, err := st.LoadBreak(ctx, id)
			if err != nil || replayed.Status != BreakResolved || replayed.Revision != after.Revision {
				t.Fatalf("replay changed current state: %+v %v", replayed, err)
			}
		}
	}
}
