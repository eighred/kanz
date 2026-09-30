package ledger

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/outbox"
)

func income(revision uint64, rate string, known int) *Event {
	return &Event{EntryID: ActionEntryID("PF", "div", revision), PortfolioID: "PF", Type: EntryCorporateAction, InstrumentID: "AAPL", Effective: day(2), Knowledge: day(known), SourceRef: "div",
		Action: &Action{ActionLifecycle: ActionLifecycle{ActionID: "div", Revision: revision, PayDate: day(23)}, Kind: CorpActDividend, PerUnit: dec.Rat(rate), Currency: "USD"}}
}

func assertIncome(t *testing.T, b *Book, cash, accrued, settled string) {
	t.Helper()
	for _, v := range []struct {
		name string
		got  *big.Rat
		want string
	}{
		{"cash", b.CashBalance("USD"), cash}, {"accrued", b.AccruedBalance("USD"), accrued}, {"settled", orZero(b.SettledCash["USD"]), settled},
	} {
		if v.got.Cmp(dec.Rat(v.want)) != 0 {
			t.Fatalf("%s = %s want %s", v.name, v.got, v.want)
		}
	}
}

func TestIncomeRevisionPITAndPaymentAfterDisposal(t *testing.T) {
	for _, sign := range []int64{1, -1} {
		hold := trade("buy", "AAPL", new(big.Int).Mul(big.NewInt(sign), big.NewInt(100)).String(), "10", 1, 1)
		// Isolate income from trade cash; positions are all this scenario needs.
		hold.Cash = nil
		sell := trade("dispose", "AAPL", new(big.Int).Mul(big.NewInt(sign), big.NewInt(-100)).String(), "10", 10, 10)
		sell.Cash = nil
		v1, v2, paid := income(1, "0.5", 2), income(2, "0.55", 5), income(3, "0.55", 24)
		paid.Action.PaidAt, paid.Action.PaymentRef = day(23), "custody-payment"
		journal := []*Event{paid, sell, v2, hold, v1, v1}
		amount := func(n int64) string { return big.NewInt(n * sign).String() }
		assertIncome(t, ReplayAsOf("PF", journal, day(22), day(3)), "0", amount(50), "0")
		assertIncome(t, ReplayAsOf("PF", journal, day(22), day(6)), "0", amount(55), "0")
		assertIncome(t, ReplayAsOf("PF", journal, day(25), day(22)), "0", amount(55), "0")
		assertIncome(t, ReplayAsOf("PF", journal, day(22), day(25)), "0", amount(55), "0")
		assertIncome(t, ReplayAsOf("PF", journal, day(25), day(25)), amount(55), "0", amount(55))
		// Scoped/PIT paths may already have staged the journal. Expansion must
		// be idempotent and must not create the payment a second time.
		assertIncome(t, Replay("PF", sortedFor(journal, day(25), day(25), true)), amount(55), "0", amount(55))
	}
}

func TestActionRevisionMovedDateAndCancellation(t *testing.T) {
	hold := trade("hold", "AAPL", "100", "10", 1, 1)
	hold.Cash = nil
	v1, v2, v3 := income(1, "1", 2), income(2, "2", 6), income(3, "2", 8)
	v2.Effective = day(12)
	v3.Action.Cancelled = true
	journal := []*Event{hold, v1, v2, v3}
	assertIncome(t, ReplayAsOf("PF", journal, day(4), day(4)), "0", "100", "0")
	assertIncome(t, ReplayAsOf("PF", journal, day(4), day(7)), "0", "0", "0")
	assertIncome(t, ReplayAsOf("PF", journal, day(15), day(7)), "0", "200", "0")
	assertIncome(t, ReplayAsOf("PF", journal, day(15), day(9)), "0", "0", "0")
}

func TestActionStoreLifecycle(t *testing.T) {
	t.Run("memory", func(t *testing.T) { exerciseActionStore(t, NewMemoryStore()) })
	t.Run("postgres", func(t *testing.T) { exerciseActionStore(t, NewPostgres(newPool(t))) })
}

func exerciseActionStore(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()
	hold := trade("hold", "AAPL", "100", "10", 1, 1)
	hold.Cash = nil
	appendEntry := func(e *Event) {
		t.Helper()
		if err := st.Append(ctx, e, nil); err != nil {
			t.Fatal(err)
		}
	}
	appendEntry(hold)
	v1 := income(1, "0.5", 2)
	appendEntry(v1)
	book, _, err := MaterializeCurrent(ctx, st, "PF")
	if err != nil {
		t.Fatal(err)
	}
	assertIncome(t, book, "0", "50", "0")
	if err := st.SaveSnapshot(ctx, book.Snapshot(day(2))); err != nil {
		t.Fatal(err)
	}
	v2 := income(2, "0.55", 5)
	// Concurrent redelivery cannot either double-book or hide a conflict.
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := st.Append(ctx, v2, nil); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	appendEntry(v1) // old exact revision remains idempotent after a newer head
	conflict := income(2, "99", 5)
	if err := st.Append(ctx, conflict, nil); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("conflict: %v", err)
	}
	if err := st.Append(ctx, income(4, "1", 6), nil); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("gap: %v", err)
	}
	book, reason, err := MaterializeCurrent(ctx, st, "PF")
	if err != nil {
		t.Fatal(err)
	}
	if reason != FullScanActionRevision && reason != FullScanNoSnapshot && reason != FullScanUnfencedSnapshot {
		t.Fatalf("snapshot reused on revision: %s", reason)
	}
	assertIncome(t, book, "0", "55", "0")
	if err := st.SaveSnapshot(ctx, book.Snapshot(day(5))); err != nil {
		t.Fatal(err)
	}
	paid := income(3, "0.55", 24)
	paid.Action.PaidAt = day(23)
	paid.Action.PaymentRef = "confirmed"
	appendEntry(paid)
	book, _, err = MaterializeCurrent(ctx, st, "PF")
	if err != nil {
		t.Fatal(err)
	}
	assertIncome(t, book, "55", "0", "55")
	if err := st.SaveSnapshot(ctx, book.Snapshot(day(24))); err != nil {
		t.Fatal(err)
	}
	book, reason, err = MaterializeCurrent(ctx, st, "PF")
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" {
		t.Fatalf("rebuilt snapshot not reusable: %s", reason)
	}
	assertIncome(t, book, "55", "0", "55")
	journal, err := st.Journal(ctx, "PF")
	if err != nil {
		t.Fatal(err)
	}
	if len(journal) != 4 {
		t.Fatalf("immutable journal length %d", len(journal))
	}
	if pg, ok := st.(*Postgres); ok {
		for _, cut := range []struct {
			eff, know     int
			cash, accrued string
		}{{22, 24, "0", "55"}, {25, 3, "0", "50"}, {25, 25, "55", "0"}} {
			rows, err := pg.JournalAsOf(ctx, "PF", day(cut.eff), day(cut.know))
			if err != nil {
				t.Fatal(err)
			}
			assertIncome(t, Replay("PF", rows), cut.cash, cut.accrued, cut.cash)
		}
	}
}

func TestActionAppendInvalidatesBackdatedKnowledgeAtomically(t *testing.T) {
	for _, backend := range []string{"memory", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var st Store = NewMemoryStore()
			if backend == "postgres" {
				st = NewPostgres(newPool(t))
			}
			ctx := context.Background()
			hold := trade("hold", "AAPL", "100", "10", 1, 1)
			hold.Cash = nil
			sale := trade("sale", "AAPL", "-50", "10", 10, 10)
			sale.Cash = nil
			for _, e := range []*Event{hold, sale} {
				if err := st.Append(ctx, e, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.SaveSnapshot(ctx, Replay("PF", []*Event{hold, sale}).Snapshot(day(10))); err != nil {
				t.Fatal(err)
			}
			late := income(1, "1", 2)
			refused := errors.New("outbox transaction refused")
			if err := st.Append(ctx, late, func(ctx context.Context, staged Store) ([]outbox.Record, error) {
				book, _, err := MaterializeCurrent(ctx, staged, "PF")
				if err != nil {
					return nil, err
				}
				assertIncome(t, book, "0", "100", "0")
				return nil, refused
			}); !errors.Is(err, refused) {
				t.Fatalf("staged append: %v", err)
			}
			book, reason, err := MaterializeCurrent(ctx, st, "PF")
			if err != nil || reason != "" {
				t.Fatalf("rollback lost checkpoint: %s %v", reason, err)
			}
			assertIncome(t, book, "0", "0", "0")
			if err := st.Append(ctx, late, nil); err != nil {
				t.Fatal(err)
			}
			book, _, err = MaterializeCurrent(ctx, st, "PF")
			if err != nil {
				t.Fatal(err)
			}
			assertIncome(t, book, "0", "100", "0")
		})
	}
}

func TestActionValidation(t *testing.T) {
	for name, change := range map[string]func(*Event){
		"missing pay date":          func(e *Event) { e.Action.PayDate = time.Time{} },
		"payment before pay date":   func(e *Event) { e.Action.PaidAt = day(20); e.Action.PaymentRef = "x" },
		"unknown payment reference": func(e *Event) { e.Action.PaidAt = day(23) },
		"future payment claim":      func(e *Event) { e.Action.PaidAt = day(23); e.Action.PaymentRef = "x" },
		"missing periodic amount":   func(e *Event) { e.Action.PerUnit = nil },
		"negative periodic amount":  func(e *Event) { e.Action.PerUnit = big.NewRat(-1, 1) },
		"identity without revision": func(e *Event) { e.EntryID = "corpact:div" },
	} {
		t.Run(name, func(t *testing.T) {
			e := income(1, "1", 2)
			change(e)
			if err := ValidateActionEntry(e); err == nil {
				t.Fatal("accepted invalid action")
			}
		})
	}
}

func TestFutureExDateIsNotAccruedEarlyAcrossCheckpoint(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	hold := trade("hold", "AAPL", "100", "10", 1, 1)
	hold.Cash = nil
	announced := income(1, "1", 2)
	announced.Effective = day(10)
	for _, e := range []*Event{hold, announced} {
		if err := st.Append(ctx, e, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, checkpoint := range []bool{false, true} {
		if checkpoint {
			b := Replay("PF", []*Event{hold, announced})
			if err := st.SaveSnapshot(ctx, b.Snapshot(day(2))); err != nil {
				t.Fatal(err)
			}
		}
		for _, cut := range []struct {
			at      int
			accrued string
		}{{5, "0"}, {11, "100"}} {
			book, _, err := materializeCurrentAt(ctx, st, "PF", day(cut.at))
			if err != nil {
				t.Fatal(err)
			}
			assertIncome(t, book, "0", cut.accrued, "0")
		}
	}
}

func TestPostgresRetiresOldCorporateActionSnapshots(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	ctx := context.Background()
	hold := trade("hold", "AAPL", "100", "10", 1, 1)
	hold.Cash = nil
	events := []*Event{hold, income(1, "1", 2)}
	for _, e := range events {
		if err := st.Append(ctx, e, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SaveSnapshot(ctx, Replay("PF", events).Snapshot(day(2))); err != nil {
		t.Fatal(err)
	}
	// Model the old writer's cash classification, then read through a new store
	// instance. NULL version must be rejected even with no new journal entries.
	if _, err := pool.Exec(ctx, `UPDATE ledger_snapshots SET corporate_action_version=NULL, cash='{"USD":"100"}', accrued='{}' WHERE portfolio_id='PF'`); err != nil {
		t.Fatal(err)
	}
	book, reason, err := MaterializeCurrent(ctx, NewPostgres(pool), "PF")
	if err != nil {
		t.Fatal(err)
	}
	if reason != FullScanUnfencedSnapshot {
		t.Fatalf("legacy checkpoint trusted: %s", reason)
	}
	assertIncome(t, book, "0", "100", "0")
	stale, err := st.StalePortfolios(ctx, 10)
	if err != nil || len(stale) != 1 || stale[0] != "PF" {
		t.Fatalf("old checkpoint not queued for rebuild: %v %v", stale, err)
	}
}
