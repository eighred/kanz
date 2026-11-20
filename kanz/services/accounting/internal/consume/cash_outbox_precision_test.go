package consume_test

import (
	"context"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/outbox"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/custody"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestSealedCashOutboxPreservesNanosecondsAcrossRestart(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("requires restricted PostgreSQL")
	}
	const tenant, portfolio = "cash-clock-tenant", "PF-clock"
	ctx := bus.WithTenantID(t.Context(), tenant)
	pool := cashTimeRestrictedPool(t, ctx, dsn, tenant)
	store := ledger.NewPostgres(pool)
	pub := new(cashTimeCapture)
	appendCash := func(id string, at time.Time, alter bool) error {
		e := &ledger.Event{EntryID: id, PortfolioID: portfolio, VenueAccountID: "ACC", Type: ledger.EntryCash,
			Cash: big.NewRat(1, 1), CashCurrency: "USD", Effective: at.Add(-time.Second), Knowledge: at.Add(-time.Second)}
		return store.Append(ctx, e, func(ctx context.Context, view ledger.Store) ([]outbox.Record, error) {
			records, err := cashTimeAnnouncer(store, pub, at).Records(ctx, view, portfolio)
			if err == nil && alter {
				records[0].EventTime = at.Add(time.Nanosecond)
			}
			return records, err
		})
	}
	at := time.Unix(1_700_000_000, 123456321).UTC()
	if err := appendCash("uncommitted", at, true); err == nil {
		t.Fatal("a one-nanosecond mismatch committed a journal entry")
	}
	if entries, rows := cashTimeCounts(t, ctx, pool, portfolio); entries != 0 || rows != 0 {
		t.Fatalf("failed clock validation left entries=%d, outbox=%d", entries, rows)
	}
	times := []time.Time{at, at.Add(400 * time.Nanosecond)}
	for i, id := range []string{"first", "second"} {
		if err := appendCash(id, times[i], false); err != nil {
			t.Fatal(err)
		}
	}
	var sqlTimes, revisions int
	if err := pool.QueryRow(ctx, `SELECT count(DISTINCT event_time),(SELECT count(*) FROM cash_commit_history)
		FROM outbox WHERE partition_key=$1`, portfolio).Scan(&sqlTimes, &revisions); err != nil || sqlTimes != 1 || revisions != 2 {
		t.Fatalf("SQL cutoffs=%d immutable cash revisions=%d err=%v", sqlTimes, revisions, err)
	}
	pending, err := store.Outbox().Pending(ctx, portfolio, 4)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending=%d err=%v", len(pending), err)
	}
	for i, row := range pending {
		if row.RecordError != nil || !row.Record.EventTime.Equal(times[i]) {
			t.Fatalf("row %d cutoff=%s err=%v", row.ID, row.Record.EventTime, row.RecordError)
		}
	}
	// This was the 7077 write path. New durable rows and inherited pending
	// rows share a schema, so recovery needs no rewrite of economic history.
	legacy := func(rec outbox.Record) {
		t.Helper()
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			return outbox.Enqueue(ctx, tx, outbox.ServiceAccounting, rec)
		}); err != nil {
			t.Fatal(err)
		}
	}
	rec := pending[0].Record
	legacy(rec)
	bad := rec
	bad.EventTime = at.Add(time.Microsecond)
	legacy(bad)

	// Custody can own the background drain of the shared accounting queue.
	// The earlier good rows publish before the corrupt row stalls this key.
	relay, err := outbox.NewRelay(custody.NewPostgres(pool).Outbox(), pub, nil)
	if err != nil {
		t.Fatal(err)
	}
	n, err := relay.Flush(ctx, portfolio)
	if n != 3 || err == nil || len(pub.events) != 3 {
		t.Fatalf("flush published %d, captured %d, err=%v", n, len(pub.events), err)
	}
	for i, want := range []time.Time{times[0], times[1], times[0]} {
		e := pub.events[i]
		if !e.EventTime.Equal(want) || !e.Payload.(*accountingpb.PortfolioCashBalance).AsOf.AsTime().Equal(want) {
			t.Fatalf("cash event %d has divergent cutoff", i)
		}
	}
	left, err := store.Outbox().Pending(ctx, portfolio, 10)
	if err != nil || len(left) != 1 || left[0].Attempts != 1 || left[0].RecordError == nil || left[0].LastError == "" {
		t.Fatalf("corrupt inherited row did not remain pending with an attributable error: %+v, %v", left, err)
	}
	for _, change := range []string{"partition", "source tenant", "same-microsecond payload"} {
		bad := rec
		switch change {
		case "partition":
			bad.PartitionKey = "different-portfolio"
		case "source tenant":
			bad.TenantID = "different-tenant"
		case "same-microsecond payload":
			e, err := rec.Event()
			if err != nil {
				t.Fatal(err)
			}
			msg := e.Payload.(*accountingpb.PortfolioCashBalance)
			msg.AsOf, msg.KnowledgeTime = timestamppb.New(times[1]), timestamppb.New(times[1])
			bad.Payload, err = proto.Marshal(msg)
			if err != nil {
				t.Fatal(err)
			}
		}
		legacy(bad)
		rows, err := store.Outbox().Pending(ctx, bad.PartitionKey, 10)
		if err != nil || len(rows) == 0 || rows[len(rows)-1].RecordError == nil {
			t.Fatalf("accepted legacy cash row with inconsistent %s: %v", change, err)
		}
	}
}
