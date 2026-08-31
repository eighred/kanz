package orderview

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/execution"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// The eviction contract for Memory.orders (#891).
//
// The map was keyed by order id and nothing ever removed an entry: Open FILTERED
// terminal orders out of its return value, which is what made the growth
// invisible rather than absent. These tests hold the repair to both halves of
// the bound — that a terminal order LEAVES, and that a working one never does,
// because the second is what the healing watchdog reconciles against.
//
// Time is injected rather than slept: the retention is a full reconciliation
// pass, and a test that waited for it would take a minute per case.

// fakeClock is a hand-advanced time source.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

// memoryAt returns a Memory on a hand-advanced clock, with the production
// retention.
func memoryAt(clk *fakeClock) *Memory {
	m := NewMemory()
	m.now = clk.now
	m.lastSweep = clk.t
	return m
}

func working(id string) *orderpb.OrderState {
	return &orderpb.OrderState{OrderId: id, InstrumentId: "BTC-USD", Status: orderpb.OrderStatus_ORDER_STATUS_ROUTED}
}

func cancelled(id string) *orderpb.OrderState {
	return &orderpb.OrderState{OrderId: id, InstrumentId: "BTC-USD", Status: orderpb.OrderStatus_ORDER_STATUS_CANCELLED}
}

// TestTerminalOrdersDoNotAccumulate is the MEASUREMENT, not an assertion of
// intent: it records ten thousand orders that go terminal over a long run and
// counts what is left in the map. Before the evictor this count was 10000.
func TestTerminalOrdersDoNotAccumulate(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	m := memoryAt(clk)
	ctx := context.Background()

	const orders = 10_000
	// Spread the run over 100 retention windows — a process that has been up for
	// well over an hour at a sustained order rate, which is exactly the shape
	// that used to grow without bound.
	step := DefaultTerminalRetention / (orders / 100)
	for i := range orders {
		id := fmt.Sprintf("ord-%d", i)
		if err := m.Record(ctx, working(id)); err != nil {
			t.Fatalf("record working %s: %v", id, err)
		}
		if err := m.Record(ctx, cancelled(id)); err != nil {
			t.Fatalf("record terminal %s: %v", id, err)
		}
		clk.add(step)
	}

	// The bound is the terminal-order RATE over the retention window, not the
	// number of orders the process has worked. At this rate that is 100 orders
	// per window, and the amortized sweep lets an entry live up to 2 windows.
	const bound = 100 * 2 * 2 // rate/window * amortization * slack
	m.mu.RLock()
	got := len(m.orders)
	m.mu.RUnlock()
	if got > bound {
		t.Fatalf("Memory.orders holds %d entries after %d terminal orders, want <= %d — "+
			"the map is growing with the number of orders the process has ever worked, "+
			"which is the leak (#891)", got, orders, bound)
	}
	if got == 0 {
		t.Fatalf("Memory.orders is EMPTY after %d orders — eviction is not bounded by the "+
			"retention, it is dropping everything, and a late execution report would be "+
			"unenrichable for an order that just went terminal", orders)
	}
	t.Logf("%d orders worked, %d entries retained", orders, got)
}

// TestTerminalOrderSurvivesTheRetentionWindow is the other side of the same
// bound: an order that has JUST gone terminal must still enrich a fill report
// that arrives after it, which is the only read a terminal entry serves.
func TestTerminalOrderSurvivesTheRetentionWindow(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	m := memoryAt(clk)
	ctx := context.Background()

	if err := m.Record(ctx, cancelled("late-fill")); err != nil {
		t.Fatalf("record: %v", err)
	}
	// One close-timeout later — the watchdog's own window — and then most of a
	// retention: still readable.
	clk.add(execution.DefaultCloseTimeout)
	if err := m.Record(ctx, working("other")); err != nil { // drive the sweep
		t.Fatalf("record other: %v", err)
	}
	if _, ok, err := m.Get(ctx, "late-fill"); err != nil || !ok {
		t.Fatalf("Get(late-fill) after one close timeout = (%v, %v), want found — a report for a "+
			"trade that raced the cancel would be dropped unenriched", ok, err)
	}
	clk.add(DefaultTerminalRetention - execution.DefaultCloseTimeout - time.Second)
	if err := m.Record(ctx, working("other2")); err != nil {
		t.Fatalf("record other2: %v", err)
	}
	if _, ok, err := m.Get(ctx, "late-fill"); err != nil || !ok {
		t.Fatalf("Get(late-fill) inside the retention window = (%v, %v), want found", ok, err)
	}
}

// TestAWorkingOrderIsNeverEvicted is the property most likely to be broken
// silently by an evictor, and it is the one the healing watchdog depends on:
// Open is its ExpectedOrders read, and an order missing from it is one the
// reconciler will never query at the exchange or heal.
func TestAWorkingOrderIsNeverEvicted(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	m := memoryAt(clk)
	ctx := context.Background()

	if err := m.Record(ctx, working("resting")); err != nil {
		t.Fatalf("record: %v", err)
	}
	// A resting limit order can sit at the venue for days. Drive a thousand
	// retention windows of unrelated traffic past it.
	for i := range 1000 {
		clk.add(DefaultTerminalRetention)
		if err := m.Record(ctx, cancelled(fmt.Sprintf("noise-%d", i))); err != nil {
			t.Fatalf("record noise: %v", err)
		}
	}

	open, err := m.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(open) != 1 || open[0].GetOrderId() != "resting" {
		t.Fatalf("Open() = %v, want exactly the resting order — the healing watchdog reconciles "+
			"against this read, and an order it cannot see is one nothing at the exchange gets "+
			"queried or healed", open)
	}
	if _, ok, err := m.Get(ctx, "resting"); err != nil || !ok {
		t.Fatalf("Get(resting) = (%v, %v), want found", ok, err)
	}
}

// TestTerminalClockIsNotResetByARerecord: a redelivered close must not push the
// retention out. The reasoning is execution.CloseRegistry.Track's, which
// preserves its original RequestedAt for the same reason — a retry that resets
// the clock is a bound that never elapses under exactly the conditions (a
// redelivery storm) that make the bound matter.
func TestTerminalClockIsNotResetByARerecord(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	m := memoryAt(clk)
	ctx := context.Background()

	first := clk.t
	if err := m.Record(ctx, cancelled("redelivered")); err != nil {
		t.Fatalf("record: %v", err)
	}
	// Three redeliveries, none of them reaching the retention, so the entry is
	// still there and its clock is observable.
	for range 3 {
		clk.add(DefaultTerminalRetention / 8)
		if err := m.Record(ctx, cancelled("redelivered")); err != nil {
			t.Fatalf("re-record: %v", err)
		}
	}
	m.mu.RLock()
	e, ok := m.orders["redelivered"]
	m.mu.RUnlock()
	if !ok {
		t.Fatal("the re-recorded order was evicted inside its retention window")
	}
	if !e.terminalAt.Equal(first) {
		t.Fatalf("terminalAt = %v after 3 redeliveries, want the FIRST sighting %v — the "+
			"retention clock is being reset by the redelivery, so a redelivery storm on one "+
			"order holds its entry forever", e.terminalAt, first)
	}
}

// TestOpenIsUnchangedByEviction pins the claim DefaultTerminalRetention makes:
// the watchdog's read is bit-identical before and after a sweep, because Open
// never returned a terminal order in the first place.
func TestOpenIsUnchangedByEviction(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	m := memoryAt(clk)
	ctx := context.Background()

	for i := range 5 {
		if err := m.Record(ctx, working(fmt.Sprintf("open-%d", i))); err != nil {
			t.Fatalf("record: %v", err)
		}
		if err := m.Record(ctx, cancelled(fmt.Sprintf("done-%d", i))); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	before, err := m.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	clk.add(2 * DefaultTerminalRetention)
	if err := m.Record(ctx, working("trigger")); err != nil { // drives the sweep
		t.Fatalf("record trigger: %v", err)
	}
	after, err := m.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// "trigger" is itself open, so the set grows by exactly one.
	if len(after) != len(before)+1 {
		t.Fatalf("Open() = %d order(s) after the sweep, want %d — eviction changed what the "+
			"healing watchdog can see", len(after), len(before)+1)
	}
	m.mu.RLock()
	held := len(m.orders)
	m.mu.RUnlock()
	if held != len(after) {
		t.Fatalf("Memory.orders holds %d entries but Open() returns %d — the terminal orders "+
			"were not swept", held, len(after))
	}
}

// TestRecordStillRefusesAnEmptyOrderID guards the behaviour the rewrite passed
// through: an order with no id has no key and must be refused rather than
// recorded under "".
func TestRecordStillRefusesAnEmptyOrderID(t *testing.T) {
	m := NewMemory()
	if err := m.Record(context.Background(), &orderpb.OrderState{}); err == nil {
		t.Fatal("Record with an empty order_id returned nil, want an error")
	}
}

// TestRecordedStateIsCloned: the store must not alias the caller's proto, or a
// later mutation of the ExecuteRequest would rewrite history in the view.
func TestRecordedStateIsCloned(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	st := working("aliased")
	if err := m.Record(ctx, st); err != nil {
		t.Fatalf("record: %v", err)
	}
	st.Status = orderpb.OrderStatus_ORDER_STATUS_CANCELLED

	got, ok, err := m.Get(ctx, "aliased")
	if err != nil || !ok {
		t.Fatalf("Get = (%v, %v), want found", ok, err)
	}
	if got.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_ROUTED {
		t.Fatalf("stored status = %v, want ROUTED — the store aliased the caller's message", got.GetStatus())
	}
}
