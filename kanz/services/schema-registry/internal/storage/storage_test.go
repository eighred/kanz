package storage_test

import (
	"context"
	"errors"
	"os"
	"testing"

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

func newPostgres(t *testing.T) storage.Storage {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("postgres connect: %v", err)
	}
	if _, err := pool.Exec(context.Background(), "TRUNCATE schemas"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "TRUNCATE schemas")
		pool.Close()
	})
	return storage.NewPostgres(pool)
}
