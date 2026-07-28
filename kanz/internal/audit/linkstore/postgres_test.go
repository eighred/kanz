package linkstore

// Postgres audit-link-store integration tests (REG-02). Gated on
// TEST_POSTGRES_URL — they require a real database and skip otherwise,
// mirroring the risk-engine persist and accounting ledger Postgres tests. The
// DB-free chain/idempotency properties are proven in linkstore_test.go; these
// prove the durable contract beneath them: the chain survives a restart (head
// recovery), re-append is idempotent, and the persisted links verify.

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eighred/kanz/internal/audit/signer"
)

const migrationDir = "../../../services/regulatory/migrations"

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL to run linkstore Postgres integration tests")
	}
	pool, err := pgxpool.New(context.Background(), url)
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
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS audit_chain_links CASCADE`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	files, err := filepath.Glob(filepath.Join(migrationDir, "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob migrations %s: %v (found %d)", migrationDir, err, len(files))
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

func TestPostgres_RestartContinuity(t *testing.T) {
	pool := newPool(t)
	driveRestartContinuity(t, NewPostgres(pool))
}

func TestPostgres_AppendIsIdempotent(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	store := NewPostgres(pool)
	link := signer.Link{Prev: "genesis", Cur: "dup-hash", Body: []byte("body")}
	if err := store.Append(ctx, link); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := store.Append(ctx, link); err != nil {
		t.Fatalf("Append (dup): %v", err)
	}
	links, err := store.Links(ctx)
	if err != nil {
		t.Fatalf("Links: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("idempotent append persisted %d rows, want 1", len(links))
	}
	if links[0].Body == nil || string(links[0].Body) != "body" {
		t.Fatalf("canonical bytes not round-tripped: %q", links[0].Body)
	}
}
