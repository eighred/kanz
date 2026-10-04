package capital

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestScheduledAllocationUsesOriginalAuthorityAfterOverBudgetExecution(t *testing.T) {
	pool := database(t)
	ctx := context.Background()
	now := snapshot(1, "200").ObservedAt
	if err := Apply(ctx, pool, snapshot(1, "200")); err != nil {
		t.Fatal(err)
	}
	if err := transaction(pool, func(tx pgx.Tx) error { return Reserve(ctx, tx, "fund", "USD", "parent", "100", now, time.Minute) }); err != nil {
		t.Fatal(err)
	}
	if err := transaction(pool, func(tx pgx.Tx) error { return ObserveExecution(ctx, tx, "fund", "USD", "parent", "120") }); err != nil {
		t.Fatal(err)
	}
	if err := transaction(pool, func(tx pgx.Tx) error {
		return AllocateChild(ctx, tx, "fund", "USD", "parent", "child", "110", now, time.Minute)
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("over-budget execution widened child authority: %v", err)
	}
	var required, admission, allocated string
	if err := pool.QueryRow(ctx, `SELECT required_debit,admission_debit,allocated_debit FROM capital_commitments WHERE order_id='parent'`).Scan(&required, &admission, &allocated); err != nil {
		t.Fatal(err)
	}
	if required != "120" || admission != "100" || allocated != "0" {
		t.Fatalf("bounds changed: required=%s admission=%s allocated=%s", required, admission, allocated)
	}
}
