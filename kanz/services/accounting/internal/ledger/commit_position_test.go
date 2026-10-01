package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/outbox"
	"github.com/jackc/pgx/v5/pgxpool"
)

func applyCommitPositionMigration(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ddl, err := os.ReadFile(filepath.Join(migrationDir, "0019_journal_commit_position.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), string(ddl)); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresBatchInsertDoesNotCreateCommitPositionGaps(t *testing.T) {
	pool := newPool(t)
	ctx := t.Context()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.venue_account_id','',true)`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries(tenant_id,entry_id,portfolio_id,entry_type,instrument_id,quantity,price,effective_time,knowledge_time)
		 SELECT app_current_tenant(),'batch-'||i,'PF',0,'AAPL','1','10',$1,$2 FROM generate_series(1,4) i ON CONFLICT DO NOTHING`, day(1), day(10)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	st := NewPostgres(pool)
	if snap := checkpointAll(t, st, "PF"); snap.JournalPosition != 4 {
		t.Fatalf("batch/duplicates produced cursor gap: %d", snap.JournalPosition)
	}
	tail, err := st.JournalSince(ctx, "PF", 2)
	if err != nil || len(tail) != 2 {
		t.Fatalf("batch tail: %d %v", len(tail), err)
	}
	equalCurrentJournal(t, st, "PF")
}

func TestPostgresCommitPositionSerializesConcurrentWriters(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if err := st.Append(ctx, trade("first", "AAPL", "100", "10", 1, 10), nil); err != nil {
		t.Fatal(err)
	}
	checkpointAll(t, st, "PF")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- st.Append(ctx, trade("writer-one", "AAPL", "1", "10", 2, 5), func(context.Context, Store) ([]outbox.Record, error) {
			close(entered)
			select {
			case <-release:
				return nil, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cfg := pool.Config()
	cfg.ConnConfig.RuntimeParams["application_name"] = "ledger-position-contender"
	other, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { unblock(); cancel(); other.Close() }()
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- NewPostgres(other).Append(ctx, trade("writer-two", "AAPL", "1", "10", 3, 1), nil)
	}()
	// Observe an actual database lock wait, not a scheduler-dependent sleep.
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name='ledger-position-contender' AND wait_event='advisory')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-secondDone:
			t.Fatalf("second writer bypassed serialization: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	// Uncommitted appends cannot leak into either a full read or its checkpoint tail.
	if reason := equalCurrentJournal(t, st, "PF"); reason != "" {
		t.Fatalf("uncommitted cursor leaked: %s", reason)
	}
	unblock()
	for _, done := range []chan error{firstDone, secondDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	tail, err := st.JournalSince(ctx, "PF", 1)
	if err != nil || len(tail) != 2 {
		t.Fatalf("concurrent tail %v %v", tail, err)
	}
	for id, want := range map[string]int64{"writer-one": 2, "writer-two": 3} {
		var got int64
		if err := pool.QueryRow(ctx, `SELECT position FROM ledger_append_positions WHERE entry_id=$1`, id).Scan(&got); err != nil || got != want {
			t.Fatalf("%s position=%d want=%d err=%v", id, got, want, err)
		}
	}
	equalCurrentJournal(t, NewPostgres(pool), "PF")
}

func commitStores(t *testing.T, test func(*testing.T, Store)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { test(t, NewMemoryStore()) })
	t.Run("postgres", func(t *testing.T) { test(t, NewPostgres(newPool(t))) })
}

func checkpointAll(t *testing.T, st Store, portfolio string) *Snapshot {
	t.Helper()
	events, err := st.Journal(t.Context(), portfolio)
	if err != nil {
		t.Fatal(err)
	}
	var through time.Time
	for _, e := range events {
		if e.Knowledge.After(through) {
			through = e.Knowledge
		}
	}
	snap := ReplayAsOf(portfolio, events, time.Now(), time.Time{}).Snapshot(through)
	if err := st.SaveSnapshot(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	return snap
}

func equalCurrentJournal(t *testing.T, st Store, portfolio string) FullScanReason {
	t.Helper()
	events, err := st.Journal(t.Context(), portfolio)
	if err != nil {
		t.Fatal(err)
	}
	want := ReplayAsOf(portfolio, events, time.Now(), time.Time{})
	got, reason, err := MaterializeCurrent(t.Context(), st, portfolio)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(encodePositions(got.Positions), encodePositions(want.Positions)) ||
		!reflect.DeepEqual(encodeRatMap(got.Cash), encodeRatMap(want.Cash)) ||
		!reflect.DeepEqual(encodePositions(got.SettledPositions), encodePositions(want.SettledPositions)) ||
		!reflect.DeepEqual(encodeRatMap(got.SettledCash), encodeRatMap(want.SettledCash)) ||
		!reflect.DeepEqual(encodeRatMap(got.Accrued), encodeRatMap(want.Accrued)) || got.journalPosition != want.journalPosition {
		t.Fatalf("checkpoint differs from journal: position %d/%d, holdings %v/%v cash %v/%v reason=%q", got.journalPosition, want.journalPosition, encodePositions(got.Positions), encodePositions(want.Positions), encodeRatMap(got.Cash), encodeRatMap(want.Cash), reason)
	}
	return reason
}

func TestLateOrdinaryCommitsAreNeverLostBehindKnowledgeWatermark(t *testing.T) {
	commitStores(t, func(t *testing.T, st Store) {
		ctx := t.Context()
		first := trade("first", "AAPL", "100", "10", 1, 10)
		if err := st.Append(ctx, first, nil); err != nil {
			t.Fatal(err)
		}
		checkpointAll(t, st, "PF")
		late := []*Event{
			trade("late-buy", "AAPL", "1", "12", 2, 2),
			trade("equal-knowledge-sell", "AAPL", "-20", "15", 3, 10),
			cashEntry("late-deposit", "1000", 4, 1),
			cashEntry("late-fee", "-7", 5, 1),
		}
		late[3].Type = EntryFee
		for _, e := range late {
			if err := st.Append(ctx, e, func(ctx context.Context, pending Store) ([]outbox.Record, error) {
				book, reason, err := MaterializeCurrent(ctx, pending, "PF")
				if err != nil {
					return nil, err
				}
				events, err := pending.Journal(ctx, "PF")
				if err != nil {
					return nil, err
				}
				full := Replay("PF", events)
				if !reflect.DeepEqual(encodeRatMap(book.Cash), encodeRatMap(full.Cash)) {
					return nil, fmt.Errorf("staged cash omitted late entry, reason=%s", reason)
				}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			if reason := equalCurrentJournal(t, st, "PF"); reason != "" {
				t.Fatalf("ordinary append should remain bounded: %s", reason)
			}
			stale, err := st.StalePortfolios(ctx, 10)
			if err != nil || len(stale) != 1 {
				t.Fatalf("late commit not queued: %v %v", stale, err)
			}
			checkpointAll(t, st, "PF")
		}
		snap, err := st.LoadSnapshot(ctx, "PF")
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range late {
			if err := st.Append(ctx, e, nil); err != nil {
				t.Fatal(err)
			}
		}
		tail, err := st.JournalSince(ctx, "PF", snap.JournalPosition)
		if err != nil || len(tail) != 0 {
			t.Fatalf("redelivery advanced position: %v %v", tail, err)
		}
		equalCurrentJournal(t, st, "PF")
	})
}

func TestCommitPositionRollbackAndStaleCheckpointFence(t *testing.T) {
	commitStores(t, func(t *testing.T, st Store) {
		ctx := t.Context()
		e := trade("first", "AAPL", "100", "10", 1, 10)
		if err := st.Append(ctx, e, nil); err != nil {
			t.Fatal(err)
		}
		old := checkpointAll(t, st, "PF")
		late := trade("late", "AAPL", "1", "10", 2, 2)
		sentinel := errors.New("announcement refused")
		if err := st.Append(ctx, late, func(context.Context, Store) ([]outbox.Record, error) { return nil, sentinel }); !errors.Is(err, sentinel) {
			t.Fatalf("rollback: %v", err)
		}
		if err := st.SaveSnapshot(ctx, old); err != nil {
			t.Fatalf("rollback advanced head: %v", err)
		}
		tail, err := st.JournalSince(ctx, "PF", 1)
		if err != nil || len(tail) != 0 {
			t.Fatalf("rollback tail: %v %v", tail, err)
		}
		if err := st.Append(ctx, late, nil); err != nil {
			t.Fatal(err)
		}
		if err := st.SaveSnapshot(ctx, old); !errors.Is(err, ErrStaleSnapshot) {
			t.Fatalf("stale snapshot accepted: %v", err)
		}
		equalCurrentJournal(t, st, "PF")
		if snap := checkpointAll(t, st, "PF"); snap.JournalPosition != 2 {
			t.Fatalf("rollback left gap: %d", snap.JournalPosition)
		}
	})
}

func TestEqualEffectiveTailReplaysCanonicalOrder(t *testing.T) {
	commitStores(t, func(t *testing.T, st Store) {
		// The late buy sorts before the sale even though it commits afterwards.
		for _, e := range []*Event{trade("buy", "AAPL", "100", "10", 1, 1), trade("sale", "AAPL", "-50", "20", 2, 10)} {
			if err := st.Append(t.Context(), e, nil); err != nil {
				t.Fatal(err)
			}
		}
		checkpointAll(t, st, "PF")
		if err := st.Append(t.Context(), trade("late-buy", "AAPL", "100", "30", 2, 2), nil); err != nil {
			t.Fatal(err)
		}
		if reason := equalCurrentJournal(t, st, "PF"); reason != FullScanBackdatedTail {
			t.Fatalf("unfenced canonical tie: %s", reason)
		}
	})
}

func TestPostgresLegacyPositionMigrationAndOldWriterRejection(t *testing.T) {
	pool := newPoolThrough(t, "0018_corporate_action_revisions.sql")
	ctx := t.Context()
	// Seed using the old write shape: migration must not rewrite these records.
	if _, err := pool.Exec(ctx, `SELECT set_config('app.venue_account_id','',false)`); err != nil {
		t.Fatal(err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = conn.Exec(ctx, `SELECT set_config('app.venue_account_id','',false)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = conn.Exec(ctx, `INSERT INTO ledger_entries(tenant_id,entry_id,portfolio_id,entry_type,instrument_id,quantity,price,effective_time,knowledge_time) VALUES(current_setting('app.tenant_id'),'legacy','PF',0,'AAPL','100','10',$1,$2)`, day(1), day(10))
	if err != nil {
		t.Fatal(err)
	}
	_, err = conn.Exec(ctx, `INSERT INTO ledger_snapshots(tenant_id,portfolio_id,positions,through_time,max_effective_time,corporate_action_version) VALUES(current_setting('app.tenant_id'),'PF','{}',$1,$1,1)`, day(10))
	if err != nil {
		t.Fatal(err)
	}
	var before string
	if err := conn.QueryRow(ctx, `SELECT row_to_json(e)::text FROM ledger_entries e WHERE entry_id='legacy'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO ledger_entries(tenant_id,entry_id,portfolio_id,entry_type,instrument_id,quantity,price,effective_time,knowledge_time) VALUES(current_setting('app.tenant_id'),'cold-legacy','COLD',0,'AAPL','100','10',$1,$2)`, day(1), day(10)); err != nil {
		t.Fatal(err)
	}
	conn.Release()
	applyCommitPositionMigration(t, pool)
	st := NewPostgres(pool)
	cold := trade("cold-late", "AAPL", "1", "10", 2, 2)
	cold.PortfolioID = "COLD"
	if err := st.Append(ctx, cold, nil); err != nil {
		t.Fatal(err)
	}
	if snap := checkpointAll(t, st, "COLD"); snap.JournalPosition != 2 {
		t.Fatalf("lazy legacy append baseline: %d", snap.JournalPosition)
	}
	equalCurrentJournal(t, st, "COLD")
	snap, err := st.LoadSnapshot(ctx, "PF")
	if err != nil || !snap.MaxEffective.IsZero() {
		t.Fatalf("legacy snapshot trusted: %+v %v", snap, err)
	}
	var after string
	if err := pool.QueryRow(ctx, `SELECT row_to_json(e)::text FROM ledger_entries e WHERE entry_id='legacy'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("migration rewrote financial history")
	}
	checkpointAll(t, st, "PF")
	if err := st.Append(ctx, trade("late", "AAPL", "1", "10", 2, 2), nil); err != nil {
		t.Fatal(err)
	}
	equalCurrentJournal(t, NewPostgres(pool), "PF") // reconstructed store, durable cursor
	if _, err := pool.Exec(ctx, `UPDATE ledger_snapshots SET positions='{}',corporate_action_version=1 WHERE portfolio_id='PF'`); err == nil {
		t.Fatal("old writer replaced new checkpoint")
	}
	if _, err := pool.Exec(ctx, `UPDATE ledger_append_positions SET position=99 WHERE portfolio_id='PF'`); err == nil {
		t.Fatal("append index is mutable")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM ledger_append_positions WHERE portfolio_id='PF'`); err == nil {
		t.Fatal("append index can be deleted")
	}
	snap = checkpointAll(t, st, "PF")
	if snap.JournalPosition != 2 {
		t.Fatalf("legacy baseline: %d", snap.JournalPosition)
	}
	// An old binary still inserts only journal columns. The database trigger
	// must index its accepted row and reject a duplicate without a cursor gap.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.venue_account_id','',true)`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries(tenant_id,entry_id,portfolio_id,entry_type,instrument_id,quantity,price,effective_time,knowledge_time) VALUES(current_setting('app.tenant_id'),'old-writer','PF',0,'AAPL','1','10',$1,$2) ON CONFLICT DO NOTHING`, day(3), day(1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if reason := equalCurrentJournal(t, st, "PF"); reason != "" {
		t.Fatalf("old writer not indexed: %s", reason)
	}
	if snap := checkpointAll(t, st, "PF"); snap.JournalPosition != 3 {
		t.Fatalf("old writer duplicate consumed a position: %d", snap.JournalPosition)
	}
	other, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Rollback(ctx) }()
	if _, err := other.Exec(ctx, `SELECT set_config('app.tenant_id','other-tenant',true)`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"ledger_heads", "ledger_append_positions"} {
		var n int
		if err := other.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("cross-tenant %s visibility: %d %v", table, n, err)
		}
	}
	if _, err := other.Exec(ctx, `INSERT INTO ledger_heads(tenant_id,portfolio_id,position) VALUES('__system__','foreign',0)`); err == nil {
		t.Fatal("cross-tenant head insertion accepted")
	}
}
