// Ledger WORM enforcement (0004_ledger_worm.sql).
//
// 0001_ledger.sql calls the table an "append-only journal", and the Go code
// honours that -- Store exposes only Append/Journal/snapshots, and no UPDATE or
// DELETE SQL exists anywhere in the accounting tree. But that is a property of
// the CURRENT SHAPE of the Go code, not a guarantee. RLS does not help: it
// restricts which ROWS a role may touch, never which COMMAND it may issue, so a
// session correctly scoped to its own tenant could rewrite its own ledger
// history and RLS would permit every row.
//
// audit_log has been protected at the engine since AUDIT-01. These tests assert
// the ledger now is too, and they run the mutation for real rather than
// asserting the trigger merely exists -- a WORM guard that has never been fired
// is indistinguishable from one that does not work.
package ledger

import (
	"context"
	"strings"
	"testing"
	"time"
)

// appendOne inserts a single entry through the real Append path and returns it.
func appendOne(t *testing.T, st *Postgres) *Event {
	t.Helper()
	t0 := time.Unix(1_700_000_000, 0).UTC()
	e := tradeEvent("worm-1", "AAPL", 100, 150, -15000, t0, t0)
	if err := st.Append(context.Background(), e, nil); err != nil {
		t.Fatalf("append: %v", err)
	}
	return e
}

// An UPDATE against a committed ledger row must be refused by the ENGINE, not
// merely absent from the Go code. This is the correction path a well-meaning
// operator reaches for; a bitemporal ledger corrects by APPENDING at a later
// knowledge_time, never by rewriting what was believed earlier.
func TestLedgerEntries_RefusesUpdate(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	appendOne(t, st)

	_, err := pool.Exec(context.Background(),
		`UPDATE ledger_entries SET cash = '999999' WHERE entry_id = 'worm-1'`)
	if err == nil {
		t.Fatal("UPDATE on ledger_entries SUCCEEDED — the ledger is rewritable and its 'append-only journal' comment is false")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("UPDATE failed with %v — want the WORM rejection, not an unrelated error", err)
	}
}

// A DELETE must be refused for the same reason: destroying an entry destroys the
// audit trail the bitemporal model exists to preserve.
func TestLedgerEntries_RefusesDelete(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	appendOne(t, st)

	_, err := pool.Exec(context.Background(),
		`DELETE FROM ledger_entries WHERE entry_id = 'worm-1'`)
	if err == nil {
		t.Fatal("DELETE on ledger_entries SUCCEEDED — ledger history can be destroyed")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("DELETE failed with %v — want the WORM rejection, not an unrelated error", err)
	}
}

// NON-VACUITY: the guard must block MUTATION without blocking the ledger's own
// write path. A trigger that rejected everything would pass both tests above
// while having bricked the service.
func TestLedgerEntries_AppendStillWorksUnderWORM(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	ctx := context.Background()
	t0 := time.Unix(1_700_000_000, 0).UTC()

	if err := st.Append(ctx, tradeEvent("worm-a", "AAPL", 10, 100, -1000, t0, t0), nil); err != nil {
		t.Fatalf("append 1: %v", err)
	}
	// A second, later entry — the bitemporal CORRECTION path. This is how the
	// ledger is meant to be amended, and it must remain open.
	if err := st.Append(ctx, tradeEvent("worm-b", "AAPL", -10, 110, 1100,
		t0.Add(time.Hour), t0.Add(time.Hour)), nil); err != nil {
		t.Fatalf("append 2 (the correction path) rejected: %v — WORM must block rewrites, not appends", err)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM ledger_entries WHERE entry_id IN ('worm-a','worm-b')`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("rows = %d, want 2 — both appends must have landed", n)
	}
}
