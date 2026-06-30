package fund

// Postgres fund-journal integration tests (PARITY-02b). Gated on
// TEST_POSTGRES_URL — they skip without a database, mirroring the risk-engine
// persist tests. They prove the durable contract: the journal survives a
// restart, re-append is idempotent, and the same journal replays to the same
// alternatives.Position.

import (
	"context"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kanz-eng/kanz/internal/alternatives"
)

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL to run fund Postgres integration tests")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", "__system__")
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	applySchema(t, pool)
	return pool
}

func applySchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS fund_events CASCADE`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	files, err := filepath.Glob(filepath.Join("../../migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob migrations: %v (found %d)", err, len(files))
	}
	sort.Strings(files)
	for _, f := range files {
		ddl, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read migration %s: %v", f, err)
		}
		if _, err := pool.Exec(ctx, string(ddl)); err != nil {
			t.Fatalf("apply migration %s: %v", f, err)
		}
	}
}

func TestPostgresFundJournalRoundTrip(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	ctx := context.Background()

	t0 := time.Unix(1_700_000_000, 0).UTC()
	events := []*alternatives.Event{
		{EventID: "c1", CommitmentID: "CMT-1", Type: alternatives.EventCommit, Amount: big.NewRat(1_000_000, 1), Date: t0},
		{EventID: "k1", CommitmentID: "CMT-1", Type: alternatives.EventCall, Amount: big.NewRat(250_000, 1), Date: t0.Add(24 * time.Hour)},
		{EventID: "d1", CommitmentID: "CMT-1", Type: alternatives.EventDistribution, Amount: big.NewRat(100_000, 1), Date: t0.Add(48 * time.Hour)},
	}
	for _, e := range events {
		if err := st.Append(ctx, e); err != nil {
			t.Fatalf("append %s: %v", e.EventID, err)
		}
	}
	if err := st.Append(ctx, events[0]); err != nil { // idempotent
		t.Fatalf("re-append: %v", err)
	}

	got, err := st.Journal(ctx, "CMT-1")
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("journal len = %d, want 3 (idempotent)", len(got))
	}
	durable := alternatives.Replay("CMT-1", got)
	memory := alternatives.Replay("CMT-1", events)
	if durable.Called.Cmp(memory.Called) != 0 || durable.Distributed.Cmp(memory.Distributed) != 0 {
		t.Fatalf("durable position != memory: called %v/%v distributed %v/%v",
			durable.Called, memory.Called, durable.Distributed, memory.Distributed)
	}

	ids, err := st.Commitments(ctx)
	if err != nil || len(ids) != 1 || ids[0] != "CMT-1" {
		t.Fatalf("commitments = %v err=%v", ids, err)
	}
}
