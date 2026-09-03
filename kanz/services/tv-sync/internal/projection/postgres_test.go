package projection

// EXEC-M21 — tv-sync's book was LOST on every restart.
//
// The projection folds order-lifecycle FACTs into memory and the TradingView Broker API
// serves orders, executions, positions and P&L straight out of it. There was NO DATABASE
// anywhere in the service, and the consumer is a DURABLE GROUP: a restarted pod resumes at
// its last ack and never re-reads what it already folded. So a pod roll left the trader
// looking at an EMPTY account — no positions, no orders, zero P&L — while the fund's real
// positions sat open at the exchanges.
//
// And there was no way back: the EXECUTION stream that carries order.> has a 24h max-age,
// nothing archives it, and no table anywhere persists a fill. Yesterday's history was the
// most that could ever be recovered; realized P&L since inception was simply gone.
//
// These tests run against a REAL Postgres under the same non-superuser + RLS posture the
// service runs under. Gated on TEST_POSTGRES_URL.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

// testSchema isolates this package's table in its OWN Postgres schema: the suites run in
// parallel against one database and would otherwise race to DROP and CREATE it.
const testSchema = "tv_sync_projection_test"

const testTenant = "acme"

func newPool(t *testing.T, tenant string) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL to run the tv-sync fact-log integration tests")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	// Every connection lands in this schema and carries the tenant GUC — the same
	// authenticated-session-GUC posture the service runs under (internal/pg.NewTenantPool).
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

func freshSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

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

// factEnv is one FACT as the bus delivers it: the envelope carries the identity
// (event_id) that makes the fold exactly-once.
func factEnv(eventID, eventType string) *envelopepb.Envelope {
	return &envelopepb.Envelope{EventId: eventID, TenantId: testTenant, EventType: eventType}
}

// deliver folds one FACT through a projection exactly as the bus consumer would.
func deliver(t *testing.T, p *Projection, eventID, eventType string, msg proto.Message) {
	t.Helper()
	if err := p.Handle(context.Background(), factEnv(eventID, eventType), mustMarshal(t, msg)); err != nil {
		t.Fatalf("deliver %s: %v", eventID, err)
	}
}

// TestABootingPodRebuildsTheBookItLost is the point of EXEC-M21.
//
// A pod folds two fills, then DIES. A fresh pod — with a fresh, empty in-memory projection
// and a durable consumer group that will never redeliver those FACTs — must come back with
// the SAME book. Not an approximation of it: the same positions, the same executions, the
// same realized P&L, folded from the same FACTs in the same order.
func TestABootingPodRebuildsTheBookItLost(t *testing.T) {
	pool := newPool(t, testTenant)
	freshSchema(t, pool)
	ctx := context.Background()

	log := NewPostgresLog(pool)

	// The pod that is about to die. It buys 2 BTC at 100, then sells 1 at 150.
	dying := New(time.Now, nil, WithLog(log, testTenant))
	deliver(t, dying, "evt-1", evtFilled, filledOrder("o1", "fund-alpha", "BTC", orderpb.Side_SIDE_BUY, 2, 100))
	deliver(t, dying, "evt-2", evtFilled, filledOrder("o2", "fund-alpha", "BTC", orderpb.Side_SIDE_SELL, 1, 150))

	before, ok := dying.Positions(testTenant, "fund-alpha", time.Time{})
	if !ok || len(before) != 1 {
		t.Fatalf("the dying pod's own book is wrong: %+v", before)
	}

	// The pod is rolled. A NEW process, a NEW empty projection, and the consumer group will
	// not replay a single one of those FACTs.
	reborn := New(time.Now, nil, WithLog(log, testTenant))
	if pos, _ := reborn.Positions(testTenant, "fund-alpha", time.Time{}); len(pos) != 0 {
		t.Fatalf("a fresh projection was not empty: %+v", pos)
	}
	if err := reborn.Rehydrate(ctx); err != nil {
		t.Fatalf("rehydrate: %v", err)
	}

	after, ok := reborn.Positions(testTenant, "fund-alpha", time.Time{})
	if !ok {
		t.Fatal("the rebuilt book has no account — the trader sees an empty TradingView account while the fund holds 1 BTC")
	}
	if len(after) != 1 || after[0].Instrument != "BTC" {
		t.Fatalf("rebuilt positions = %+v, want one BTC holding", after)
	}
	if after[0].Qty != before[0].Qty || after[0].Side != before[0].Side {
		t.Errorf("rebuilt position = %s %s, want %s %s", after[0].Side, after[0].Qty, before[0].Side, before[0].Qty)
	}
	// Realized P&L is the number that CANNOT be recovered from anywhere else: the fill that
	// produced it aged off the 24h stream, and no other store holds it.
	if after[0].RealizedPnl != before[0].RealizedPnl {
		t.Errorf("rebuilt realized P&L = %s, want %s — the fund's realized gain was lost on a pod roll",
			after[0].RealizedPnl, before[0].RealizedPnl)
	}

	execs, _ := reborn.Executions(testTenant, "fund-alpha", "", time.Time{})
	if len(execs) != 2 {
		t.Errorf("rebuilt execution log has %d fills, want 2 — the fund's trade history did not survive the restart", len(execs))
	}
	orders, _ := reborn.Orders(testTenant, "fund-alpha", time.Time{})
	if len(orders) != 2 {
		t.Errorf("rebuilt order book has %d orders, want 2", len(orders))
	}
}

// TestAFactIsFoldedExactlyOnce: the bus redelivers (an ack lost to a crash, a pod that died
// between folding and acking). A FACT already folded must not be folded again — a second
// fold of the same fill would double the position and the realized P&L, and the Broker API
// would report a holding the fund does not have.
func TestAFactIsFoldedExactlyOnce(t *testing.T) {
	pool := newPool(t, testTenant)
	freshSchema(t, pool)

	p := New(time.Now, nil, WithLog(NewPostgresLog(pool), testTenant))
	fill := filledOrder("o1", "fund-alpha", "BTC", orderpb.Side_SIDE_BUY, 2, 100)

	deliver(t, p, "evt-1", evtFilled, fill)
	deliver(t, p, "evt-1", evtFilled, fill) // the SAME event, redelivered

	pos, _ := p.Positions(testTenant, "fund-alpha", time.Time{})
	if len(pos) != 1 || pos[0].Qty != "2" {
		t.Fatalf("positions = %+v, want a single holding of 2 BTC — the redelivery was folded twice", pos)
	}
	execs, _ := p.Executions(testTenant, "fund-alpha", "", time.Time{})
	if len(execs) != 1 {
		t.Errorf("execution log has %d entries, want 1 — the same fill was ledgered twice", len(execs))
	}
}

// TestAFactThatCouldNotBeRecordedIsNotFolded: if the log write fails, Handle must NACK.
// Folding a FACT we could not durably record puts it in RAM and nowhere else — and the
// durable consumer group would then ack it and never send it again, so the next restart
// would lose it silently. That is the original bug, one layer down.
func TestAFactThatCouldNotBeRecordedIsNotFolded(t *testing.T) {
	p := New(time.Now, nil, WithLog(brokenLog{}, testTenant))

	err := p.Handle(context.Background(), factEnv("evt-1", evtFilled),
		mustMarshal(t, filledOrder("o1", "fund-alpha", "BTC", orderpb.Side_SIDE_BUY, 2, 100)))
	if err == nil {
		t.Fatal("Handle acked a FACT it could not durably record — the consumer group will never send it again")
	}
	if pos, _ := p.Positions(testTenant, "fund-alpha", time.Time{}); len(pos) != 0 {
		t.Errorf("the FACT was folded into memory anyway: %+v", pos)
	}
}

type brokenLog struct{}

func (brokenLog) Append(context.Context, Fact) (bool, int64, error) {
	return false, 0, errors.New("postgres is down")
}
func (brokenLog) Replay(context.Context, int64, func(Fact) error) error { return nil }

func (brokenLog) SaveCheckpoint(context.Context, int64, []byte) error { return nil }

func (brokenLog) LoadCheckpoint(context.Context) ([]byte, int64, bool, error) {
	return nil, 0, false, nil
}
