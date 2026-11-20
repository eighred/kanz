package custody

import (
	"crypto/sha256"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/google/uuid"
)

func TestPostgresHistoricalReceiptAfterCorrection(t *testing.T) {
	const tenant = "history-tenant"
	pool := newCustodyPool(t, tenant)
	requireRestrictedCustodyRole(t, pool)
	applyCustodySchema(t, pool)
	ctx := bus.WithTenantID(t.Context(), tenant)
	st := NewPostgres(pool)
	journal := ledger.NewPostgres(pool)
	if err := journal.Append(ctx, closeTrade("original", 10, -100, t0), nil); err != nil {
		t.Fatal(err)
	}
	sub := subject()
	sub.BusinessDate = BusinessDay(t0)
	now := t0.AddDate(0, 0, 2).Add(123 * time.Nanosecond)
	r, err := NewReconciler(st, LedgerBookLoader(journal, closeScope(t, true), testLogger()), &capturePublisher{}, nil, nil, testLogger(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	feed := manualSignedFeed(t, ctx, st)
	source := feed(8, -100)
	opened, err := r.Reconcile(ctx, sub)
	if err != nil {
		t.Fatal(err)
	}
	corrected := feed(10, -100)
	now = now.Add(time.Hour)
	closed, err := r.ReconcileManual(ctx, sub, uuid.NewString(), "operator")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM outbox WHERE tenant_id=app_current_tenant()"); err != nil {
		t.Fatal(err)
	}
	current, err := st.OutstandingBreaks(ctx)
	if err != nil || len(current) != 0 {
		t.Fatalf("post-correction queue %v %v", current, err)
	}
	original, err := st.HistoricalReceipt(ctx, sub, "ACC1", opened.RunID)
	if err != nil || original.State != "recorded" || original.StatementID != source || original.Run.Outcome != OutcomeBreaks || len(original.Run.Breaks) != 1 {
		t.Fatalf("old completion %+v %v", original, err)
	}
	var raw []byte
	if err := pool.QueryRow(ctx, "SELECT signed_batch FROM custody_batch_receipts WHERE statement_id=$1", source).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	b := original.Run.Breaks[0]
	if original.SignedStatementSHA256 != fmt.Sprintf("%x", sha256.Sum256(raw)) || b.Key != "AAPL" ||
		b.IBOR.Cmp(big.NewRat(10, 1)) != 0 || b.Custodian.Cmp(big.NewRat(8, 1)) != 0 || b.Diff.Cmp(big.NewRat(2, 1)) != 0 ||
		original.Run.Basis.Transactions == nil || original.Run.Basis.Transactions.Pass != "balances_only" {
		t.Fatalf("incorrect original evidence %+v %+v", original, b)
	}
	clean, err := st.HistoricalReceipt(ctx, sub, "ACC1", closed.RunID)
	if err != nil || clean.State != "recorded" || clean.StatementID != corrected || clean.Run.Outcome != OutcomeClean || len(clean.Run.Breaks) != 0 || clean.SignedStatementSHA256 == original.SignedStatementSHA256 {
		t.Fatalf("corrected completion %+v %v", clean, err)
	}
	assertUnknown := func(st *Postgres, tenant string, subject Subject, account, id string) {
		t.Helper()
		got, err := st.HistoricalReceipt(bus.WithTenantID(t.Context(), tenant), subject, account, id)
		if err != nil || got.State != "unknown" || got.Run.RunID != "" || got.StatementID != "" || got.SignedStatementSHA256 != "" || len(got.Run.Breaks) != 0 {
			t.Fatalf("unexpected historical disclosure %+v %v", got, err)
		}
	}
	for _, mismatch := range []Subject{
		{PortfolioID: "PF2", CustodianID: sub.CustodianID, BusinessDate: sub.BusinessDate},
		{PortfolioID: sub.PortfolioID, CustodianID: "OTHER", BusinessDate: sub.BusinessDate},
		{PortfolioID: sub.PortfolioID, CustodianID: sub.CustodianID, BusinessDate: sub.BusinessDate.AddDate(0, 0, -1)},
	} {
		assertUnknown(st, tenant, mismatch, "ACC1", opened.RunID)
	}
	assertUnknown(st, tenant, sub, "OTHER", opened.RunID)
	assertUnknown(st, tenant, sub, "ACC1", "absent")
	assertUnknown(NewPostgres(newCustodyPool(t, "foreign-history")), "foreign-history", sub, "ACC1", opened.RunID)
	assertUnknown(st, "foreign-history", sub, "ACC1", opened.RunID)
	// Historical absence remains unknown even while a materialized source exists.
	legacy := opened
	legacy.RunID = "legacy"
	if err := st.SaveRun(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	assertUnknown(st, tenant, sub, "ACC1", legacy.RunID)
	// Exercise old/restored records written before the read-time checks. Each
	// variant gets an independent row. The valid signed source stays immutable.
	digest := sha256.Sum256(raw)
	for _, name := range []string{"legacy_missing_payload", "wrong_signed_digest", "wrong_statement", "wrong_decimal", "wrong_break_identity", "wrong_time", "wrong_date", "out_of_bounds", "unmeasured_coverage"} {
		t.Run(name, func(t *testing.T) {
			row := cloneRun(opened)
			row.RequestID, row.Actor = uuid.NewString(), "operator"
			row.RunID = "manual:" + row.RequestID
			wire := cloneRun(row)
			binding := digest
			switch name {
			case "wrong_signed_digest":
				binding[0] ^= 1
			case "wrong_statement":
				wire.StatementID = corrected
			case "wrong_decimal":
				// Internally consistent subtraction still disagrees with signed r1.
				wire.Breaks[0].Custodian, wire.Breaks[0].Diff = big.NewRat(7, 1), big.NewRat(3, 1)
			case "wrong_break_identity":
				wire.Breaks[0].BreakID = "other"
			case "wrong_time":
				wire.CompletedAt = wire.CompletedAt.Add(time.Minute)
			case "wrong_date":
				wire.Subject.BusinessDate = wire.Subject.BusinessDate.AddDate(0, 0, -1)
			case "unmeasured_coverage":
				row.Basis.Transactions = nil
			}
			record, err := runRecord(ctx, tenant, wire)
			if err != nil {
				t.Fatal(err)
			}
			payload := record.Payload
			switch name {
			case "legacy_missing_payload":
				payload = nil
			case "out_of_bounds":
				payload = make([]byte, BatchLimit+1)
			}
			if err := st.saveRun(ctx, row, binding[:], payload); err != nil {
				t.Fatal(err)
			}
			if name == "unmeasured_coverage" {
				got, err := st.HistoricalReceipt(ctx, sub, "ACC1", row.RunID)
				if err != nil || got.State != "recorded" || got.Run.Basis == nil || got.Run.Basis.Transactions != nil {
					t.Fatalf("legacy financial evidence acquired transaction coverage: %+v %v", got, err)
				}
				return
			}
			assertUnknown(st, tenant, sub, "ACC1", row.RunID)
		})
	}
}
