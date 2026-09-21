package order

import (
	"context"
	"errors"
	"testing"

	"github.com/eighred/kanz/internal/execution"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
)

func TestRecoveryEvidenceIsDurableAndImmutable(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	s := NewPostgres(pool)
	m := RecoveryMapping{"__system__", "fund-a", "BINANCE", "account-a", "exchange-a"}
	if _, err := s.RecordRecoveryMapping(ctx, m, execution.AccountProof{}); err == nil {
		t.Fatal("unproven account accepted")
	}
	proof := execution.AccountProof{Verified: true, ExchangeAccountID: m.ExchangeAccount}
	version, err := s.RecordRecoveryMapping(ctx, m, proof)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := s.RecordRecoveryMapping(ctx, m, proof); err != nil || again != version {
		t.Fatalf("duplicate: %s %v", again, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE execution_account_mappings SET portfolio_id='fund-b' WHERE version=$1`, version); err == nil {
		t.Fatal("historical mapping was rewritten")
	}
	first, err := s.ObserveRecovery(ctx, "case-a", "order-a", "cursor-a", []byte("original observation"))
	if err != nil {
		t.Fatal(err)
	}
	// A new store instance must recover the exact checkpoint and evidence.
	restarted := NewPostgres(pool)
	again, err := restarted.ObserveRecovery(ctx, "case-a", "order-a", "cursor-a", first.Evidence)
	if err != nil || again.Digest != first.Digest || again.Status != "observed" || again.Checkpoint != 0 {
		t.Fatalf("restart: %+v %v", again, err)
	}
	if _, err := restarted.ObserveRecovery(ctx, "case-a", "order-a", "cursor-a", []byte("changed observation")); !errors.Is(err, ErrRecoveryEvidenceConflict) {
		t.Fatalf("changed evidence: %v", err)
	}
	for _, query := range []string{
		`UPDATE execution_recovery_cases SET evidence='changed', version=version+1 WHERE case_id='case-a'`,
		`UPDATE execution_recovery_cases SET source_cursor='changed', version=version+1 WHERE case_id='case-a'`,
		`UPDATE execution_recovery_cases SET status='investigating' WHERE case_id='case-a'`,
		`DELETE FROM execution_recovery_cases WHERE case_id='case-a'`,
	} {
		if _, err := pool.Exec(ctx, query); err == nil {
			t.Fatalf("immutable evidence or CAS bypass accepted: %s", query)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE execution_recovery_cases SET mapping_version=$1, status='investigating', checkpoint=1, version=version+1 WHERE case_id='case-a'`, version); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE execution_recovery_cases SET checkpoint=0, version=version+1 WHERE case_id='case-a'`); err == nil {
		t.Fatal("checkpoint moved backwards")
	}
	if _, err := pool.Exec(ctx, `UPDATE execution_recovery_cases SET mapping_version=NULL, version=version+1 WHERE case_id='case-a'`); err == nil {
		t.Fatal("mapping proof erased")
	}
	m.Tenant = "another-tenant"
	if _, err := s.RecordRecoveryMapping(ctx, m, proof); err == nil {
		t.Fatal("cross-tenant mapping accepted")
	}
}

func TestExecutionJournalCommitsWithFillAndRollsBackWithConflict(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	s := NewPostgres(pool)
	st := state("journal-order", orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED)
	if err := s.Create(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, st, 99, claimFacts(st.OrderId, "execution-a"), "execution-a"); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM order_executions`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed CAS leaked evidence: %d %v", count, err)
	}
	if err := s.Save(ctx, st, 0, claimFacts(st.OrderId, "execution-a"), "execution-a"); err != nil {
		t.Fatal(err)
	}
	var data []byte
	if err := pool.QueryRow(ctx, `SELECT fill FROM order_executions WHERE order_id=$1`, st.OrderId).Scan(&data); err != nil {
		t.Fatal(err)
	}
	var fill orderpb.Fill
	if err := proto.Unmarshal(data, &fill); err != nil || fill.FillId != "execution-a" || fill.OrderId != st.OrderId {
		t.Fatalf("execution: %v %v", &fill, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM order_executions WHERE order_id=$1`, st.OrderId); err == nil {
		t.Fatal("execution evidence deleted")
	}
}
