package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// newCoveragePool applies the REAL migration, not hand-written DDL. The merge
// rule this store depends on lives in the SQL (ON CONFLICT ... GREATEST), so a
// test against a table this file invented would prove a store the database does
// not implement.
func newCoveragePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL (kanz/test/backing/up.sh) to run the coverage store tests")
	}
	ctx := context.Background()

	schema := fmt.Sprintf("coverage_test_%d", time.Now().UnixNano())
	boot, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect (bootstrap): %v", err)
	}
	defer boot.Close()
	if _, err := boot.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	// search_path WITHOUT public, so an unqualified name resolves here and
	// nowhere else — a missing object errors instead of silently hitting a
	// shared table (#212).
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		drop, derr := pgxpool.New(context.Background(), url)
		if derr != nil {
			t.Errorf("reconnect to drop schema %s: %v — it is now residue", schema, derr)
			return
		}
		defer drop.Close()
		if _, derr := drop.Exec(context.Background(),
			`DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`); derr != nil {
			t.Errorf("drop schema %s: %v — it is now residue", schema, derr)
		}
	})

	b, err := os.ReadFile(filepath.Join("..", "..", "..", "services", "market-data", "migrations",
		"0005_ingestion_coverage.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(b)); err != nil {
		t.Fatalf("apply migration: %v", err)
	}
	return pool
}

func TestPostgresCoverageRoundTrip(t *testing.T) {
	pool := newCoveragePool(t)
	p := NewPostgres(pool)
	ctx := context.Background()

	in := []Coverage{
		cov(0, time.Minute, "binance:trades:BTCUSDT"),
		cov(1, 30*time.Second, "binance:trades:BTCUSDT"),
	}
	in[1].Breaks = 2
	if err := p.PutCoverage(ctx, in); err != nil {
		t.Fatalf("PutCoverage: %v", err)
	}
	got, err := p.Coverage(ctx, CoverageQuery{
		InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: Resolution1m,
	})
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	if !got[0].Whole() {
		t.Errorf("row 0 observed %s, want a whole bucket", got[0].Observed)
	}
	if got[1].Observed != 30*time.Second || got[1].Breaks != 2 {
		t.Errorf("row 1 = (%s, %d breaks), want (30s, 2)", got[1].Observed, got[1].Breaks)
	}
	if got[1].Attestor != "binance:trades:BTCUSDT" {
		t.Errorf("attestor did not survive the round trip: %q", got[1].Attestor)
	}
}

// THE MERGE RULE IS IN THE SQL, so it is proven against the database.
//
// A shorter re-delivery must not retract coverage that was already proven — an
// interval that goes from OBSERVED back to partial silently becomes UNKNOWN
// downstream, and UNKNOWN is what refuses claims. Last-write-wins here would be
// a data-loss bug that no unit test against a map would find.
func TestPostgresCoverageKeepsTheStrongerProof(t *testing.T) {
	pool := newCoveragePool(t)
	p := NewPostgres(pool)
	ctx := context.Background()

	strong := cov(0, time.Minute, "binance:trades:BTCUSDT")
	weak := cov(0, 20*time.Second, "binance:trades:BTCUSDT")
	weak.Breaks = 3
	weak.RecordedAt = strong.RecordedAt.Add(time.Hour) // later, and still weaker

	if err := p.PutCoverage(ctx, []Coverage{strong}); err != nil {
		t.Fatal(err)
	}
	if err := p.PutCoverage(ctx, []Coverage{weak}); err != nil {
		t.Fatal(err)
	}
	got, err := p.Coverage(ctx, CoverageQuery{
		InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: Resolution1m,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].Observed != time.Minute {
		t.Fatalf("observed = %s, want 1m — a later, shorter claim retracted coverage that was "+
			"already proven, and the interval now reads as UNKNOWN", got[0].Observed)
	}
	if got[0].Breaks != 0 {
		t.Errorf("breaks = %d, want 0 — the row that proved MORE wins wholesale, so a losing "+
			"claim's break count must not be mixed into a winning claim's coverage", got[0].Breaks)
	}
}

// TWO ATTESTORS COEXIST IN THE PRIMARY KEY. Each speaks only for itself, and
// AttestedWindowOf ORs across them — so a collapse here would decide which
// subscription's word counted by insertion order.
func TestPostgresCoverageKeepsEveryAttestorSeparately(t *testing.T) {
	pool := newCoveragePool(t)
	p := NewPostgres(pool)
	ctx := context.Background()

	if err := p.PutCoverage(ctx, []Coverage{
		cov(0, 20*time.Second, "okx:trades:BTC-USDT"),
		cov(0, time.Minute, "binance:trades:BTCUSDT"),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := p.Coverage(ctx, CoverageQuery{
		InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: Resolution1m,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 — one attestor's claim must not overwrite another's", len(got))
	}
	att, _ := AttestedWindowOf(nil, got, covBase, covBase.Add(time.Minute), Resolution1m)
	if att.Quiet != 1 || att.Unknown != 0 {
		t.Fatalf("Quiet=%d Unknown=%d, want 1/0 — one whole attestor is enough to explain the "+
			"absence, whichever order the rows came back in", att.Quiet, att.Unknown)
	}
}

// THE DATABASE REFUSES AN OVER-CLAIM TOO, not only the Go validator.
//
// The CHECK constraint is the backstop for anything that reaches the table
// without going through PutCoverage — a repair script, a future writer, a
// migration. An observed_ns longer than its bucket is the one value that would
// make a window maximally trustworthy for maximally wrong reasons.
func TestPostgresCoverageRefusesANegativeObservation(t *testing.T) {
	pool := newCoveragePool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		INSERT INTO ingestion_coverage
			(instrument_id, venue, resolution, bucket_start, observed_ns, breaks, attestor, recorded_at)
		VALUES ('BTC-USDT', 'XBIN', '1m', $1, -1, 0, 'hand-written', $1)
	`, covBase)
	if err == nil {
		t.Fatal("the database accepted a negative observed_ns — the CHECK is the backstop for " +
			"every writer that does not go through PutCoverage")
	}
}
