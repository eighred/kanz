package position

// EXEC-M18 — the position book was an in-memory map, PER POD, fed by a durable
// consumer GROUP (which load-balances), on a deployment shipping `replicas: 2`.
//
// So each pod folded only the fills IT received, held a PARTIAL book, and published
// domain.v1.PositionState as an ABSOLUTE quantity. The risk engine, the compliance
// monitor and tv-sync all believed it — and so did the OMS's own PRE-TRADE GATE, which
// projects every order onto this book before admitting it. A control evaluating a
// concentration limit against half the fund's positions does not fail loudly; it
// quietly says yes.
//
// These drive a REAL Postgres under a NON-SUPERUSER role, so RLS is enforced rather
// than bypassed.

import (
	"context"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dec"
)

// testSchema isolates this package's tables in their OWN Postgres schema.
//
// `go test ./...` runs packages IN PARALLEL, and the order-store suite drops and
// re-applies the very same migrations against the very same database. Sharing the public
// schema means two packages racing to DROP and CREATE `orders`. Each suite owns a
// schema; nobody trips over anybody.
//
// This used to name a second reason — migration 0002's DO-block rewriting the RLS
// policy on every table in current_schema(), and racing itself with "tuple
// concurrently updated". That is no longer true: #227 scoped every
// *_tenant_scope_required.sql to a literal list of its OWN service's tables, and
// test/arch/migration_table_discovery_test.go fails the build if catalog discovery
// comes back. The schema isolation above still earns its keep on the first reason.
const testSchema = "oms_position_test"

func newPool(t *testing.T, tenant string) *pgxpool.Pool {
	t.Helper()
	return newTracedPool(t, tenant, nil)
}

// newTracedPool is newPool with a pgx QueryTracer attached. The tracer is how
// concurrent_venues_test.go pins an interleaving that would otherwise depend on the
// scheduler: a concurrency test that has never been seen to fail proves nothing, and one
// that reproduces by luck fails to reproduce on the machine that needs it. A nil tracer is
// the ordinary pool.
func newTracedPool(t *testing.T, tenant string, tracer pgx.QueryTracer) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL to run position Postgres integration tests")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.ConnConfig.Tracer = tracer
	// Every connection lands in this package's schema and carries the tenant GUC — the
	// same authenticated-session-GUC posture the services run under (internal/pg).
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if _, err := conn.Exec(ctx, "SET search_path TO "+testSchema); err != nil {
			return err
		}
		_, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", tenant)
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// freshSchema recreates this package's schema and applies the migrations into it.
func freshSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	// The pool's connections already point at the schema, so it must exist before they
	// are used — recreate it on a connection of our own, with an explicit search_path.
	admin, err := pgxpool.New(ctx, os.Getenv("TEST_POSTGRES_URL"))
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

// buy is one BUY fill of qty at price, at a venue — because a holding sits AT a venue and
// the store refuses a fill that cannot say where it happened (EXEC-M19a).
func buy(id, instrument, qty, price string, at time.Time) *orderpb.Fill {
	f := fillAt(id, instrument, qty, price, orderpb.Side_SIDE_BUY, at)
	f.Venue = "XBIN"
	return f
}

func fillAt(id, instrument, qty, price string, side orderpb.Side, at time.Time) *orderpb.Fill {
	q, _ := new(big.Rat).SetString(qty)
	p, _ := new(big.Rat).SetString(price)
	return &orderpb.Fill{
		FillId:       id,
		InstrumentId: instrument,
		Side:         side,
		Quantity:     dec.ToProto(q),
		Price:        dec.ToProto(p),
		ExecutedAt:   timestamppb.New(at),
	}
}

// TestTwoPodsConvergeOnOneBook is THE test for EXEC-M18.
//
// Two OMS pods, one durable book. The consumer group hands pod A one fill and pod B the
// other — which is exactly what a load-balanced group does, and exactly what made the
// in-memory book wrong. Each pod must publish the TRUE ABSOLUTE position, not the part of
// it that it happened to see.
//
// With the old per-pod map: pod A says 1 BTC, pod B says 2 BTC, and the fund holds 3.
// Whoever published last wins, and the risk engine, the compliance monitor and the
// pre-trade gate all believe a number that is short by every fill the other pod folded.
func TestTwoPodsConvergeOnOneBook(t *testing.T) {
	pool := newPool(t, "__system__")
	freshSchema(t, pool)
	ctx := context.Background()
	now := time.Now().UTC()

	podA := NewPostgres(pool, "USD")
	podB := NewPostgres(pool, "USD")

	// The group splits the stream: A gets the first fill, B the second.
	if _, err := podA.Apply(ctx, "fund-alpha", buy("f1", "BTC-USD", "1", "50000", now), now, nil); err != nil {
		t.Fatalf("pod A apply: %v", err)
	}
	st, err := podB.Apply(ctx, "fund-alpha", buy("f2", "BTC-USD", "2", "50000", now), now, nil)
	if err != nil {
		t.Fatalf("pod B apply: %v", err)
	}

	// Pod B never saw fill f1. It must still publish the fund's REAL position.
	if got := dec.FromProto(st.Aggregate.GetQuantity()); got.Cmp(big.NewRat(3, 1)) != 0 {
		t.Fatalf("pod B published position %s, want 3 — it folded only its own fill and asserted it as the ABSOLUTE position", got.RatString())
	}

	// And both pods read the same book.
	for name, s := range map[string]*Postgres{"podA": podA, "podB": podB} {
		snap, err := s.Snapshot(ctx, "fund-alpha", now)
		if err != nil {
			t.Fatalf("%s snapshot: %v", name, err)
		}
		if n := len(snap.GetPositions()); n != 1 {
			t.Fatalf("%s snapshot has %d positions, want 1", name, n)
		}
		if got := dec.FromProto(snap.GetPositions()[0].GetQuantity()); got.Cmp(big.NewRat(3, 1)) != 0 {
			t.Errorf("%s snapshot quantity = %s, want 3 — the PRE-TRADE GATE reads this", name, got.RatString())
		}
	}
}

// TestTheSameFillIsCountedOnce: a redelivery, or the same fill reaching both pods, must
// not double the position. The fill ledger is the engine-side guarantee.
func TestTheSameFillIsCountedOnce(t *testing.T) {
	pool := newPool(t, "__system__")
	freshSchema(t, pool)
	ctx := context.Background()
	now := time.Now().UTC()

	podA := NewPostgres(pool, "USD")
	podB := NewPostgres(pool, "USD")

	f := buy("dup-1", "BTC-USD", "1", "50000", now)
	if _, err := podA.Apply(ctx, "fund-alpha", f, now, nil); err != nil {
		t.Fatal(err)
	}
	st, err := podB.Apply(ctx, "fund-alpha", f, now, nil) // same fill_id, second delivery
	if err != nil {
		t.Fatal(err)
	}
	if got := dec.FromProto(st.Aggregate.GetQuantity()); got.Cmp(big.NewRat(1, 1)) != 0 {
		t.Errorf("position = %s after the SAME fill was applied twice, want 1", got.RatString())
	}
}

// TestTheBookSurvivesARestart: a pod that comes back must not come back flat. With the
// in-memory map it did — and the next 0.1 BTC fill published `position = 0.1` while the
// fund held 5.1.
func TestTheBookSurvivesARestart(t *testing.T) {
	pool := newPool(t, "__system__")
	freshSchema(t, pool)
	ctx := context.Background()
	now := time.Now().UTC()

	before := NewPostgres(pool, "USD")
	if _, err := before.Apply(ctx, "fund-alpha", buy("f1", "BTC-USD", "5", "50000", now), now, nil); err != nil {
		t.Fatal(err)
	}

	// The pod restarts: a brand-new store, nothing carried over in memory.
	after := NewPostgres(pool, "USD")
	st, err := after.Apply(ctx, "fund-alpha", buy("f2", "BTC-USD", "0.1", "51000", now), now, nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := new(big.Rat).SetString("5.1")
	if got := dec.FromProto(st.Aggregate.GetQuantity()); got.Cmp(want) != 0 {
		t.Errorf("position after restart = %s, want 5.1 — the restarted pod published a position built from nothing", got.RatString())
	}
}

// TestPositionsAreTenantIsolated: the house guard. Another tenant cannot see the book,
// and an unscoped session cannot read it at all (app_current_tenant RAISES).
func TestPositionsAreTenantIsolated(t *testing.T) {
	acme := newPool(t, "acme")
	freshSchema(t, acme)
	ctx := context.Background()
	now := time.Now().UTC()

	if _, err := NewPostgres(acme, "USD").Apply(ctx, "fund-alpha", buy("f1", "BTC-USD", "1", "50000", now), now, nil); err != nil {
		t.Fatal(err)
	}

	other := newPool(t, "other-tenant")
	snap, err := NewPostgres(other, "USD").Snapshot(ctx, "fund-alpha", now)
	if err != nil {
		t.Fatalf("other tenant snapshot: %v", err)
	}
	if n := len(snap.GetPositions()); n != 0 {
		t.Errorf("another tenant read %d of acme's positions, want 0", n)
	}
}

// buyAtVenue is a BUY fill AT a venue — which is where a holding actually sits.
func buyAtVenue(id, instrument, venue, qty, price string, at time.Time) *orderpb.Fill {
	f := buy(id, instrument, qty, price, at)
	f.Venue = venue
	return f
}

// TestTwoVenuesAreTwoHoldings is THE test for EXEC-M19a.
//
// The fund holds 1 BTC at Binance and 2 BTC at OKX. Those are DIFFERENT HOLDINGS: you
// cannot sell the OKX BTC on Binance, and a CLOSE signal must flatten each venue for what
// it actually holds. The book keyed on (portfolio, instrument) alone, so the second venue's
// fill OVERWROTE the first — the platform could not answer "how much BTC is at OKX", and
// webhook-ingest's CLOSE path was wired to an empty map because there was nothing to wire.
//
// The FUND still holds 3 BTC, and that — the aggregate — is what the risk engine and the
// compliance monitor consume. Both must be true at once.
func TestTwoVenuesAreTwoHoldings(t *testing.T) {
	pool := newPool(t, "__system__")
	freshSchema(t, pool)
	ctx := context.Background()
	now := time.Now().UTC()

	store := NewPostgres(pool, "USD")

	if _, err := store.Apply(ctx, "fund-alpha", buyAtVenue("f1", "BTC-USD", "XBIN", "1", "50000", now), now, nil); err != nil {
		t.Fatal(err)
	}
	res, err := store.Apply(ctx, "fund-alpha", buyAtVenue("f2", "BTC-USD", "XOKX", "2", "50000", now), now, nil)
	if err != nil {
		t.Fatal(err)
	}

	// The per-venue FACT is about OKX ALONE — 2 BTC, not the fund's 3.
	if got := dec.FromProto(res.Venue.GetQuantity()); got.Cmp(big.NewRat(2, 1)) != 0 {
		t.Errorf("OKX holding = %s, want 2 — a CLOSE at OKX must flatten what OKX holds", got.RatString())
	}
	if res.Venue.GetVenue() != "XOKX" {
		t.Errorf("per-venue FACT carries venue %q, want XOKX", res.Venue.GetVenue())
	}

	// The fund-level FACT is the SUM across venues — what risk and compliance consume. If
	// this reported 2, the fund's BTC exposure would be understated by everything held at
	// Binance, and every limit checked against it would be checked against a smaller book.
	if got := dec.FromProto(res.Aggregate.GetQuantity()); got.Cmp(big.NewRat(3, 1)) != 0 {
		t.Errorf("fund-level BTC = %s, want 3 — one venue's holding overwrote the other", got.RatString())
	}
	if res.Aggregate.GetVenue() != "" {
		t.Errorf("the aggregate carries venue %q — it belongs to no single venue", res.Aggregate.GetVenue())
	}
}

// TestSnapshotAggregatesAcrossVenues: the PRE-TRADE GATE reads Snapshot. It must see the
// fund's whole BTC position, not one venue's slice of it — a concentration limit checked
// against a fraction of the holding does not refuse loudly, it says yes.
func TestSnapshotAggregatesAcrossVenues(t *testing.T) {
	pool := newPool(t, "__system__")
	freshSchema(t, pool)
	ctx := context.Background()
	now := time.Now().UTC()

	store := NewPostgres(pool, "USD")
	if _, err := store.Apply(ctx, "fund-alpha", buyAtVenue("f1", "BTC-USD", "XBIN", "1", "50000", now), now, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Apply(ctx, "fund-alpha", buyAtVenue("f2", "BTC-USD", "XOKX", "2", "50000", now), now, nil); err != nil {
		t.Fatal(err)
	}

	snap, err := store.Snapshot(ctx, "fund-alpha", now)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(snap.GetPositions()); n != 1 {
		t.Fatalf("snapshot has %d positions, want 1 — BTC held at two venues is ONE instrument to the gate", n)
	}
	if got := dec.FromProto(snap.GetPositions()[0].GetQuantity()); got.Cmp(big.NewRat(3, 1)) != 0 {
		t.Errorf("snapshot BTC = %s, want 3", got.RatString())
	}
}
