package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/services/datamaster/internal/pricing"
)

// Retry and cross-replica deduplication are database properties: a memory queue
// cannot establish that repeated detection preserves the original evidence.
func TestPostgresConcurrentPriceDetectionPreservesEvidence(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	var privileged bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&privileged); err != nil {
		t.Fatal(err)
	}
	if privileged {
		t.Fatal("price detection verification requires a role without superuser or BYPASSRLS")
	}
	es := NewPostgresExceptions(pool, "__system__")
	original := pricing.Arbitrate("X", nil, nil, 0, pgNow).Exceptions
	if err := es.AddAll(ctx, original); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			otherReplica := NewPostgresExceptions(pool, "__system__")
			retry := pricing.Arbitrate("X", nil, nil, 0, pgNow.Add(time.Hour)).Exceptions
			if err := otherReplica.AddAll(ctx, retry); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	open, err := es.Open(ctx)
	if err != nil || len(open) != 1 {
		t.Fatalf("duplicate detection: %v %v", open, err)
	}
	if open[0].ID != original[0].ID || !open[0].DetectedAt.Equal(pgNow) || open[0].Detail != original[0].Detail {
		t.Fatalf("retry replaced evidence: %+v", open[0])
	}
	// RLS must hide the same deterministic identifier from another tenant.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', 'other-test-tenant', true)`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM exceptions WHERE exception_id = $1`, original[0].ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("price exception leaked across tenant boundary")
	}
}
