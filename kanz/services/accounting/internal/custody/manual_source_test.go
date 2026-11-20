package custody

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/google/uuid"
)

// The importer writes the WORM receipt; its real signed-only consumer accepts
// the exact emitted bytes. Each call advances the same source revision chain.
func manualSignedFeed(t *testing.T, ctx context.Context, st *Postgres) func(int, int) string {
	t.Helper()
	write := signedStatementFeed(t, ctx, st)
	return func(qty, cash int) string {
		position, currencies := "[]", "[]"
		if qty != 0 {
			position = fmt.Sprintf(`[{"instrumentId":"AAPL","quantity":{"coefficient":"%d"}}]`, qty)
		}
		if cash != 0 {
			currencies = fmt.Sprintf(`[{"currencyCode":"USD","balance":{"coefficient":"%d"}}]`, cash)
		}
		stmt := json.RawMessage(fmt.Sprintf(`{"portfolioId":"PF1","custodianId":"CUST-A","businessDate":"%s","positions":%s,"cash":%s,"transactions":[],"grain":"STATEMENT_GRAIN_BALANCES_ONLY"}`, BusinessDay(t0).Format(time.RFC3339), position, currencies))
		return write(stmt)
	}
}

func signedStatementFeed(t *testing.T, ctx context.Context, st *Postgres) func(json.RawMessage) string {
	t.Helper()
	return signedStatementFeedForAccount(t, ctx, st, "ACC1")
}
func signedStatementFeedForAccount(t *testing.T, ctx context.Context, st *Postgres, account string) func(json.RawMessage) string {
	t.Helper()
	retain := retainedStatementFeedForAccount(t, ctx, st, account)
	return func(stmt json.RawMessage) string {
		t.Helper()
		id, deliver := retain(stmt)
		deliver()
		return id
	}
}

// Keep the committed import and its signed consumer delivery as two real steps.
func retainedStatementFeedForAccount(t *testing.T, ctx context.Context, st *Postgres, account string) func(json.RawMessage) (string, func()) {
	t.Helper()
	tenant := bus.TenantIDFromContext(ctx)
	a, key := exampleAuthority(t, tenant)
	a.Account, a.Instruments = account, []string{"AAPL"}
	importer, err := NewBatchImporter(a, tenant, []string{"PF1:CUST-A"}, "PF1:CUST-A:"+account, st.pool, &capturePublisher{}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := NewSignedBatchConsumer(st, tenant, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	revision, previous := int64(0), ""
	return func(stmt json.RawMessage) (string, func()) {
		t.Helper()
		revision++
		raw, err := json.Marshal(signedBatch{BatchFormat, a.Source, tenant, a.Account, revision, previous, true, stmt})
		if err != nil {
			t.Fatal(err)
		}
		id, err := importer.Import(ctx, raw, ed25519.Sign(key, raw))
		if err != nil {
			t.Fatal(err)
		}
		previous = fmt.Sprintf("%x", sha256.Sum256(raw))
		var payload []byte
		if err := st.pool.QueryRow(ctx, `SELECT o.payload FROM custody_batch_receipts r JOIN outbox o ON o.tenant_id=r.tenant_id AND o.producer_service=$2 AND o.event_time=r.received_at WHERE r.statement_id=$1`, id, BatchProducer).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		return id, func() {
			t.Helper()
			if err := consumer.Handle(ctx, &envelopepb.Envelope{TenantId: tenant, Source: BatchProducer}, payload); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestManualDatedComparisonRequiresMatchingSignedWinner(t *testing.T) {
	const tenant = "manual-source"
	pool := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	ctx := bus.WithTenantID(t.Context(), tenant)
	st := NewPostgres(pool)
	journal := ledger.NewPostgres(pool)
	if err := journal.Append(ctx, closeTrade("initial", 10, -100, t0), nil); err != nil {
		t.Fatal(err)
	}
	sub := subject()
	sub.BusinessDate = BusinessDay(t0)
	r := manualReconciler(t, st, LedgerBookLoader(journal, closeScope(t, true), testLogger()), &capturePublisher{})
	signed := manualSignedFeed(t, ctx, st)
	signed(8, -100)
	opened, err := r.ReconcileManual(ctx, sub, uuid.NewString(), "operator")
	if err != nil || len(opened.Breaks) != 1 {
		t.Fatalf("signed initial comparison: %+v %v", opened, err)
	}
	assertRefused := func(id string) {
		t.Helper()
		key := uuid.NewString()
		for range 2 {
			got, err := r.ReconcileManual(ctx, sub, key, "operator")
			if !errors.Is(err, errBatchReceipt) || got.Outcome != OutcomeFailed || got.StatementID != id || len(got.Breaks) != 0 {
				t.Fatalf("unverified result: %+v %v", got, err)
			}
		}
		b, err := st.LoadBreak(ctx, opened.Breaks[0].BreakID)
		if err != nil || b.Status != BreakOpen || b.Revision != opened.Breaks[0].Revision {
			t.Fatalf("unverified input changed open break: %+v %v", b, err)
		}
	}
	legacy := statement("resident-legacy", map[string]int64{"AAPL": 10}, map[string]int64{"USD": -100})
	legacy.ReceivedAt = time.Now().UTC().Add(time.Hour)
	if err := st.SaveStatement(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	assertRefused(legacy.StatementID)
	// A plausible reserved ID also needs its own immutable receipt.
	fakeID := batchStatementPrefix + "unreceipted"
	if _, err := pool.Exec(ctx, "UPDATE custody_statements SET statement_id=$1 WHERE statement_id=$2", fakeID, legacy.StatementID); err != nil {
		t.Fatal(err)
	}
	assertRefused(fakeID)
	// Simulate materialized rows inherited from an earlier deployment or restore.
	if _, err := pool.Exec(ctx, "UPDATE custody_statements SET received_at=$1 WHERE statement_id=$2", t0, fakeID); err != nil {
		t.Fatal(err)
	}
	corrected := signed(9, -100)
	// The restored row still advertises exact values. The signed receipt must
	// catch divergence even when the independent numeric-precision guard passes.
	tx, err := st.exactTransaction(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE custody_statements SET positions='{"AAPL":"10"}' WHERE statement_id=$1`, corrected); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	assertRefused(corrected)
	// A byte-for-byte signed revision survives transport retention.
	finalID := signed(10, -100)
	if _, err := pool.Exec(ctx, "DELETE FROM outbox WHERE producer_service=$1 AND published_at IS NOT NULL", BatchProducer); err != nil {
		t.Fatal(err)
	}
	verdict, err := r.ReconcileManual(ctx, sub, uuid.NewString(), "operator")
	if err != nil || verdict.StatementID != finalID || verdict.Outcome != OutcomeClean {
		t.Fatalf("signed correction: %+v %v", verdict, err)
	}
}

func TestManualAbsentLevelWaitsForRetainedSignedInput(t *testing.T) {
	const tenant = "manual-pending"
	pool := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	ctx := bus.WithTenantID(t.Context(), tenant)
	st := NewPostgres(pool)
	sub := subject()
	sub.BusinessDate = BusinessDay(t0)
	id := manualSignedFeed(t, ctx, st)(0, 0)
	// Recreate the durable window between append and the independent consumer.
	if _, err := pool.Exec(ctx, "DELETE FROM custody_statements WHERE statement_id=$1", id); err != nil {
		t.Fatal(err)
	}
	r := manualReconciler(t, st, LedgerBookLoader(ledger.NewPostgres(pool), closeScope(t, true), testLogger()), &capturePublisher{})
	key := uuid.NewString()
	if run, err := r.ReconcileManual(ctx, sub, key, "operator"); !errors.Is(err, ErrStatementChanged) || run.RunID != "" {
		t.Fatalf("pending signed intake consumed request: %+v %v", run, err)
	}
	var wire []byte
	if err := pool.QueryRow(ctx, "SELECT payload FROM outbox WHERE producer_service=$1", BatchProducer).Scan(&wire); err != nil {
		t.Fatal(err)
	}
	owner, err := NewSignedBatchConsumer(st, tenant, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Handle(ctx, &envelopepb.Envelope{TenantId: tenant, Source: BatchProducer}, wire); err != nil {
		t.Fatal(err)
	}
	if run, err := r.ReconcileManual(ctx, sub, key, "operator"); err != nil || run.Outcome != OutcomeClean || run.StatementID != id {
		t.Fatalf("delivered signed rerun: %+v %v", run, err)
	}
}

func TestScheduledComparisonWaitsForRetainedSignedInput(t *testing.T) {
	const tenant = "scheduled-pending"
	pool := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	ctx := bus.WithTenantID(t.Context(), tenant)
	st := NewPostgres(pool)
	sub := subject()
	sub.BusinessDate = BusinessDay(t0)
	now := t0.AddDate(0, 0, 3)
	r, err := NewReconciler(st, LedgerBookLoader(ledger.NewPostgres(pool), closeScope(t, true), testLogger()), &capturePublisher{}, nil, nil, testLogger(), func() time.Time {
		now = now.Add(time.Second)
		return now
	})
	if err != nil {
		t.Fatal(err)
	}
	retain := retainedStatementFeedForAccount(t, ctx, st, "ACC1")
	counts := func(want int) {
		t.Helper()
		var runs, completions int
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM custody_runs),
            (SELECT count(*) FROM outbox WHERE subject=$1)`, SubjectRun).Scan(&runs, &completions); err != nil {
			t.Fatal(err)
		}
		if runs != want || completions != want {
			t.Errorf("committed runs/FACTs = %d/%d; want %d/%d", runs, completions, want, want)
		}
	}
	for i, tc := range []struct {
		positions string
		outcome   Outcome
	}{
		{"[]", OutcomeClean},
		{`[{"instrumentId":"AAPL","quantity":{"coefficient":"1"}}]`, OutcomeBreaks},
	} {
		stmt := json.RawMessage(fmt.Sprintf(`{"portfolioId":"PF1","custodianId":"CUST-A","businessDate":"%s","positions":%s,"cash":[],"transactions":[],"grain":"STATEMENT_GRAIN_BALANCES_ONLY"}`, sub.BusinessDate.Format(time.RFC3339), tc.positions))
		id, deliver := retain(stmt)
		var found bool
		if err := pool.QueryRow(ctx, "SELECT EXISTS(SELECT FROM custody_statements WHERE statement_id=$1)", id).Scan(&found); err != nil || found {
			t.Fatalf("revision %d already materialized: %t %v", i+1, found, err)
		}
		run, err := r.Reconcile(ctx, sub)
		if !errors.Is(err, ErrStatementChanged) || run.RunID != "" {
			t.Errorf("pending revision %d returned %s (%s), %v", i+1, run.Outcome, run.RunID, err)
		}
		counts(i)
		pending, err := st.DatedResult(ctx, sub)
		if err != nil || pending.State != "pending" || pending.StatementID != id || pending.Run.RunID != "" {
			t.Fatalf("pending revision %d: %+v %v", i+1, pending, err)
		}
		deliver()
		run, err = r.Reconcile(ctx, sub)
		if err != nil || run.Outcome != tc.outcome || run.StatementID != id || run.RunID == "" {
			t.Fatalf("materialized revision %d: %+v %v", i+1, run, err)
		}
		counts(i + 1)
		recorded, err := st.DatedResult(ctx, sub)
		if err != nil || recorded.State != "recorded" || recorded.Run.RunID != run.RunID || recorded.StatementID != id {
			t.Fatalf("recorded revision %d: %+v %v", i+1, recorded, err)
		}
	}
}

func TestDatedSourceAttestationBindsOnlyVerifiedComparisons(t *testing.T) {
	const tenant = "run-attestation"
	pool := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	ctx := bus.WithTenantID(t.Context(), tenant)
	st := NewPostgres(pool)
	sub := subject()
	sub.BusinessDate = BusinessDay(t0)
	r := manualReconciler(t, st, LedgerBookLoader(ledger.NewPostgres(pool), closeScope(t, true), testLogger()), &capturePublisher{})
	digest := func(id string) []byte {
		t.Helper()
		var got []byte
		if err := pool.QueryRow(ctx, "SELECT signed_statement_sha256 FROM custody_runs WHERE run_id=$1", id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	missing, err := r.ReconcileManual(ctx, sub, uuid.NewString(), "operator")
	if err != nil || missing.Outcome != OutcomeNoStatement || digest(missing.RunID) != nil {
		t.Fatalf("missing: %+v %v", missing, err)
	}
	signed := manualSignedFeed(t, ctx, st)
	id := signed(0, 0)
	var raw []byte
	if err := pool.QueryRow(ctx, "SELECT signed_batch FROM custody_batch_receipts WHERE statement_id=$1", id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	// Metadata from an earlier deployment can name the exact same signed ID.
	// It carries no proof of which source values were actually compared.
	earlier := Run{RunID: "pre-enrollment", Subject: sub, StatementID: id, Outcome: OutcomeClean, CompletedAt: t0}
	if err := st.SaveRun(ctx, earlier); err != nil || digest(earlier.RunID) != nil {
		t.Fatal("pre-existing record was certified", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE custody_runs SET signed_statement_sha256=$1 WHERE run_id=$2", sum[:], earlier.RunID); err == nil {
		t.Fatal("historical attestation can be rewritten")
	}
	scheduled, err := r.Reconcile(t.Context(), sub) // scheduler identity comes from the tenant-scoped DB pool
	if err != nil || scheduled.Outcome != OutcomeClean || !bytes.Equal(digest(scheduled.RunID), sum[:]) {
		t.Fatalf("scheduled: %+v %v", scheduled, err)
	}
	manual, err := r.ReconcileManual(ctx, sub, uuid.NewString(), "operator")
	if err != nil || manual.Outcome != OutcomeClean || !bytes.Equal(digest(manual.RunID), sum[:]) {
		t.Fatalf("manual: %+v %v", manual, err)
	}
	// A normal, independently scheduled legacy comparison remains operational.
	legacy := statement("ordinary-source", nil, nil)
	legacy.BusinessDate = sub.BusinessDate.AddDate(0, 0, 1)
	if err := st.SaveStatement(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	old, err := r.Reconcile(ctx, legacy.Subject())
	if err != nil || old.Outcome != OutcomeClean || digest(old.RunID) != nil {
		t.Fatalf("ordinary: %+v %v", old, err)
	}
}
