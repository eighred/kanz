package custody

import (
	"context"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/services/accounting/internal/recon"
)

func TestCustodyPrecisionMigrationDoesNotCertifyRoundedHistory(t *testing.T) {
	pool := newCustodyPool(t, "__system__")
	applyCustodySchema(t, pool, "0011_custody_precision.sql")
	ctx := context.Background()
	id := BreakID("PF1", "CUST-A", recon.BreakQuantity, "AAPL")
	if _, err := pool.Exec(ctx, `INSERT INTO custody_breaks (break_id,portfolio_id,custodian_id,kind,break_key,ibor,custodian,difference,status,first_seen_at,last_seen_at,status_changed_at) VALUES ($1,'PF1','CUST-A','quantity','AAPL','1','1','0','open',now(),now(),now())`, id); err != nil {
		t.Fatal(err)
	}
	ddl, err := os.ReadFile(filepath.Join(custodyMigrationDir, "0011_custody_precision.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(ddl)); err != nil {
		t.Fatal(err)
	}
	st := NewPostgres(pool)
	b, err := st.LoadBreak(ctx, id)
	if err != nil || b.ValuesVerified || b.IBOR != nil || b.Diff != nil {
		t.Fatalf("migration certified rounded history: %+v %v", b, err)
	}
	if _, err := st.ApplyAction(ctx, Action{Tenant: "__system__", Actor: "test-operator", RequestID: "legacy", BreakID: id, Kind: "claim", ExpectedRevision: 1}); !errors.Is(err, ErrUnverifiedPrecision) {
		t.Fatalf("legacy zero accepted for review: %v", err)
	}
}

func TestPostgresCustodyPrecisionSurvivesRestartAndOldWriter(t *testing.T) {
	st, ctx := newCustodyStore(t)
	exact := dec.Rat("123456789.000000000000000001")
	s := Statement{StatementID: "precision", CustodianID: "CUST-A", PortfolioID: "PF1", BusinessDate: BusinessDay(t0), ReceivedAt: t0, Positions: map[string]*big.Rat{"AAPL": exact}, Cash: map[string]*big.Rat{"USD": exact}, Grain: recon.GrainTransactions, Transactions: []recon.Transaction{{ExternalRef: "trade-1", Quantity: exact, Price: exact, Cash: exact}}}
	if err := st.SaveStatement(ctx, s); err != nil {
		t.Fatal(err)
	}
	got, err := NewPostgres(st.pool).LatestStatement(ctx, subject())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []*big.Rat{got.Positions["AAPL"], got.Cash["USD"], got.Transactions[0].Quantity, got.Transactions[0].Price, got.Transactions[0].Cash} {
		if v.Cmp(exact) != 0 {
			t.Fatalf("rounded stored value: %v", v)
		}
	}
	// Simulates a pre-upgrade writer's normal UPDATE. It cannot retain the
	// verification established by a new process, even on a reused connection.
	if _, err := st.pool.Exec(ctx, `UPDATE custody_statements SET positions='{"AAPL":"123456789"}' WHERE statement_id='precision'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LatestStatement(ctx, subject()); !errors.Is(err, ErrUnverifiedPrecision) {
		t.Fatalf("old writer trusted: %v", err)
	}
	if err := st.SaveStatement(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LatestStatement(ctx, subject()); err != nil {
		t.Fatalf("source replay did not recover: %v", err)
	}

	detected := FromRecon(subject(), []recon.Break{{Kind: recon.BreakQuantity, Key: "AAPL", IBOR: exact, Custodian: big.NewRat(1, 1), Diff: new(big.Rat).Sub(exact, big.NewRat(1, 1))}}, t0)
	if _, err := st.UpsertBreaks(ctx, subject(), detected, t0); err != nil {
		t.Fatal(err)
	}
	id := detected[0].BreakID
	b, err := st.LoadBreak(ctx, id)
	if err != nil || !b.ValuesVerified || b.IBOR.Cmp(exact) != 0 {
		t.Fatalf("break precision: %+v %v", b, err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE custody_breaks SET ibor='123456789' WHERE break_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	b, err = st.LoadBreak(ctx, id)
	if err != nil || b.ValuesVerified || b.IBOR != nil {
		t.Fatalf("legacy values presented: %+v %v", b, err)
	}
	a := Action{Tenant: "__system__", Actor: "test-operator", RequestID: "legacy-review", Kind: "claim", BreakID: id, ExpectedRevision: b.Revision}
	if _, err := st.ApplyAction(ctx, a); !errors.Is(err, ErrUnverifiedPrecision) {
		t.Fatalf("legacy action accepted: %v", err)
	}
	if _, err := st.UpsertBreaks(ctx, subject(), detected, t0); err != nil {
		t.Fatal(err)
	}
	b, err = st.LoadBreak(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	a.ExpectedRevision = b.Revision
	if _, err := st.ApplyAction(ctx, a); err != nil {
		t.Fatalf("verified reconciliation did not recover actions: %v", err)
	}
}

func TestPostgresCustodyMalformedValuesRefuseWithoutPanicking(t *testing.T) {
	st, ctx := newCustodyStore(t)
	if _, err := st.UpsertBreaks(ctx, subject(), detectedQuantityBreak(t0, 100, 90), t0); err != nil {
		t.Fatal(err)
	}
	tx, err := st.exactTransaction(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE custody_breaks SET ibor='1e999999999'`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.OutstandingBreaks(ctx); !errors.Is(err, ErrUnverifiedPrecision) {
		t.Fatalf("malformed verified storage: %v", err)
	}
}
