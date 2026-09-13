package report

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eighred/kanz/services/audit/internal/audit"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func prefixPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL to run real audit prefix tests")
	}
	ctx := context.Background()
	boot, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer boot.Close()
	var privileged bool
	if err := boot.QueryRow(ctx, `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&privileged); err != nil || privileged {
		t.Fatalf("restricted role required: privileged=%v err=%v", privileged, err)
	}
	schema := fmt.Sprintf("audit_prefix_%d", time.Now().UnixNano())
	name := pgx.Identifier{schema}.Sanitize()
	if _, err := boot.Exec(ctx, "CREATE SCHEMA "+name); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		cleanup, err := pgxpool.New(context.Background(), url)
		if err != nil {
			t.Error(err)
			return
		}
		defer cleanup.Close()
		if _, err := cleanup.Exec(context.Background(), "DROP SCHEMA "+name+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("migrations unavailable: %v", err)
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(data)); err != nil {
			t.Fatal(err)
		}
	}
	return pool
}

func TestPostgresReportPrefixSurvivesAnInterleavedAppend(t *testing.T) {
	for _, empty := range []bool{true, false} {
		checkPrefix(t, audit.NewPostgres(prefixPool(t)), empty)
	}
}

type appendDuringScan struct {
	audit.Store
	appended *audit.Record
}

func (s *appendDuringScan) Scan(ctx context.Context, yield func(*audit.Record) error) error {
	return s.Store.Scan(ctx, func(r *audit.Record) error {
		if s.appended == nil {
			// A separate connection commits while the original query's snapshot
			// remains open. The streamed prefix must keep its original head.
			var err error
			s.appended, err = s.Store.Append(ctx, &audit.Record{EventID: "during", TenantID: "test-tenant", Kind: audit.KindEvent})
			if err != nil {
				return err
			}
		}
		return yield(r)
	})
}

func TestPostgresVerificationHeadComesFromItsScanSnapshot(t *testing.T) {
	ctx := context.Background()
	store := audit.NewPostgres(prefixPool(t))
	first, err := store.Append(ctx, &audit.Record{EventID: "first", TenantID: "test-tenant", Kind: audit.KindEvent})
	if err != nil {
		t.Fatal(err)
	}
	wrapper := &appendDuringScan{Store: store}
	att, err := Verify(ctx, wrapper)
	if err != nil {
		t.Fatal(err)
	}
	if wrapper.appended == nil || !att.Verified || att.Records != 1 || att.HeadSeq != first.Seq || att.Head != first.Hash() {
		t.Fatalf("incoherent scan: %+v", att)
	}
	later, found, err := store.Get(ctx, "during")
	if err != nil || !found || later.Seq <= att.HeadSeq {
		t.Fatalf("append did not commit during scan: %+v %v", later, err)
	}
}
