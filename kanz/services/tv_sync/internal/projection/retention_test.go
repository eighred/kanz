package projection

// BOUNDING THE RESIDENT HISTORY MUST NOT CHANGE A NUMBER (#809).
//
// The heap half of #809 drops folded history out of memory. #988 refused the
// windowed rebuild the issue originally recommended because it would report the
// fund's positions and realized P&L since the window opened — EXEC-M21's defect
// with a shorter horizon, on the surface a trader acts from. This file holds the
// property that makes eviction a different thing from that window: what leaves is
// folded into the account's baseline, so every LIVE figure is bit-identical
// before and after, forever.
//
// The tests below are ordered by what they would cost if they failed:
//
//  1. a live figure moves when history is evicted — the fund's book, wrong;
//  2. an as-of read behind the horizon is ANSWERED — a plausible number for a
//     book that never existed, which is worse than the refusal;
//  3. a working order is evicted — an order resting at a venue vanishes from the
//     blotter while it can still fill;
//  4. a fill id outlives its execution or dies before it — a double-counted fill;
//  5. nothing is actually released — the leak, invisible instead of absent.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// foldHistory delivers n fills, one per hour, alternating buy and sell so the
// average-cost basis actually moves and a lost prefix cannot hide behind
// arithmetic that happens to commute.
func foldHistory(t *testing.T, p *Projection, clock *fakeClock, tenant, acct string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		side := orderpb.Side_SIDE_BUY
		if i%3 == 2 {
			side = orderpb.Side_SIDE_SELL
		}
		ev := filledOrder(fmt.Sprintf("o%d", i), acct, "BTC", side, int64(1+i%4), int64(100+i))
		// An event id on every FACT, so the same fixture drives a log-backed
		// projection (checkpoint_retention_test.go) as well as a memory-only one:
		// the durable log refuses a FACT it cannot record exactly once.
		e := env(tenant, evtFilled)
		e.EventId = fmt.Sprintf("evt-%s-%d", acct, i)
		if err := p.Handle(context.Background(), e, mustMarshal(t, ev)); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
		clock.t = clock.t.Add(time.Hour)
	}
}

// TestEvictionDoesNotMoveTheLiveBook is the property everything else rests on.
//
// Two projections fold the IDENTICAL history from the identical clock. One keeps
// everything; the other keeps six hours. Their live positions and their account
// state must agree exactly — same quantity, same average cost, same realized P&L,
// same open-position count. A difference here is the fund's book being wrong on
// the screen a trader reads.
func TestEvictionDoesNotMoveTheLiveBook(t *testing.T) {
	const tenant, acct = "acme", "fund-alpha"
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	kept := &fakeClock{t: start}
	full := New(kept.now, nil)
	foldHistory(t, full, kept, tenant, acct, 40)

	evicted := &fakeClock{t: start}
	bounded := New(evicted.now, nil, WithRetention(6*time.Hour))
	foldHistory(t, bounded, evicted, tenant, acct, 40)
	bounded.Evict(evicted.t)

	wantPos, err := full.Positions(tenant, acct, time.Time{})
	if err != nil {
		t.Fatalf("unbounded positions: %v", err)
	}
	gotPos, err := bounded.Positions(tenant, acct, time.Time{})
	if err != nil {
		t.Fatalf("bounded positions: %v", err)
	}
	if len(wantPos) != len(gotPos) {
		t.Fatalf("positions: unbounded %d, bounded %d", len(wantPos), len(gotPos))
	}
	for i := range wantPos {
		if wantPos[i] != gotPos[i] {
			t.Fatalf("retention moved the live position book.\n unbounded: %+v\n   bounded: %+v\n"+
				"Evicted history is supposed to be folded into the account baseline; if it is not, "+
				"the fund is shown a position and a realized P&L that start when the window does "+
				"— EXEC-M21 with a shorter horizon (#809)", wantPos[i], gotPos[i])
		}
	}

	wantSt, err := full.State(tenant, acct, time.Time{})
	if err != nil {
		t.Fatalf("unbounded state: %v", err)
	}
	gotSt, err := bounded.State(tenant, acct, time.Time{})
	if err != nil {
		t.Fatalf("bounded state: %v", err)
	}
	if *wantSt != *gotSt {
		t.Fatalf("retention moved the account state.\n unbounded: %+v\n   bounded: %+v", wantSt, gotSt)
	}

	// And the eviction must actually have happened, or this test proves nothing.
	if held := bounded.Resident(); held.Executions >= 40 {
		t.Fatalf("nothing was evicted (%d executions resident of 40) — this test would pass on a "+
			"projection that ignores retention entirely", held.Executions)
	}
}

// TestAnAsOfReadWithinTheWindowIsStillExact.
//
// The baseline answers "now". An as-of read inside the resident window has to
// fold the baseline PLUS the visible remainder — folding only the remainder
// reports the account flat in an instrument it has held all along, and the number
// is plausible, so nothing else catches it.
func TestAnAsOfReadWithinTheWindowIsStillExact(t *testing.T) {
	const tenant, acct = "acme", "fund-alpha"
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	kept := &fakeClock{t: start}
	full := New(kept.now, nil)
	foldHistory(t, full, kept, tenant, acct, 20)

	evicted := &fakeClock{t: start}
	bounded := New(evicted.now, nil, WithRetention(6*time.Hour))
	foldHistory(t, bounded, evicted, tenant, acct, 20)
	bounded.Evict(evicted.t)

	// Two hours inside the window: comfortably after the horizon, comfortably
	// before "now", so it is a real historical question rather than the live read
	// under another name.
	asOf := evicted.t.Add(-2 * time.Hour)

	want, err := full.Positions(tenant, acct, asOf)
	if err != nil {
		t.Fatalf("unbounded as-of: %v", err)
	}
	got, err := bounded.Positions(tenant, acct, asOf)
	if err != nil {
		t.Fatalf("bounded as-of read inside its own window was refused: %v", err)
	}
	if len(want) != len(got) {
		t.Fatalf("as-of positions: unbounded %d, bounded %d", len(want), len(got))
	}
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("an as-of read inside the resident window is not exact.\n unbounded: %+v\n"+
				"   bounded: %+v\nThe fold must start from the account baseline; starting from zero "+
				"reports the book since the window opened (#809)", want[i], got[i])
		}
	}
}

// TestAnAsOfReadBehindTheHorizonIsRefusedRatherThanAnswered.
//
// A fold of a partial history succeeds. It returns a position, an average cost
// and a realized P&L that look exactly like correct ones and describe a book the
// fund never had. That is the single worst outcome available here — worse than
// the refusal and worse than an outage — so the refusal is the behaviour under
// test, on every one of the four reads.
func TestAnAsOfReadBehindTheHorizonIsRefusedRatherThanAnswered(t *testing.T) {
	const tenant, acct = "acme", "fund-alpha"
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	clock := &fakeClock{t: start}
	p := New(clock.now, nil, WithRetention(6*time.Hour))
	foldHistory(t, p, clock, tenant, acct, 20)
	p.Evict(clock.t)

	from, ok := p.RetainedFrom(tenant, acct)
	if !ok || from.IsZero() {
		t.Fatal("nothing was evicted, so there is no horizon to read behind — the fixture is wrong, " +
			"not the code")
	}
	behind := from.Add(-time.Hour)

	reads := map[string]func() error{
		"Positions":  func() error { _, err := p.Positions(tenant, acct, behind); return err },
		"State":      func() error { _, err := p.State(tenant, acct, behind); return err },
		"Orders":     func() error { _, err := p.Orders(tenant, acct, behind); return err },
		"Executions": func() error { _, err := p.Executions(tenant, acct, "", behind); return err },
	}
	for name, read := range reads {
		if err := read(); !errors.Is(err, ErrBeforeRetention) {
			t.Errorf("%s answered an as-of read behind the resident horizon (err = %v). It folded "+
				"the surviving window into a number that looks correct and describes a book the fund "+
				"never had — the fact log still holds the answer, this process does not, and saying "+
				"so is the only honest reply (#809)", name, err)
		}
	}

	// The live read is unaffected: the horizon bounds historical questions, never
	// the current book.
	if _, err := p.Positions(tenant, acct, time.Time{}); err != nil {
		t.Fatalf("the LIVE read was refused (%v) — retention must never touch the current book", err)
	}
}

// TestAWorkingOrderIsNeverEvicted.
//
// An order resting at a venue can still fill. Dropping it because it is old
// removes it from the blotter an operator acts from while the fund's exposure is
// live — which is a capital-facing defect, not a memory saving. Only a terminal
// order (filled, cancelled, rejected, expired) emits no further FACT and can go.
func TestAWorkingOrderIsNeverEvicted(t *testing.T) {
	const tenant, acct = "acme", "fund-alpha"
	clock := &fakeClock{t: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}
	p := New(clock.now, nil, WithRetention(time.Hour))

	// One order accepted and routed — working, resting, never filled.
	working := &orderpb.OrderAccepted{State: &orderpb.OrderState{
		OrderId: "resting", PortfolioId: acct, InstrumentId: "BTC",
		Side: orderpb.Side_SIDE_BUY, OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT,
		OrderedQuantity: d(5), FilledQuantity: d(0), LeavesQuantity: d(5), LimitPrice: d(50),
		Status: orderpb.OrderStatus_ORDER_STATUS_ROUTED, AsOf: timestamppb.New(clock.t),
	}}
	if err := p.Handle(context.Background(), env(tenant, evtAccepted), mustMarshal(t, working)); err != nil {
		t.Fatalf("accept: %v", err)
	}
	// And one that fills and is therefore terminal.
	if err := p.Handle(context.Background(), env(tenant, evtFilled),
		mustMarshal(t, filledOrder("done", acct, "BTC", orderpb.Side_SIDE_BUY, 1, 100))); err != nil {
		t.Fatalf("fill: %v", err)
	}

	clock.t = clock.t.Add(72 * time.Hour) // both far outside a one-hour window
	p.Evict(clock.t)

	ords, err := p.Orders(tenant, acct, time.Time{})
	if err != nil {
		t.Fatalf("orders: %v", err)
	}
	if len(ords) != 1 || ords[0].OrderID != "resting" {
		t.Fatalf("orders after eviction = %+v; want exactly the WORKING order. A resting order "+
			"dropped for age disappears from the blotter while it can still fill, and a terminal "+
			"one kept forever is the leak (#809)", ords)
	}
}

// TestTheFillDedupSurvivesExactlyAsLongAsItsExecution.
//
// seenFills is the dedup for the DUAL fill path — the synchronous venue response
// and the asynchronous user-data websocket echo of the SAME fill, published as
// two FACTs with two event_ids, which tv_facts's (tenant_id, event_id) key does
// NOT deduplicate. An id dropped before its echo arrives doubles a position the
// fund does not hold. Tying it to the execution is what makes the dedup horizon
// the retention window; this holds both halves of that.
func TestTheFillDedupSurvivesExactlyAsLongAsItsExecution(t *testing.T) {
	const tenant, acct = "acme", "fund-alpha"
	clock := &fakeClock{t: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}
	p := New(clock.now, nil, WithRetention(6*time.Hour))

	ev := filledOrder("o1", acct, "BTC", orderpb.Side_SIDE_BUY, 2, 100)
	if err := p.Handle(context.Background(), env(tenant, evtFilled), mustMarshal(t, ev)); err != nil {
		t.Fatalf("first: %v", err)
	}

	// An hour later — well inside the window — the echo of the same fill arrives.
	clock.t = clock.t.Add(time.Hour)
	p.Evict(clock.t)
	if err := p.Handle(context.Background(), env(tenant, evtFilled), mustMarshal(t, ev)); err != nil {
		t.Fatalf("echo: %v", err)
	}

	pos, err := p.Positions(tenant, acct, time.Time{})
	if err != nil {
		t.Fatalf("positions: %v", err)
	}
	if len(pos) != 1 || pos[0].Qty != "2" {
		t.Fatalf("position = %+v, want qty 2. The echoed fill was folded a second time: the fill-id "+
			"dedup was evicted while its execution was still resident, which doubles a position the "+
			"fund does not hold (#809)", pos)
	}

	// And the other half: once the execution goes, its id goes with it. Holding
	// the id after the execution would be the leak this whole file is about,
	// growing one string per fill forever.
	clock.t = clock.t.Add(24 * time.Hour)
	p.Evict(clock.t)
	if held := p.Resident(); held.Fills != 0 || held.Executions != 0 {
		t.Fatalf("resident after the window passed = %+v, want zero executions and zero fill ids — "+
			"the dedup set must leave with the executions it belongs to, not outlive them", held)
	}
}

// TestTheResidentSetIsFlatAgainstTotalHistory is #809's own "Verified when",
// expressed as a test rather than as a thing to check on a dashboard.
//
// Fold ten times more history than the window holds and the resident set must not
// grow with it. The comparison is against a second projection folding a tenth of
// the history through the same window: same window, same rate, same answer.
func TestTheResidentSetIsFlatAgainstTotalHistory(t *testing.T) {
	const tenant, acct = "acme", "fund-alpha"
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	measure := func(n int) Resident {
		clock := &fakeClock{t: start}
		p := New(clock.now, nil, WithRetention(6*time.Hour))
		for i := 0; i < n; i++ {
			ev := filledOrder(fmt.Sprintf("o%d", i), acct, "BTC", orderpb.Side_SIDE_BUY, 1, 100)
			if err := p.Handle(context.Background(), env(tenant, evtFilled), mustMarshal(t, ev)); err != nil {
				t.Fatalf("fill %d: %v", i, err)
			}
			clock.t = clock.t.Add(time.Hour)
			p.Evict(clock.t)
		}
		return p.Resident()
	}

	short, long := measure(20), measure(200)
	if short != long {
		t.Fatalf("the resident set grew with total history: 20 fills held %+v, 200 fills held %+v.\n"+
			"#809's 'Verified when' is that the resident set is flat against total history and "+
			"proportional to the configured window — a line that tracks lifetime volume is the leak "+
			"still present", short, long)
	}
	if long.Executions == 0 {
		t.Fatal("nothing at all is resident, so this test would pass on a projection that forgets " +
			"the book — the window must hold the recent working set")
	}
}

// TestEvictionReleasesTheBackingArray.
//
// a.execs[n:] is the obvious way to drop a prefix and it keeps the original array
// — every evicted execution still reachable — so the heap would not shrink and
// the leak would be INVISIBLE rather than absent. Capacity is the only observable
// difference between the two, which is why it is asserted directly.
func TestEvictionReleasesTheBackingArray(t *testing.T) {
	const tenant, acct = "acme", "fund-alpha"
	clock := &fakeClock{t: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}
	p := New(clock.now, nil, WithRetention(6*time.Hour))
	foldHistory(t, p, clock, tenant, acct, 60)
	p.Evict(clock.t)

	a := p.acct(tenant, acct, false)
	if a == nil {
		t.Fatal("account vanished")
	}
	if cap(a.execs) > 2*len(a.execs)+1 {
		t.Fatalf("execution slice holds len=%d cap=%d after eviction — the evicted prefix is still "+
			"reachable through the original backing array, so the resident set shrank on paper and "+
			"not in the heap (#809)", len(a.execs), cap(a.execs))
	}
}

// TestAZeroWindowKeepsEverything.
//
// Retention is opt-in at the type level and required at the composition root:
// config.Load refuses a non-positive TV_SYNC_RETENTION, and every memory-only
// caller (including every other test in this package) gets the pre-#809
// behaviour. A projection that quietly windowed itself would change what those
// callers mean.
func TestAZeroWindowKeepsEverything(t *testing.T) {
	const tenant, acct = "acme", "fund-alpha"
	clock := &fakeClock{t: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}
	p := New(clock.now, nil)
	foldHistory(t, p, clock, tenant, acct, 30)

	clock.t = clock.t.Add(10 * 365 * 24 * time.Hour)
	if st := p.Evict(clock.t); st != (EvictionStats{}) {
		t.Fatalf("a projection with no window evicted %+v", st)
	}
	if held := p.Resident(); held.Executions != 30 {
		t.Fatalf("resident executions = %d, want all 30", held.Executions)
	}
	if from, _ := p.RetainedFrom(tenant, acct); !from.IsZero() {
		t.Fatalf("RetainedFrom = %s on a projection that has forgotten nothing; a non-zero horizon "+
			"makes the Broker API declare a window that does not exist", from)
	}
}
