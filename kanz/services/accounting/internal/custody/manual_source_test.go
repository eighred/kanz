package custody

import (
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
	tenant := bus.TenantIDFromContext(ctx)
	a, key := exampleAuthority(t, tenant)
	a.Account, a.Instruments = "ACC1", []string{"AAPL"}
	importer, err := NewBatchImporter(a, tenant, []string{"PF1:CUST-A"}, "PF1:CUST-A:ACC1", st.pool, &capturePublisher{}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := NewSignedBatchConsumer(st, tenant, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	revision, previous := int64(0), ""
	return func(qty, cash int) string {
		t.Helper()
		position, currencies := "[]", "[]"
		if qty != 0 {
			position = fmt.Sprintf(`[{"instrumentId":"AAPL","quantity":{"coefficient":"%d"}}]`, qty)
		}
		if cash != 0 {
			currencies = fmt.Sprintf(`[{"currencyCode":"USD","balance":{"coefficient":"%d"}}]`, cash)
		}
		stmt := json.RawMessage(fmt.Sprintf(`{"portfolioId":"PF1","custodianId":"CUST-A","businessDate":"%s","positions":%s,"cash":%s,"transactions":[],"grain":"STATEMENT_GRAIN_BALANCES_ONLY"}`, BusinessDay(t0).Format(time.RFC3339), position, currencies))
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
		if err := consumer.Handle(ctx, &envelopepb.Envelope{TenantId: tenant, Source: BatchProducer}, payload); err != nil {
			t.Fatal(err)
		}
		return id
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
	// Simulate materialized rows inherited from an earlier deployment or restore.
	if _, err := pool.Exec(ctx, "UPDATE custody_statements SET received_at=$1 WHERE statement_id=$2", t0, legacy.StatementID); err != nil {
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
