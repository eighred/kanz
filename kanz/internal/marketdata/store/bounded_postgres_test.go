package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func boundedPostgres(t testing.TB) *Postgres {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("TEST_POSTGRES_URL required")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	var super, bypass bool
	if err := admin.QueryRow(ctx, "SELECT rolsuper,rolbypassrls FROM pg_roles WHERE rolname=current_user").Scan(&super, &bypass); err != nil || super || bypass {
		t.Fatalf("ordinary PostgreSQL role required: %v", err)
	}
	schema := pgx.Identifier{fmt.Sprintf("bounded_%d", time.Now().UnixNano())}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, name := range []string{"0001_price_history.sql"} {
		sql, err := os.ReadFile(filepath.Join("..", "..", "..", "services", "market-data", "migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatal(err)
		}
	}
	return NewPostgres(pool)
}

func TestBoundedPostgres(t *testing.T) {
	s := boundedPostgres(t)
	ctx := context.Background()
	if err := s.Put(ctx, boundedFixture(10000)); err != nil {
		t.Fatal(err)
	}
	checkBoundedParity(t, s)
	if _, err := s.pool.Exec(ctx, "ANALYZE price_observations"); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []PriceKind{PriceKindUnspecified, PriceKindClose} {
		var plan []byte
		if err := s.pool.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+boundedHistorySQL, "BOUND", int32(kind), nil, day(10000), day(10000), 251).Scan(&plan); err != nil {
			t.Fatal(err)
		}
		var nodes []map[string]any
		if err := json.Unmarshal(plan, &nodes); err != nil {
			t.Fatal(err)
		}
		indexed := false
		var visit func(map[string]any)
		visit = func(n map[string]any) {
			if strings.Contains(n["Node Type"].(string), "Index") {
				indexed = true
			}
			if rows, ok := n["Actual Rows"].(float64); ok && rows > 1100 {
				t.Fatalf("unbounded executor node: %s", plan)
			}
			if children, ok := n["Plans"].([]any); ok {
				for _, child := range children {
					visit(child.(map[string]any))
				}
			}
		}
		visit(nodes[0]["Plan"].(map[string]any))
		if !indexed {
			t.Fatalf("missing index: %s", plan)
		}
		t.Logf("kind %d bounded plan: %s", kind, plan)
	}
	// Cancellation must interrupt a real blocked database operation, not only an
	// already-canceled context. The exclusive lock stays owned by this transaction.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "LOCK TABLE price_observations IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	timeout, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := s.History(timeout, Query{InstrumentID: "BOUND", Limit: 251}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked query cancellation: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancellation failed to bound lock wait")
	}
}

func BenchmarkBoundedPostgres(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			s := boundedPostgres(b)
			ctx := context.Background()
			if err := s.Put(ctx, boundedFixture(n)); err != nil {
				b.Fatal(err)
			}
			if _, err := s.pool.Exec(ctx, "ANALYZE price_observations"); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				o, err := s.History(ctx, Query{InstrumentID: "BOUND", Kind: PriceKindClose, Limit: 251})
				if err != nil || len(o) != 251 {
					b.Fatalf("rows=%d err=%v", len(o), err)
				}
			}
		})
	}
}
