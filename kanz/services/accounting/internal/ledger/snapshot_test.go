// Ledger checkpointing (#229).
//
// The defect these cover: MaterializeCurrent called Journal — the whole
// lifetime journal, unbounded, no LIMIT — BEFORE loading the snapshot, so the
// checkpoint bounded only the in-memory fold and never the query. And nothing
// ever wrote a checkpoint: SaveSnapshot had no production caller, so
// ledger_snapshots was permanently empty and the unbounded path was the ONLY
// path.
//
// Every test here therefore asserts on WHICH READ HAPPENED, not just on the
// book that came out. A book folded from a full scan is numerically identical
// to one folded from a checkpoint — that is the whole reason the original
// defect was invisible, and an assertion that only compares books would have
// passed against the broken code.
//
// These run without Postgres. What they cannot prove is the SQL: that
// JournalSince is a range scan rather than a filtered full scan is a property
// of the index in 0005, asserted in postgres_test.go and only executed where
// TEST_POSTGRES_URL is set.
package ledger

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"testing"
	"time"
)

// countingStore records which read each materialization actually performed.
type countingStore struct {
	Store
	journalCalls      int // unbounded whole-journal reads
	journalRows       int // rows those reads returned
	journalSinceCalls int // bounded tail reads
	journalSinceRows  int
	saveErr           error // when non-nil, every SaveSnapshot fails
	saveCalls         int
}

func (c *countingStore) Journal(ctx context.Context, id string) ([]*Event, error) {
	out, err := c.Store.Journal(ctx, id)
	c.journalCalls++
	c.journalRows += len(out)
	return out, err
}

func (c *countingStore) JournalSince(ctx context.Context, id string, after time.Time) ([]*Event, error) {
	out, err := c.Store.JournalSince(ctx, id, after)
	c.journalSinceCalls++
	c.journalSinceRows += len(out)
	return out, err
}

func (c *countingStore) SaveSnapshot(ctx context.Context, s *Snapshot) error {
	c.saveCalls++
	if c.saveErr != nil {
		return c.saveErr
	}
	return c.Store.SaveSnapshot(ctx, s)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// seed appends n trades an hour apart, both axes advancing together.
func seed(t *testing.T, st Store, portfolio string, n int) []*Event {
	t.Helper()
	t0 := time.Unix(1_700_000_000, 0).UTC()
	out := make([]*Event, 0, n)
	for i := range n {
		at := t0.Add(time.Duration(i) * time.Hour)
		e := &Event{
			EntryID:      portfolio + "-" + string(rune('a'+i%26)) + time.Duration(i).String(),
			PortfolioID:  portfolio,
			Type:         EntryTrade,
			InstrumentID: "AAPL",
			Quantity:     big.NewRat(10, 1),
			Price:        big.NewRat(100, 1),
			Cash:         big.NewRat(-1000, 1),
			CashCurrency: "USD",
			Effective:    at,
			Knowledge:    at,
		}
		if err := st.Append(context.Background(), e, nil); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		out = append(out, e)
	}
	return out
}

// THE DEFECT ITSELF. With a checkpoint in place, materializing must read ONLY
// the tail. Against the pre-#229 code this fails on the first assertion:
// Journal was called unconditionally, first, before the snapshot was even
// loaded.
func TestMaterializeCurrentReadsOnlyTheTail(t *testing.T) {
	ctx := context.Background()
	mem := NewMemoryStore()
	st := &countingStore{Store: mem}
	events := seed(t, st, "PF", 20)
	st.journalCalls, st.journalRows = 0, 0 // seeding is not a read

	// Checkpoint through the 15th entry; 5 remain in the tail.
	snap := Replay("PF", events[:15]).Snapshot(events[14].Knowledge)
	if err := st.SaveSnapshot(ctx, snap); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}

	book, reason, err := MaterializeCurrent(ctx, st, "PF")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if reason != "" {
		t.Fatalf("reason = %q, want the bounded path", reason)
	}
	if st.journalCalls != 0 {
		t.Fatalf("Journal (the UNBOUNDED whole-journal read) was called %d times with a checkpoint "+
			"present — this is #229: the snapshot bounds the fold but not the query", st.journalCalls)
	}
	if st.journalSinceCalls != 1 {
		t.Fatalf("JournalSince calls = %d, want exactly 1", st.journalSinceCalls)
	}
	if st.journalSinceRows != 5 {
		t.Fatalf("tail read returned %d rows, want 5 — the read must be proportional to the TAIL, "+
			"not to the 20-entry lifetime journal", st.journalSinceRows)
	}
	// And it is still the right book.
	full := Replay("PF", events)
	if book.Positions["AAPL"].Qty.Cmp(full.Positions["AAPL"].Qty) != 0 {
		t.Fatalf("bounded qty %v != full replay %v", book.Positions["AAPL"].Qty, full.Positions["AAPL"].Qty)
	}
	if book.CashBalance("USD").Cmp(full.CashBalance("USD")) != 0 {
		t.Fatalf("bounded cash %v != full replay %v", book.CashBalance("USD"), full.CashBalance("USD"))
	}
}

// With no checkpoint the full read is still correct — and it SAYS SO, which is
// the difference between the fallback and the defect. Before #229 this path was
// taken on every request in silence.
func TestMaterializeCurrentWithoutSnapshotReportsTheFullScan(t *testing.T) {
	ctx := context.Background()
	st := &countingStore{Store: NewMemoryStore()}
	events := seed(t, st, "PF", 6)
	st.journalCalls = 0

	book, reason, err := MaterializeCurrent(ctx, st, "PF")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if reason != FullScanNoSnapshot {
		t.Fatalf("reason = %q, want %q", reason, FullScanNoSnapshot)
	}
	if st.journalCalls != 1 {
		t.Fatalf("Journal calls = %d, want 1", st.journalCalls)
	}
	if book.Positions["AAPL"].Qty.Cmp(Replay("PF", events).Positions["AAPL"].Qty) != 0 {
		t.Fatal("fallback produced the wrong book")
	}
}

// A BACKDATED TAIL MUST NOT BE FOLDED ONTO A CHECKPOINT.
//
// The fold is order-sensitive (weighted-average cost realizes P&L in sequence,
// ordered by effective_time). A checkpoint is ordered by KNOWLEDGE, so a
// late-reported entry effective before the checkpoint's fence would be folded
// LAST where a replay folds it in the middle — a different realized P&L and a
// different NAV. This is only reachable now that checkpoints are actually
// written, so it ships with them.
func TestMaterializeCurrentRefusesABackdatedTail(t *testing.T) {
	ctx := context.Background()
	mem := NewMemoryStore()
	st := &countingStore{Store: mem}
	t0 := time.Unix(1_700_000_000, 0).UTC()

	buy := &Event{EntryID: "buy", PortfolioID: "PF", Type: EntryTrade, InstrumentID: "AAPL",
		Quantity: big.NewRat(100, 1), Price: big.NewRat(10, 1), Cash: big.NewRat(-1000, 1), CashCurrency: "USD",
		Effective: t0.Add(2 * time.Hour), Knowledge: t0.Add(2 * time.Hour)}
	sell := &Event{EntryID: "sell", PortfolioID: "PF", Type: EntryTrade, InstrumentID: "AAPL",
		Quantity: big.NewRat(-50, 1), Price: big.NewRat(20, 1), Cash: big.NewRat(1000, 1), CashCurrency: "USD",
		Effective: t0.Add(3 * time.Hour), Knowledge: t0.Add(3 * time.Hour)}
	for _, e := range []*Event{buy, sell} {
		if err := st.Append(ctx, e, nil); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	snap := Replay("PF", []*Event{buy, sell}).Snapshot(sell.Knowledge)
	if err := st.SaveSnapshot(ctx, snap); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}

	// A fill that HAPPENED first but was learned of last: effective before the
	// fence, knowledge after the watermark.
	backdated := &Event{EntryID: "late", PortfolioID: "PF", Type: EntryTrade, InstrumentID: "AAPL",
		Quantity: big.NewRat(100, 1), Price: big.NewRat(5, 1), Cash: big.NewRat(-500, 1), CashCurrency: "USD",
		Effective: t0, Knowledge: t0.Add(9 * time.Hour)}
	if err := st.Append(ctx, backdated, nil); err != nil {
		t.Fatalf("append backdated: %v", err)
	}

	st.journalCalls = 0
	book, reason, err := MaterializeCurrent(ctx, st, "PF")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if reason != FullScanBackdatedTail {
		t.Fatalf("reason = %q, want %q — a backdated tail entry cannot be folded onto a "+
			"knowledge-ordered checkpoint", reason, FullScanBackdatedTail)
	}
	if st.journalCalls != 1 {
		t.Fatalf("Journal calls = %d, want 1 (the replay fallback)", st.journalCalls)
	}
	// NON-VACUITY: the fallback must actually equal the replay, including the
	// realized P&L that the naive resume would have gotten wrong.
	full := Replay("PF", []*Event{buy, sell, backdated})
	if book.Positions["AAPL"].Realized.Cmp(full.Positions["AAPL"].Realized) != 0 {
		t.Fatalf("realized %v != full replay %v", book.Positions["AAPL"].Realized, full.Positions["AAPL"].Realized)
	}
	// And the naive resume really would have differed — otherwise this test
	// proves nothing about why the fence exists.
	naive := RestoreBook(snap)
	naive.Apply(backdated)
	if naive.Positions["AAPL"].Realized.Cmp(full.Positions["AAPL"].Realized) == 0 {
		t.Fatal("folding the backdated entry onto the checkpoint matched the replay — this fixture no " +
			"longer exercises order sensitivity, so the fence is untested")
	}
}

// A checkpoint that does not state its fence is never resumed from, and cannot
// be stored in the first place.
func TestSnapshotWithoutFenceIsRefused(t *testing.T) {
	ctx := context.Background()
	mem := NewMemoryStore()
	err := mem.SaveSnapshot(ctx, &Snapshot{PortfolioID: "PF", Through: time.Unix(1, 0)})
	if err == nil {
		t.Fatal("SaveSnapshot accepted a snapshot with no MaxEffective fence — MaterializeCurrent " +
			"would never trust it, so storing it buys a permanent silent full scan")
	}

	// And if one somehow exists, the read path distrusts it.
	st := &countingStore{Store: mem}
	seed(t, st, "PF", 3)
	mem.snapshots["PF"] = &Snapshot{PortfolioID: "PF", Through: time.Unix(1<<40, 0)}
	st.journalCalls = 0
	if _, reason, err := MaterializeCurrent(ctx, st, "PF"); err != nil {
		t.Fatalf("materialize: %v", err)
	} else if reason != FullScanUnfencedSnapshot {
		t.Fatalf("reason = %q, want %q", reason, FullScanUnfencedSnapshot)
	}
	if st.journalCalls != 1 {
		t.Fatalf("Journal calls = %d, want 1 — an unfenced checkpoint must not be resumed from", st.journalCalls)
	}
}

// The write half: a pass finds the stale portfolio, checkpoints it, and the
// next read is bounded. This is the end-to-end shape that had NO production
// caller before #229.
func TestSnapshotterCheckpointsAndBoundsTheNextRead(t *testing.T) {
	ctx := context.Background()
	st := &countingStore{Store: NewMemoryStore()}
	seed(t, st, "PF", 10)

	s, err := NewSnapshotter(st, testLogger(), SnapshotterConfig{Interval: time.Minute, Batch: 8})
	if err != nil {
		t.Fatalf("new snapshotter: %v", err)
	}
	n, err := s.Once(ctx)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if n != 1 {
		t.Fatalf("wrote %d snapshots, want 1", n)
	}

	st.journalCalls, st.journalSinceCalls = 0, 0
	if _, reason, err := MaterializeCurrent(ctx, st, "PF"); err != nil {
		t.Fatalf("materialize: %v", err)
	} else if reason != "" {
		t.Fatalf("reason = %q — the checkpoint the snapshotter just wrote was not used", reason)
	}
	if st.journalCalls != 0 {
		t.Fatalf("Journal calls = %d, want 0", st.journalCalls)
	}

	// A portfolio with nothing new is no longer stale, so the next pass is free.
	stale, err := st.StalePortfolios(ctx, 8)
	if err != nil {
		t.Fatalf("stale: %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("stale = %v, want empty after checkpointing", stale)
	}
}

// A FAILED CHECKPOINT WRITE MUST BE LOUD AND MUST NOT CORRUPT THE READ.
//
// The dangerous outcome is not the failure, it is the silence: reads stay
// correct and merely fall back to the full journal, which is indistinguishable
// from a healthy service until the journal is large. Once must surface the
// error and count nothing as written.
func TestSnapshotterSurfacesWriteFailures(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("pool exhausted")
	st := &countingStore{Store: NewMemoryStore(), saveErr: boom}
	events := seed(t, st, "PF", 5)

	s, err := NewSnapshotter(st, testLogger(), SnapshotterConfig{Interval: time.Minute, Batch: 8})
	if err != nil {
		t.Fatalf("new snapshotter: %v", err)
	}
	n, err := s.Once(ctx)
	if !errors.Is(err, boom) {
		t.Fatalf("pass err = %v, want the write failure surfaced — a checkpoint job that swallows "+
			"its own failures reinstates #229 invisibly", err)
	}
	if n != 0 {
		t.Fatalf("reported %d writes despite the failure", n)
	}
	if st.saveCalls != 1 {
		t.Fatalf("SaveSnapshot calls = %d, want 1", st.saveCalls)
	}

	// The read is still CORRECT — it just costs the whole journal, and says so.
	book, reason, err := MaterializeCurrent(ctx, st, "PF")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if reason != FullScanNoSnapshot {
		t.Fatalf("reason = %q, want %q", reason, FullScanNoSnapshot)
	}
	if book.Positions["AAPL"].Qty.Cmp(Replay("PF", events).Positions["AAPL"].Qty) != 0 {
		t.Fatal("a failed checkpoint changed the answer — it must only change the cost")
	}
}

// One bad portfolio must not stop the rest of the pass.
func TestSnapshotterContinuesPastOneBadPortfolio(t *testing.T) {
	ctx := context.Background()
	mem := NewMemoryStore()
	seed(t, mem, "GOOD-1", 3)
	seed(t, mem, "BAD", 3)
	seed(t, mem, "GOOD-2", 3)

	st := &failOneStore{Store: mem, bad: "BAD"}
	s, err := NewSnapshotter(st, testLogger(), SnapshotterConfig{Interval: time.Minute, Batch: 8})
	if err != nil {
		t.Fatalf("new snapshotter: %v", err)
	}
	n, err := s.Once(ctx)
	if err == nil {
		t.Fatal("pass reported success despite a failing portfolio")
	}
	if n != 2 {
		t.Fatalf("wrote %d, want 2 — one failure must not abandon the other portfolios", n)
	}
}

type failOneStore struct {
	Store
	bad string
}

func (f *failOneStore) SaveSnapshot(ctx context.Context, s *Snapshot) error {
	if s.PortfolioID == f.bad {
		return errors.New("nope")
	}
	return f.Store.SaveSnapshot(ctx, s)
}

// The watermark must never move backwards: a stale checkpoint overwriting a
// newer one is not a wrong book, but it is an unbounded read that stops
// converging.
func TestSaveSnapshotWatermarkIsMonotonic(t *testing.T) {
	ctx := context.Background()
	mem := NewMemoryStore()
	events := seed(t, mem, "PF", 6)

	newer := Replay("PF", events).Snapshot(events[5].Knowledge)
	older := Replay("PF", events[:2]).Snapshot(events[1].Knowledge)
	if err := mem.SaveSnapshot(ctx, newer); err != nil {
		t.Fatalf("save newer: %v", err)
	}
	if err := mem.SaveSnapshot(ctx, older); err != nil {
		t.Fatalf("save older: %v", err)
	}
	got, err := mem.LoadSnapshot(ctx, "PF")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !got.Through.Equal(newer.Through) {
		t.Fatalf("watermark = %s, want it held at %s — a stale write moved it backwards",
			got.Through, newer.Through)
	}
}

// A misconfigured snapshotter must refuse to start rather than run as a no-op:
// a checkpoint job that silently does nothing is the original defect wearing
// the fix's name.
func TestNewSnapshotterRejectsAConfigurationThatWouldDoNothing(t *testing.T) {
	for name, cfg := range map[string]SnapshotterConfig{
		"zero interval":     {Interval: 0, Batch: 8},
		"negative interval": {Interval: -time.Second, Batch: 8},
		"zero batch":        {Interval: time.Minute, Batch: 0},
		"negative batch":    {Interval: time.Minute, Batch: -1},
	} {
		if _, err := NewSnapshotter(NewMemoryStore(), testLogger(), cfg); err == nil {
			t.Errorf("%s: accepted, want refused", name)
		}
	}
	if _, err := NewSnapshotter(NewMemoryStore(), nil, SnapshotterConfig{Interval: time.Minute, Batch: 1}); err == nil {
		t.Error("nil logger accepted — the job's whole failure mode is silence")
	}
}

// StalePortfolios is the queue: only portfolios past their watermark, bounded.
func TestStalePortfoliosIsTheUnsnapshottedSet(t *testing.T) {
	ctx := context.Background()
	mem := NewMemoryStore()
	a := seed(t, mem, "A", 3)
	seed(t, mem, "B", 3)
	seed(t, mem, "C", 3)

	if err := mem.SaveSnapshot(ctx, Replay("A", a).Snapshot(a[2].Knowledge)); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := mem.StalePortfolios(ctx, 10)
	if err != nil {
		t.Fatalf("stale: %v", err)
	}
	if len(got) != 2 || got[0] != "B" || got[1] != "C" {
		t.Fatalf("stale = %v, want [B C] — A is fully checkpointed", got)
	}
	if limited, err := mem.StalePortfolios(ctx, 1); err != nil || len(limited) != 1 {
		t.Fatalf("limited = %v (err %v), want exactly 1", limited, err)
	}
}

// Run stops on cancellation rather than outliving the process it is joined to.
func TestSnapshotterRunStopsOnCancel(t *testing.T) {
	s, err := NewSnapshotter(NewMemoryStore(), testLogger(), SnapshotterConfig{
		Interval: time.Millisecond, Batch: 4,
	})
	if err != nil {
		t.Fatalf("new snapshotter: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation — it would hold the shutdown join budget")
	}
}
