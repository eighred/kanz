package projection

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Restore the captured raw fact/checkpoint bytes into a freshly created schema;
// the verifier must read this SQL state, not the producer's in-memory projection.
func restoredReplayFixture(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newPool(t, testTenant)
	freshSchema(t, pool)
	log, snap := replayFixture(t)
	restoreReplayInputs(t, pool, log, snap)
	return pool
}

func restoreReplayInputs(t *testing.T, pool *pgxpool.Pool, log *memLog, snap replaySnapshot) {
	t.Helper()
	ctx := context.Background()
	for _, f := range log.facts {
		if _, err := pool.Exec(ctx, `INSERT INTO tv_facts(event_id,event_type,payload,knowledge_at,seq) VALUES($1,$2,$3,$4,$5)`, f.EventID, f.EventType, f.Payload, f.Knowledge, f.Seq); err != nil {
			t.Fatal("restore fact failed")
		}
	}
	if err := NewPostgresLog(pool).SaveCheckpoint(ctx, snap.checkpointSeq, snap.checkpoint); err != nil {
		t.Fatal("restore checkpoint failed")
	}
}

func TestReplayVerifierPostgresRefusesRoundedCheckpoint(t *testing.T) {
	pool := newPool(t, testTenant)
	freshSchema(t, pool)
	// Keep the tiny execution resident. Evicting it into exact baseline strings
	// would accidentally hide the serialization-only verifier's false positive.
	log, snap := roundedReplayFixtureAt(t, time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond))
	restoreReplayInputs(t, pool, log, snap)
	if _, err := VerifyPostgresSnapshot(context.Background(), pool, testTenant, 7*24*time.Hour); !errors.Is(err, ErrReplayMismatch) {
		t.Fatal("rounded restored checkpoint qualified")
	}
}

func TestReplayVerifierPostgresRestoredSnapshot(t *testing.T) {
	pool := restoredReplayFixture(t)
	ctx := context.Background()
	proof, err := VerifyPostgresSnapshot(ctx, pool, testTenant, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if proof.FactCount != 3 || proof.CheckpointSeq != 2 || proof.Watermark != 3 {
		t.Fatal("snapshot binding mismatch")
	}
	again, err := VerifyPostgresSnapshot(ctx, pool, testTenant, 7*24*time.Hour)
	if err != nil || again.InputSHA256 != proof.InputSHA256 {
		t.Fatal("verification mutated restored inputs")
	}
	other := newPool(t, "different-tenant")
	if _, err := VerifyPostgresSnapshot(ctx, other, "different-tenant", 7*24*time.Hour); !errors.Is(err, ErrReplayNoHistory) {
		t.Fatal("other tenant discovered restored history")
	}
	if _, err := VerifyPostgresSnapshot(ctx, pool, "different-tenant", 7*24*time.Hour); !errors.Is(err, ErrReplayInput) {
		t.Fatal("tenant GUC mismatch accepted")
	}
}

func TestReplayVerifierPostgresSnapshotIsolationAndReadOnly(t *testing.T) {
	pool := restoredReplayFixture(t)
	ctx := context.Background()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal("begin failed")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	snap, err := inspectReplaySnapshot(ctx, tx, testTenant)
	if err != nil {
		t.Fatal(err)
	}
	// Another connection adds a new fact after the verifier's snapshot exists.
	if _, err := pool.Exec(ctx, `INSERT INTO tv_facts(event_id,event_type,payload,knowledge_at,seq) SELECT 'concurrent',event_type,payload,knowledge_at,4 FROM tv_facts WHERE seq=3`); err != nil {
		t.Fatal("concurrent append failed")
	}
	proof, err := verifyCapturedReplay(ctx, &PostgresLog{db: tx}, testTenant, 7*24*time.Hour, snap)
	if err != nil || proof.FactCount != 3 || proof.Watermark != 3 {
		t.Fatal("snapshot changed underneath replay")
	}
	_, err = tx.Exec(ctx, `DELETE FROM tv_facts`)
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "25006" {
		t.Fatal("transaction did not enforce read-only")
	}
}

func TestReplayVerifierPostgresRefusesEmptyCorruptAndOversized(t *testing.T) {
	pool := restoredReplayFixture(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE tv_checkpoints SET payload=$1`, []byte{0xff}); err != nil {
		t.Fatal("corrupt fixture failed")
	}
	if _, err := VerifyPostgresSnapshot(ctx, pool, testTenant, 7*24*time.Hour); !errors.Is(err, ErrReplayInput) {
		t.Fatal("corrupt checkpoint qualified")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tv_facts(event_id,event_type,payload,knowledge_at,seq) VALUES('oversize','order.order.filled',decode(repeat('00',65537),'hex'),now(),40)`); err != nil {
		t.Fatal("oversized fixture failed")
	}
	if _, err := VerifyPostgresSnapshot(ctx, pool, testTenant, 7*24*time.Hour); !errors.Is(err, ErrReplayLimit) {
		t.Fatal("oversized fact escaped SQL preflight")
	}
	freshSchema(t, pool)
	if _, err := VerifyPostgresSnapshot(ctx, pool, testTenant, 7*24*time.Hour); !errors.Is(err, ErrReplayNoHistory) {
		t.Fatal("empty genesis qualified as restored")
	}
}

func TestReplayVerifierPostgresBoundsTextBeforeReplay(t *testing.T) {
	ctx := context.Background()
	// Invalid payload would produce ErrReplayInput if transferred and decoded;
	// ErrReplayLimit must instead come from the SQL length-only preflight.
	for _, column := range []string{"event_id", "event_type"} {
		t.Run(column, func(t *testing.T) {
			pool := restoredReplayFixture(t)
			id, subject := "bad-text", "order.order.filled"
			if column == "event_id" {
				id = strings.Repeat("x", 1025)
			} else {
				subject = strings.Repeat("x", 1025)
			}
			if _, err := pool.Exec(ctx, "INSERT INTO tv_facts(event_id,event_type,payload,knowledge_at,seq) VALUES($1,$2,decode('ff','hex'),now(),40)", id, subject); err != nil {
				t.Fatal("text fixture failed", err)
			}
			if _, err := VerifyPostgresSnapshot(ctx, pool, testTenant, 7*24*time.Hour); !errors.Is(err, ErrReplayLimit) {
				t.Fatal("text escaped SQL bound")
			}

		})
	}
}

func TestReplayVerifierPostgresRefusesPermissiveRLS(t *testing.T) {
	pool := restoredReplayFixture(t)
	ctx := context.Background()
	other := newPool(t, "different-tenant")
	log, snap := replayFixture(t)
	f := log.facts[0]
	if _, err := other.Exec(ctx, `INSERT INTO tv_facts(event_id,event_type,payload,knowledge_at,seq) VALUES('foreign',$1,$2,$3,100)`, f.EventType, f.Payload, f.Knowledge); err != nil {
		t.Fatal("foreign fixture failed")
	}
	// Misrestored policy retains ENABLE/FORCE RLS while allowing all tenants.
	if _, err := pool.Exec(ctx, `ALTER POLICY tenant_isolation ON tv_facts USING(true) WITH CHECK(true)`); err != nil {
		t.Fatal("policy fixture failed")
	}
	if _, err := VerifyPostgresSnapshot(ctx, pool, testTenant, 7*24*time.Hour); !errors.Is(err, ErrReplayInput) {
		t.Fatal("foreign facts qualified under permissive RLS")
	}
	freshSchema(t, pool)
	restoreReplayInputs(t, pool, log, snap)
	if err := NewPostgresLog(other).SaveCheckpoint(ctx, snap.checkpointSeq, snap.checkpoint); err != nil {
		t.Fatal("foreign checkpoint fixture failed")
	}
	if _, err := pool.Exec(ctx, `ALTER POLICY tenant_isolation ON tv_checkpoints USING(true) WITH CHECK(true)`); err != nil {
		t.Fatal("checkpoint policy fixture failed")
	}
	if _, err := VerifyPostgresSnapshot(ctx, pool, testTenant, 7*24*time.Hour); !errors.Is(err, ErrReplayInput) {
		t.Fatal("foreign checkpoint qualified under permissive RLS")
	}
}
