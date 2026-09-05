package ledger

// Postgres-gated proof of the settlement axis (#1043). Gated on
// TEST_POSTGRES_URL like every other durable ledger test: the DB-free properties
// are in settlement_test.go, and these prove the half that only a real engine can
// — that migration 0008's columns exist, that Append writes them, that the
// journal read brings them back, and that a checkpoint written before the axis
// existed comes back saying so rather than looking like a settled-nothing book.
//
// WHY THIS CANNOT BE PROVEN IN MEMORY. MemoryStore hands back the very *Event
// pointer it was given, so a settlement basis that never reached a column, and a
// settled fold that was never encoded into JSONB, both round-trip perfectly there.
// The one defect this file can catch is precisely the one that seam cannot see.

import (
	"context"
	"math/big"
	"testing"
	"time"
)

// THE AXIS MUST SURVIVE THE JOURNAL. A basis written and not read back is worse
// than one never written: every *Event reconstructed from Postgres would carry
// the zero value, which is SettlementUnknown — so a settled fill would come back
// as "nobody said", the settled book would empty itself on every restart, and
// SettlementBasisComplete would report a gap that does not exist. That is the
// exact shape of #415's venue_account_id defect, one column over, which is why
// journal_columns_test.go exists beside this.
func TestPostgresJournalRoundTripsTheSettlementAxis(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	ctx := context.Background()

	eff := time.Now().UTC().Truncate(time.Millisecond)
	due := eff.Add(48 * time.Hour)

	settled := tradeEvent("stl:settled", "BTC-USD", 2, 100, -200, eff, eff)
	settled.SettlementBasis = SettlementSettled
	settled.SettlementDate = eff

	pending := tradeEvent("stl:pending", "ETH-USD", 3, 10, -30, eff, eff)
	pending.SettlementBasis = SettlementPending
	pending.SettlementDate = due

	// Unasserted: no basis, no date. It must come back that way rather than
	// acquiring one from a column default.
	unknown := tradeEvent("stl:unknown", "SOL-USD", 4, 5, -20, eff, eff)

	for _, e := range []*Event{settled, pending, unknown} {
		if err := st.Append(ctx, e, nil); err != nil {
			t.Fatalf("append %s: %v", e.EntryID, err)
		}
	}

	journal, err := st.Journal(ctx, "PORT-1")
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	got := map[string]*Event{}
	for _, e := range journal {
		got[e.EntryID] = e
	}

	for _, tc := range []struct {
		id    string
		basis SettlementBasis
		date  time.Time
	}{
		{"stl:settled", SettlementSettled, eff},
		{"stl:pending", SettlementPending, due},
		{"stl:unknown", SettlementUnknown, time.Time{}},
	} {
		e, ok := got[tc.id]
		if !ok {
			t.Fatalf("%s is missing from the journal read", tc.id)
		}
		if e.SettlementBasis != tc.basis {
			t.Errorf("%s read back with basis %v, want %v — the settlement axis did not survive "+
				"the round trip, so every book rebuilt from this journal loses it", tc.id,
				e.SettlementBasis, tc.basis)
		}
		switch {
		case tc.date.IsZero() && !e.SettlementDate.IsZero():
			t.Errorf("%s asserted NO settlement date and read back %s — a date nobody established "+
				"is now in the book of record", tc.id, e.SettlementDate)
		case !tc.date.IsZero() && !e.SettlementDate.Equal(tc.date):
			t.Errorf("%s settlement date = %s, want %s", tc.id, e.SettlementDate, tc.date)
		}
	}

	// And the fold over what the ENGINE returned must agree with the fold over
	// what was written — the property the whole axis is for.
	book := Replay("PORT-1", journal)
	if _, ok := book.SettledPositions["ETH-USD"]; ok {
		t.Error("the PENDING holding is in the settled book after a Postgres round trip")
	}
	if p := book.SettledPositions["BTC-USD"]; p == nil || p.Qty.Cmp(big.NewRat(2, 1)) != 0 {
		t.Errorf("settled BTC-USD after a Postgres round trip = %v, want qty 2", p)
	}
	if book.SettlementBasisComplete() {
		t.Error("the replayed book claims a complete settled basis with an unasserted entry in " +
			"the journal")
	}
}

// THE CHECKPOINT MUST CARRY BOTH BASES THROUGH THE DATABASE. MaterializeCurrent
// restores from ledger_snapshots and folds only the tail, so a settled view that
// did not survive SaveSnapshot/LoadSnapshot produces a book whose settled
// balances are short by everything the checkpoint absorbed.
func TestPostgresSnapshotRoundTripsTheSettledFold(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	ctx := context.Background()

	eff := time.Now().UTC().Truncate(time.Millisecond)
	b := NewBook("PORT-SNAP")
	settled := tradeEvent("snap:settled", "BTC-USD", 2, 100, -200, eff, eff)
	settled.PortfolioID = "PORT-SNAP"
	settled.SettlementBasis = SettlementSettled
	settled.SettlementDate = eff
	b.Apply(settled)
	unknown := tradeEvent("snap:unknown", "ETH-USD", 5, 10, -50, eff, eff)
	unknown.PortfolioID = "PORT-SNAP"
	b.Apply(unknown)

	if err := st.SaveSnapshot(ctx, b.Snapshot(eff)); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
	loaded, err := st.LoadSnapshot(ctx, "PORT-SNAP")
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	if !loaded.SettlementStated {
		t.Fatal("a checkpoint written by a live book came back stating NO settled view — every " +
			"materialization from it would then refuse the settled read forever")
	}
	if got := loaded.SettledPositions["BTC-USD"]; got == nil || got.Qty.Cmp(big.NewRat(2, 1)) != 0 {
		t.Errorf("settled BTC-USD in the loaded checkpoint = %v, want qty 2", got)
	}
	if _, ok := loaded.SettledPositions["ETH-USD"]; ok {
		t.Error("the unasserted holding was persisted into the settled half of the checkpoint")
	}
	if got := loaded.SettledCash["USD"]; got == nil || got.Cmp(big.NewRat(-200, 1)) != 0 {
		t.Errorf("settled cash in the loaded checkpoint = %v, want -200", got)
	}
	if loaded.UnknownSettlement != 1 {
		t.Errorf("loaded UnknownSettlement = %d, want 1 — without the count a restart launders an "+
			"unknown into a clean bill of health", loaded.UnknownSettlement)
	}
	if RestoreBook(loaded).SettlementBasisComplete() {
		t.Error("a book restored from this checkpoint claims a complete settled basis")
	}
}

// A ROW WRITTEN BEFORE MIGRATION 0008 STATES NOTHING, AND MUST COME BACK SAYING
// SO. Its settled columns are NULL because nobody wrote them, not because nothing
// settled — and an empty settled map that looks like a computed answer is the
// wrong number this whole axis exists to prevent. Same stance
// 0005_ledger_snapshot_tail.sql's max_effective_time takes on a fenceless
// checkpoint.
func TestPostgresLoadSnapshotWrittenBeforeTheAxisStatesNoSettledView(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	ctx := context.Background()
	eff := time.Now().UTC().Truncate(time.Millisecond)

	// Written the way a pre-0008 build wrote one: the settlement columns untouched.
	if _, err := pool.Exec(ctx, `
		INSERT INTO ledger_snapshots
			(tenant_id, portfolio_id, positions, cash, accrued, through_time, max_effective_time)
		VALUES (current_setting('app.tenant_id'), $1, $2, $3, '{}', $4, $4)
		ON CONFLICT (tenant_id, portfolio_id) DO NOTHING
	`, "PORT-LEGACY",
		`{"BTC-USD":{"qty":"2","avg":"100","realized":"0"}}`,
		`{"USD":"-200"}`, eff); err != nil {
		t.Fatalf("insert legacy snapshot: %v", err)
	}

	loaded, err := st.LoadSnapshot(ctx, "PORT-LEGACY")
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	if loaded.SettlementStated {
		t.Fatal("a checkpoint with NULL settled columns came back claiming to state a settled " +
			"view. Every control reading the restored book would be told the portfolio has " +
			"settled nothing, with the same confidence as a real answer")
	}
	if loaded.SettledPositions == nil || loaded.SettledCash == nil {
		t.Fatal("LoadSnapshot returned nil settled maps — a caller folding a tail onto this " +
			"checkpoint would panic rather than fail closed")
	}
	if RestoreBook(loaded).SettlementBasisComplete() {
		t.Error("a book restored from a pre-0008 checkpoint reports a complete settled basis")
	}
	if got := RestoreBook(loaded).Positions["BTC-USD"]; got == nil || got.Qty.Cmp(big.NewRat(2, 1)) != 0 {
		t.Errorf("the traded fold must survive unchanged: %v", got)
	}
}
