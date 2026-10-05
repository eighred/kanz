package outbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func requireRestrictedRole(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var super, bypass bool
	if err := pool.QueryRow(context.Background(), `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass); err != nil {
		t.Fatal(err)
	}
	if super || bypass {
		t.Fatal("outbox ownership proof requires NOSUPERUSER NOBYPASSRLS")
	}
}

func enqueueOwned(t *testing.T, pool *pgxpool.Pool, service string, records ...Record) {
	t.Helper()
	ctx := testCtx()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := Enqueue(ctx, tx, service, records...); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestEnqueueRequiresExplicitService(t *testing.T) {
	for _, service := range []string{"", " ", " oms", "oms "} {
		if err := Enqueue(context.Background(), nil, service); !errors.Is(err, ErrInvalidService) {
			t.Fatalf("service %q: %v", service, err)
		}
	}
}

func TestPostgresOutboxScopesEveryOperationToService(t *testing.T) {
	pool := newPool(t, testTenant)
	requireRestrictedRole(t, pool)
	applySchema(t)
	ctx := testCtx()
	enqueueOwned(t, pool, ServiceOMS, fact(t, ctx, "order.order.cancelled", "shared"))
	enqueueOwned(t, pool, ServiceAccounting,
		fact(t, ctx, "order.order.cancelled", "shared"),
		fact(t, ctx, "order.order.cancelled", "accounting-only"))
	oms, accounting := NewPostgres(pool, ServiceOMS), NewPostgres(pool, ServiceAccounting)
	// The old Pending predicate selected both producers under the same tenant
	// and key. Reproduce that selection against this real shared-table fixture.
	var unscopedCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox
		WHERE published_at IS NULL AND partition_key = $1`, "shared").Scan(&unscopedCount); err != nil || unscopedCount != 2 {
		t.Fatalf("pre-fix selection count=%d err=%v", unscopedCount, err)
	}
	omsRows, err := oms.Pending(ctx, "shared", 10)
	if err != nil || len(omsRows) != 1 {
		t.Fatalf("OMS pending=%v err=%v", omsRows, err)
	}
	accountingRows, err := accounting.Pending(ctx, "shared", 10)
	if err != nil || len(accountingRows) != 1 || accountingRows[0].ID == omsRows[0].ID {
		t.Fatalf("accounting pending=%v err=%v", accountingRows, err)
	}
	keys, err := oms.PendingKeys(ctx, 10)
	if err != nil || len(keys) != 1 || keys[0] != "shared" {
		t.Fatalf("OMS keys=%v err=%v", keys, err)
	}
	if err := accounting.MarkPublished(ctx, omsRows[0].ID); !errors.Is(err, ErrRecordNotOwned) {
		t.Fatalf("wrong-service publish mark: %v", err)
	}
	if err := accounting.MarkFailed(ctx, omsRows[0].ID, errors.New("wrong relay")); !errors.Is(err, ErrRecordNotOwned) {
		t.Fatalf("wrong-service failure mark: %v", err)
	}
	omsRows, err = oms.Pending(ctx, "shared", 10)
	if err != nil || len(omsRows) != 1 || omsRows[0].Attempts != 0 || omsRows[0].LastError != "" {
		t.Fatalf("foreign relay modified OMS row: %v %v", omsRows, err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `UPDATE outbox SET enqueued_at = $1 WHERE producer_service = $2`, now.Add(-time.Minute), ServiceOMS); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE outbox SET enqueued_at = $1 WHERE producer_service = $2`, now.Add(-time.Hour), ServiceAccounting); err != nil {
		t.Fatal(err)
	}
	if age, ok, err := oms.OldestPendingAge(ctx, now); err != nil || !ok || age != time.Minute {
		t.Fatalf("OMS age=%v ok=%v err=%v", age, ok, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE outbox SET producer_service = $1 WHERE id = $2`, ServiceAccounting, omsRows[0].ID); err == nil {
		t.Fatal("known ownership was reassigned")
	}
	if err := oms.MarkPublished(ctx, omsRows[0].ID); err != nil {
		t.Fatal(err)
	}
	// A retry after successful publication remains harmless for its owner.
	if err := oms.MarkPublished(ctx, omsRows[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := oms.MarkFailed(ctx, omsRows[0].ID, errors.New("late failure")); err != nil {
		t.Fatal(err)
	}
	if rows, err := accounting.Pending(ctx, "shared", 10); err != nil || len(rows) != 1 {
		t.Fatalf("OMS mark affected accounting: %v %v", rows, err)
	}
	other := newPool(t, "other-tenant")
	if rows, err := NewPostgres(other, ServiceOMS).Pending(ctx, "shared", 10); err != nil || len(rows) != 0 {
		t.Fatalf("cross-tenant rows=%v err=%v", rows, err)
	}
}

func TestPostgresOutboxPhysicalTablesDoNotShareRowsOrDrainLocks(t *testing.T) {
	pool := newPool(t, testTenant)
	requireRestrictedRole(t, pool)
	applySchema(t)
	const otherSchema = "oms_outbox_other_table_test"
	applySchemaIn(t, otherSchema, "")
	other := newPoolForSchema(t, testTenant, otherSchema)
	t.Cleanup(func() {
		if _, err := other.Exec(context.Background(), `DROP SCHEMA oms_outbox_other_table_test CASCADE`); err != nil {
			t.Errorf("clean second test schema: %v", err)
		}
	})
	ctx := testCtx()
	firstRecord := fact(t, ctx, "order.order.cancelled", "same-key")
	firstRecord.CorrelationID = "first-table"
	secondRecord := firstRecord
	secondRecord.CorrelationID = "second-table"
	enqueueOwned(t, pool, ServiceOMS, firstRecord)
	enqueueOwned(t, other, ServiceOMS, secondRecord)
	first, second := NewPostgres(pool, ServiceOMS), NewPostgres(other, ServiceOMS)
	releaseFirst, won, err := first.LockKey(ctx, "same-key", false)
	if err != nil || !won {
		t.Fatalf("first lock: won=%v err=%v", won, err)
	}
	defer releaseFirst()
	releaseSecond, won, err := second.LockKey(ctx, "same-key", false)
	if err != nil || !won {
		t.Fatalf("distinct physical table shared a drain lock: won=%v err=%v", won, err)
	}
	defer releaseSecond()
	for _, tc := range []struct {
		q    *Postgres
		want string
	}{{first, "first-table"}, {second, "second-table"}} {
		rows, err := tc.q.Pending(ctx, "same-key", 10)
		if err != nil || len(rows) != 1 || rows[0].Record.CorrelationID != tc.want {
			t.Fatalf("physical table %s rows=%v err=%v", tc.want, rows, err)
		}
	}
}

const ownershipMigration = "0025_outbox_service_owner.sql"

func TestPostgresOutboxLegacyOwnershipBlocksStateCommitAndDrain(t *testing.T) {
	pool := newPool(t, testTenant)
	requireRestrictedRole(t, pool)
	applySchemaBefore(t, ownershipMigration)
	ctx := testCtx()
	// This is the pre-migration schema and insert shape, not a disabled new
	// constraint. The fixture independently knows who created this legacy FACT.
	legacyInsert := `INSERT INTO outbox (envelope_tenant_id, partition_key, subject, event_type,
		event_class, schema_version, domain, payload_schema_ref, event_time, payload, published_at)
		VALUES ($1, 'legacy', 'order.order.cancelled', 'order.order.cancelled', $4, 1,
		'order', 'order.v1.OrderCancelled:1', now(), $2, $3) RETURNING id`
	var legacyID int64
	record := fact(t, ctx, "order.order.cancelled", "legacy")
	if err := pool.QueryRow(ctx, legacyInsert, testTenant, record.Payload, nil, int32(record.EventClass)).Scan(&legacyID); err != nil {
		t.Fatal(err)
	}
	var publishedID int64
	if err := pool.QueryRow(ctx, legacyInsert, testTenant, record.Payload, time.Now(), int32(record.EventClass)).Scan(&publishedID); err != nil {
		t.Fatal(err)
	}
	ddl, err := os.ReadFile(filepath.Join(omsMigrations, ownershipMigration))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(ddl)); err != nil {
		t.Fatal(err)
	}
	q := NewPostgres(pool, ServiceOMS)
	if _, err := q.PendingKeys(ctx, 10); !errors.Is(err, ErrUnownedPending) {
		t.Fatalf("legacy keys: %v", err)
	}
	if _, err := q.Pending(ctx, "legacy", 10); !errors.Is(err, ErrUnownedPending) {
		t.Fatalf("legacy read: %v", err)
	}
	if _, _, err := q.OldestPendingAge(ctx, time.Now()); !errors.Is(err, ErrUnownedPending) {
		t.Fatalf("legacy age: %v", err)
	}
	if _, err := NewPostgres(pool, ServiceAccounting).PendingKeys(ctx, 10); !errors.Is(err, ErrUnownedPending) {
		t.Fatalf("restart/other-service guessed legacy owner: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE state_commit_probe (id integer PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO state_commit_probe VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	if err := Enqueue(ctx, tx, ServiceOMS, record); !errors.Is(err, ErrUnownedPending) {
		t.Fatalf("enqueue past unknown legacy: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM state_commit_probe`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("state survived refusal: count=%d err=%v", count, err)
	}
	if _, err := pool.Exec(ctx, legacyInsert, testTenant, record.Payload, nil, int32(record.EventClass)); err == nil {
		t.Fatal("old writer added another unowned row after migration")
	}
	// Explicit fixture evidence authorizes only this pending row. Published
	// history remains NULL and is neither guessed nor required for new drains.
	if _, err := pool.Exec(ctx, `UPDATE outbox SET producer_service = $1 WHERE id = $2`, ServiceOMS, legacyID); err != nil {
		t.Fatal(err)
	}
	rows, err := q.Pending(ctx, "legacy", 10)
	if err != nil || len(rows) != 1 || rows[0].ID != legacyID {
		t.Fatalf("reconciled pending=%v err=%v", rows, err)
	}
	enqueueOwned(t, pool, ServiceOMS, record)
	var historicalOwner *string
	if err := pool.QueryRow(ctx, `SELECT producer_service FROM outbox WHERE id = $1`, publishedID).Scan(&historicalOwner); err != nil || historicalOwner != nil {
		t.Fatalf("published history was silently classified: %v %v", historicalOwner, err)
	}
}
