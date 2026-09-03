package projection

import (
	"context"
	"fmt"
	"math/big"
	"math/rand"
	"reflect"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// THE MAINTAINED FOLD EQUALS THE FULL FOLD, ALWAYS (#995).
//
// This is the only property that matters. #995 replaced "re-derive the position
// book from the whole history on every read" with "keep the fold up to date as
// each execution lands", and the two are the same arithmetic ONLY while every
// execution reaches the maintained fold exactly once, in the order the full fold
// would visit it. A drift here is not a slow read — it is a trader shown a
// position and a realized P&L that the history does not support.
//
// Randomised over reversals, flats, re-crossings and several instruments,
// because those are where a cost-basis fold's order sensitivity lives: an
// average price is path-dependent, so an execution applied out of order or twice
// produces a number that still looks plausible.
func TestTheMaintainedFoldEqualsTheFullFold(t *testing.T) {
	const tenant, acctID = "acme", "fund-alpha"
	instruments := []string{"BTC", "ETH", "SOL"}

	rng := rand.New(rand.NewSource(1))
	p := New(time.Now, nil)

	for i := 0; i < 400; i++ {
		inst := instruments[rng.Intn(len(instruments))]
		side := orderpb.Side_SIDE_BUY
		if rng.Intn(2) == 1 {
			side = orderpb.Side_SIDE_SELL
		}
		qty := int64(1 + rng.Intn(5))
		price := int64(90 + rng.Intn(40))
		ev := filledOrder(fmt.Sprintf("o%d", i), acctID, inst, side, qty, price)
		if err := p.Handle(context.Background(), env(tenant, evtFilled), mustMarshal(t, ev)); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}

		// After EVERY fold, not only at the end: a divergence that self-corrects
		// would still have been served to a reader in between.
		a := p.acct(tenant, acctID, false)
		if a == nil {
			t.Fatalf("account vanished at fill %d", i)
		}
		assertFoldsAgree(t, i, a)
	}
}

// assertFoldsAgree compares the maintained fold against a fresh full fold of the
// retained history.
func assertFoldsAgree(t *testing.T, step int, a *account) {
	t.Helper()
	want := foldPositions(a.execs)
	if len(a.live) != len(want) {
		t.Fatalf("step %d: maintained fold covers %d instrument(s), the history folds to %d",
			step, len(a.live), len(want))
	}
	for inst, w := range want {
		got := a.live[inst]
		if got == nil {
			t.Fatalf("step %d: %s is in the history's fold and not in the maintained one", step, inst)
		}
		for _, f := range []struct {
			name      string
			got, want *big.Rat
		}{
			{"qty", got.Qty, w.Qty},
			{"avg cost", got.AvgCost, w.AvgCost},
			{"realized", got.Realized, w.Realized},
		} {
			if f.got.Cmp(f.want) != 0 {
				t.Fatalf("step %d: %s %s = %s, the history folds to %s — the maintained fold has "+
					"drifted from what the executions support (#995)",
					step, inst, f.name, f.got.RatString(), f.want.RatString())
			}
		}
	}
}

// A CHECKPOINTED BOOT REBUILDS THE MAINTAINED FOLD, against real Postgres.
//
// restore() replays the checkpointed history into a fresh account. Without
// rebuildLive the pod would serve an EMPTY position book off a FULL history —
// every execution present, every position gone. That is EXEC-M21's failure
// reached from the other direction, and it is invisible in every other field:
// orders, executions and the dedup set would all be correct.
//
// This runs against a real database rather than in memory because the defect
// only exists on the durable round trip — a pod that never restored would never
// show it, and an in-memory fake would prove the marshalling rather than the
// boot.
func TestACheckpointedBootRebuildsTheMaintainedFold(t *testing.T) {
	pool := newPool(t, testTenant)
	freshSchema(t, pool)
	ctx := context.Background()
	log := NewPostgresLog(pool)

	live := New(time.Now, nil, WithLog(log, testTenant))
	deliver(t, live, "evt-1", evtFilled, filledOrder("o1", "fund-alpha", "BTC", orderpb.Side_SIDE_BUY, 3, 100))
	deliver(t, live, "evt-2", evtFilled, filledOrder("o2", "fund-alpha", "BTC", orderpb.Side_SIDE_SELL, 1, 120))
	deliver(t, live, "evt-3", evtFilled, filledOrder("o3", "fund-alpha", "ETH", orderpb.Side_SIDE_BUY, 5, 50))
	if err := live.Checkpoint(ctx); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	// One fact AFTER the checkpoint, so the restored pod both restores and folds
	// a tail — the maintained fold has to survive being built by two mechanisms.
	deliver(t, live, "evt-4", evtFilled, filledOrder("o4", "fund-alpha", "BTC", orderpb.Side_SIDE_BUY, 2, 130))

	reborn := New(time.Now, nil, WithLog(log, testTenant))
	if err := reborn.Rehydrate(ctx); err != nil {
		t.Fatalf("rehydrate: %v", err)
	}
	if !reborn.BootedFromCheckpoint() {
		t.Fatal("the pod replayed everything instead of resuming from its checkpoint, so this test " +
			"never exercises restore() and proves nothing about the rebuilt fold (#995)")
	}

	a := reborn.acct(testTenant, "fund-alpha", false)
	if a == nil {
		t.Fatal("the rehydrated projection has no account")
	}
	if len(a.execs) == 0 {
		t.Fatal("the rehydrated account holds no history, so this test proves nothing about its fold")
	}
	if len(a.live) == 0 {
		t.Fatalf("the rehydrated account holds %d execution(s) and an EMPTY position fold — the pod "+
			"would serve a flat book off a full history, with orders and executions all correct "+
			"beside it (#995)", len(a.execs))
	}
	assertFoldsAgree(t, -1, a)

	// Against a FULL REPLAY, not against `live`. Knowledge time comes back from
	// a TIMESTAMPTZ column at microsecond resolution while the in-memory pod
	// still holds nanoseconds, so comparing the two would fail on the precision
	// asymmetry logTime exists for rather than on anything this test is about.
	full := New(time.Now, nil, WithLog(log, testTenant))
	if err := full.replayEverythingForTest(ctx); err != nil {
		t.Fatalf("full replay: %v", err)
	}
	assertSameView(t, full, reborn)
}

// A BITEMPORAL READ STILL WALKS THE HISTORY.
//
// The maintained fold answers "now". An as-of-knowledge read asks what we
// believed at a past instant, and only the history knows that — so it must NOT
// be served the live fold, which would report today's book for every past time
// and silently break the bitemporal contract this projection's package comment
// opens with.
func TestAnAsOfReadIsNotServedTheLiveFold(t *testing.T) {
	const tenant, acctID = "acme", "fund-alpha"
	clock := &fakeClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	p := New(clock.now, nil)

	first := filledOrder("o1", acctID, "BTC", orderpb.Side_SIDE_BUY, 2, 100)
	if err := p.Handle(context.Background(), env(tenant, evtFilled), mustMarshal(t, first)); err != nil {
		t.Fatalf("first fill: %v", err)
	}
	cut := clock.t
	clock.t = clock.t.Add(time.Hour)

	second := filledOrder("o2", acctID, "BTC", orderpb.Side_SIDE_BUY, 8, 100)
	if err := p.Handle(context.Background(), env(tenant, evtFilled), mustMarshal(t, second)); err != nil {
		t.Fatalf("second fill: %v", err)
	}

	now, _ := p.Positions(tenant, acctID, time.Time{})
	if len(now) != 1 || now[0].Qty != "10" {
		t.Fatalf("live positions = %+v, want qty 10", now)
	}
	asOf, _ := p.Positions(tenant, acctID, cut)
	if len(asOf) != 1 || asOf[0].Qty != "2" {
		t.Fatalf("as-of positions = %+v, want qty 2 — the as-of read was served the live fold, so "+
			"every historical question now answers with today's book (#995)", asOf)
	}
}

// THE LIVE READ RETURNS THE MAINTAINED FOLD ITSELF, NOT A REBUILD OF IT.
//
// Deterministic where a timing assertion would be flaky, and it closes the one
// gap the mutation harness found: re-deriving the live fold from history is
// SLOW BUT STILL CORRECT, so every correctness test in this package passes on it
// and the O(N) cost simply returns. Map identity is the property that separates
// them — a rebuild allocates a new map, the maintained fold is the same one.
//
// If foldFor ever has to hand out a mutable or filtered view for the live case,
// this test is the thing that should be changed deliberately rather than the
// assertion loosened.
func TestTheLiveReadDoesNotRebuildTheFold(t *testing.T) {
	const tenant, acctID = "acme", "fund-alpha"
	p := New(time.Now, nil)
	for i := 0; i < 5; i++ {
		ev := filledOrder(fmt.Sprintf("o%d", i), acctID, "BTC", orderpb.Side_SIDE_BUY, 1, 100)
		if err := p.Handle(context.Background(), env(tenant, evtFilled), mustMarshal(t, ev)); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}
	a := p.acct(tenant, acctID, false)
	if a == nil || len(a.live) == 0 {
		t.Fatal("no account or empty fold, so this test proves nothing")
	}

	got := p.foldFor(a, time.Time{})
	if reflect.ValueOf(got).Pointer() != reflect.ValueOf(a.live).Pointer() {
		t.Fatalf("the live read returned a DIFFERENT map from the maintained fold, so it re-derived " +
			"from history. That is still correct — which is why no other test here fails — and it " +
			"restores the O(N)-per-fill cost #995 removed: 208ms per fold at 100k executions, twice " +
			"per fill, under this projection's lock.")
	}

	// And an as-of read must NOT be the same map, or it is answering a
	// historical question with today's book.
	asOf := p.foldFor(a, time.Now().Add(-time.Hour))
	if reflect.ValueOf(asOf).Pointer() == reflect.ValueOf(a.live).Pointer() {
		t.Error("an as-of read returned the live fold itself")
	}
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

// BenchmarkAppendFillByHistoryDepth measures what ONE incoming fill costs as the
// account's history grows — the path #995 changed.
//
// Before: each fill published a position delta and a state delta, and each
// re-folded the whole history — 210ms per fold at 100k execs, twice. This should
// now be flat in history depth.
func BenchmarkAppendFillByHistoryDepth(b *testing.B) {
	for _, n := range []int{100, 1000, 10000, 100000} {
		b.Run(fmt.Sprintf("history=%d", n), func(b *testing.B) {
			p := New(time.Now, nil)
			a := p.acct("acme", "fund-alpha", true)
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			for i := 0; i < n; i++ {
				side := orderpb.Side_SIDE_BUY
				if i%2 == 1 {
					side = orderpb.Side_SIDE_SELL
				}
				ex := execution{
					fillID: fmt.Sprintf("seed-%d", i), orderID: fmt.Sprintf("o-%d", i),
					instrument: "BTC-USD", venue: "BINANCE", side: side,
					qty: big.NewRat(1, 1), price: big.NewRat(int64(50000+i%100), 1),
					fee: big.NewRat(1, 100), effective: base, knowledge: base,
				}
				a.execs = append(a.execs, ex)
				applyExecution(a.livePos(ex.instrument), ex)
			}
			fill := &orderpb.Fill{
				FillId: "", OrderId: "o-live", InstrumentId: "BTC-USD",
				Side: orderpb.Side_SIDE_BUY, Quantity: d(1), Price: d(50000), Venue: "BINANCE",
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = p.appendFill("acme", "fund-alpha", fill, base)
			}
		})
	}
}
