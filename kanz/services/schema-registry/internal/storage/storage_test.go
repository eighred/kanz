package storage_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eighred/kanz/services/schema-registry/internal/storage"
)

func TestParseRef(t *testing.T) {
	tests := []struct {
		in      string
		want    storage.Ref
		wantErr bool
	}{
		{"market.v1.MarketDataEvent:7", storage.Ref{SchemaID: "market.v1.MarketDataEvent", Version: 7}, false},
		{"a.b.C:1", storage.Ref{SchemaID: "a.b.C", Version: 1}, false},
		{"no.version", storage.Ref{}, true},
		{":3", storage.Ref{}, true},
		{"x:0", storage.Ref{}, true},
		{"x:-1", storage.Ref{}, true},
		{"x:abc", storage.Ref{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := storage.ParseRef(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestStorage(t *testing.T) {
	backends := []struct {
		name string
		make func(t *testing.T) storage.Storage
	}{
		{"memory", func(*testing.T) storage.Storage { return storage.NewMemory() }},
		{"postgres", newPostgres},
	}
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			s := b.make(t)

			ref := storage.Ref{SchemaID: "market.v1.MarketDataEvent", Version: 1}
			sc := storage.Schema{Ref: ref, Descriptor: []byte("schema-bytes-v1"), SourceTag: "v0.4.2"}

			if err := s.Put(ctx, sc); err != nil {
				t.Fatalf("put: %v", err)
			}

			got, err := s.Get(ctx, ref)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if string(got.Descriptor) != "schema-bytes-v1" {
				t.Errorf("descriptor=%q want %q", got.Descriptor, "schema-bytes-v1")
			}
			if got.Fingerprint == "" {
				t.Error("fingerprint should have been derived")
			}

			if err := s.Put(ctx, sc); err != nil {
				t.Errorf("idempotent re-put: %v", err)
			}

			conflict := sc
			conflict.Descriptor = []byte("schema-bytes-v1-different")
			conflict.Fingerprint = ""
			if err := s.Put(ctx, conflict); !errors.Is(err, storage.ErrConflict) {
				t.Errorf("conflict put: got %v want ErrConflict", err)
			}

			if _, err := s.Get(ctx, storage.Ref{SchemaID: "x.y.Z", Version: 99}); !errors.Is(err, storage.ErrNotFound) {
				t.Errorf("get missing: got %v want ErrNotFound", err)
			}

			if err := s.Ping(ctx); err != nil {
				t.Errorf("ping: %v", err)
			}
		})
	}
}

func TestRegister(t *testing.T) {
	backends := []struct {
		name string
		make func(t *testing.T) storage.Storage
	}{
		{"memory", func(*testing.T) storage.Storage { return storage.NewMemory() }},
		{"postgres", newPostgres},
	}
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			s := b.make(t)

			ref1, created, err := s.Register(ctx, "domain.v1.PortfolioState", []byte("desc-A"), "v0.5.0")
			if err != nil {
				t.Fatalf("first register: %v", err)
			}
			if !created || ref1.Version != 1 {
				t.Fatalf("first register: created=%v ref=%s want created=true ref version=1", created, ref1)
			}

			ref1b, created, err := s.Register(ctx, "domain.v1.PortfolioState", []byte("desc-A"), "v0.5.1")
			if err != nil {
				t.Fatalf("idempotent register: %v", err)
			}
			if created || ref1b != ref1 {
				t.Errorf("idempotent register: got ref=%s created=%v, want ref=%s created=false", ref1b, created, ref1)
			}

			ref2, created, err := s.Register(ctx, "domain.v1.PortfolioState", []byte("desc-B"), "v0.5.2")
			if err != nil {
				t.Fatalf("changed register: %v", err)
			}
			if !created || ref2.Version != 2 {
				t.Errorf("changed register: got ref=%s created=%v, want version=2 created=true", ref2, created)
			}

			other, created, err := s.Register(ctx, "domain.v1.PositionState", []byte("desc-A"), "v0.5.0")
			if err != nil {
				t.Fatalf("sibling register: %v", err)
			}
			if !created || other.Version != 1 {
				t.Errorf("sibling register: got ref=%s created=%v, want version=1 created=true", other, created)
			}
		})
	}
}

// testSchema isolates this suite's `schemas` table in its OWN Postgres schema.
//
// `schemas` is about as generic a table name as this tree contains, and CI runs every
// package against ONE database. Owning a schema means the suite can DROP and rebuild
// its table from the migration on every run without deciding whether some other
// package put that name in `public` first — and nothing it creates is visible to
// anybody else's migration. Same stance as services/oms/internal/order.
const testSchema = "schema_registry_storage_test"

const migrationDir = "../../migrations"

// newPostgres builds the store against the SAME variable every other DB-gated suite
// reads, and CI sets: TEST_POSTGRES_URL.
//
// This gate used to read TEST_POSTGRES_DSN, which is set by no workflow and no script
// — so the postgres arm of TestStorage and the whole of TestRegister's versioning and
// idempotency contract had never executed anywhere, while the suite reported green off
// the in-memory arm alone. storage.NewPostgres is schema-registry's ONLY production
// store (cmd/schema-registry/main.go), so "green" meant nothing about what ships.
func newPostgres(t *testing.T) storage.Storage {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL to run schema-registry storage Postgres integration tests")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET search_path TO "+testSchema)
		return err
	}

	// The pool's connections already point at the schema, so it must exist before any
	// of them is used — create it on a connection of our own.
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Exec(ctx, `DROP SCHEMA IF EXISTS `+testSchema+` CASCADE`); err != nil {
		t.Fatalf("drop schema: %v", err)
	}
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+testSchema); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("postgres connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// CI migrates only services/oms (kanz-ci.yml), so `schemas` does not exist until
	// this suite creates it. Replaying the service's real migration files is also what
	// makes 0001_init.sql itself covered — a DDL typo there fails here rather than in a
	// deployment.
	files, err := filepath.Glob(filepath.Join(migrationDir, "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob migrations %s: %v (found %d)", migrationDir, err, len(files))
	}
	sort.Strings(files)
	for _, f := range files {
		ddl, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatalf("read migration %s: %v", f, rerr)
		}
		if _, err := pool.Exec(ctx, string(ddl)); err != nil {
			t.Fatalf("apply migration %s: %v", f, err)
		}
	}
	return storage.NewPostgres(pool)
}
