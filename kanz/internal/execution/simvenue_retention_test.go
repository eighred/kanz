package execution

// #895 — SimVenue's record of what it executed is bounded, and the bound must
// not be able to produce a second fill for one order.
//
// The OMS selects a SimVenue for every MIC whenever OMS_VENUE_ENDPOINTS is
// empty, so the process holding that record is a long-lived pod. Bounding it is
// straightforward; bounding it WITHOUT reintroducing the double fill it exists
// to prevent is the part these tests are about.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/bus"
)

// clock is a manually advanced time source. The retention is measured in tens of
// minutes, so expiry is not testable by sleeping — the orderview.Memory and
// bus.DedupWindow seam, for the same reason.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func newClock() *clock {
	return &clock{t: time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)}
}

// TestSimRetentionIsTheBrokerRedeliveryBudget pins the DERIVATION, not the
// number. The record has to outlive every redelivery of the submit command that
// could re-drive the order, and how long that is belongs to pkg/bus. If someone
// raises workTuning's delivery budget, this venue's memory has to move with it —
// a locally chosen duration would silently stop covering the window.
func TestSimRetentionIsTheBrokerRedeliveryBudget(t *testing.T) {
	if got, want := DefaultSimRetention(), bus.WorkRedeliveryBudget(); got != want {
		t.Fatalf("DefaultSimRetention()=%s, want bus.WorkRedeliveryBudget()=%s — the retention must be "+
			"derived from the redelivery contract, not chosen beside it", got, want)
	}
	// A floor on the derivation itself: a budget that collapsed to seconds would
	// satisfy the equality above and cover nothing.
	if got := DefaultSimRetention(); got < 30*time.Minute {
		t.Fatalf("DefaultSimRetention()=%s — too short to cover a work-class redelivery budget", got)
	}
}

// TestAReplayInsideTheRetentionDoesNotDoubleFill is the FIRST safety test.
//
// A submit command whose handler failed after venue.Execute returned is NAKed
// and redelivered, and the redelivery re-drives the same order. Within the
// retention the venue must hand back the FIRST execution's fills — same fill_id,
// one fill — because position_fills dedups on fill_id and a second identity is
// the same trade folded twice.
func TestAReplayInsideTheRetentionDoesNotDoubleFill(t *testing.T) {
	ctx := context.Background()
	clk := newClock()
	ids := 0
	v := NewSimVenue("XSIM",
		WithClock(clk.now),
		WithIDGen(func() string { ids++; return fmt.Sprintf("fill-%d", ids) }))
	st := simOrder("o-replayed")

	// A POD THAT HAS BEEN UP A WHILE, not a fresh one. The sweep is amortized off
	// the previous sweep, so an order executed immediately after construction can
	// only ever be swept at the exact instant it also expires — a test built that
	// way never runs a sweep while the order is still inside its window, and a
	// venue that dropped every record on sight would sail through it. That was
	// measured as a surviving mutation before this line existed.
	clk.add(DefaultSimRetention() * 9 / 10)

	first, err := v.Execute(ctx, st)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Every redelivery the broker can still make, spread across three quarters of
	// the budget, with LIVE ORDER FLOW alongside — which is what runs the sweep,
	// since the record only grows on the insert path.
	step := DefaultSimRetention() / 16
	for i := 0; i < 12; i++ {
		clk.add(step)
		if _, ferr := v.Execute(ctx, simOrder(fmt.Sprintf("o-fresh-%d", i))); ferr != nil {
			t.Fatalf("replay %d: fresh order: %v", i, ferr)
		}
		again, aerr := v.Execute(ctx, st)
		if aerr != nil {
			t.Fatalf("replay %d: Execute: %v", i, aerr)
		}
		if len(again) != 1 {
			t.Fatalf("replay %d returned %d fills, want 1", i, len(again))
		}
		if again[0].GetFillId() != first[0].GetFillId() {
			t.Fatalf("replay %d minted fill_id %q for an order already executed as %q — that is a "+
				"DOUBLE FILL: position_fills dedups on fill_id and would fold this trade twice",
				i, again[0].GetFillId(), first[0].GetFillId())
		}
		view, verr := v.QueryOrder(ctx, st)
		if verr != nil {
			t.Fatalf("replay %d: QueryOrder: %v", i, verr)
		}
		if view.State != OrderViewFilled {
			t.Fatalf("replay %d: QueryOrder state=%v, want FILLED — the reconciler adopts on FILLED and "+
				"re-drives on UNKNOWN", i, view.State)
		}
	}
	// 13 distinct orders were executed (o-replayed plus twelve fresh ones) and
	// nothing else may have minted an identity: one fill_id per EXECUTION, never
	// per delivery.
	if ids != 13 {
		t.Fatalf("the simulator minted %d fill ids for 13 executed orders across 12 replays", ids)
	}
}

// TestAForgottenOrderIsIndeterminateNotUnknown is the SECOND safety test, and
// the one that makes eviction admissible at all.
//
// UNKNOWN is an AFFIRMATIVE statement — order.Reconcile turns it into a RE-DRIVE
// when the OMS holds no venue ack. A venue that forgets orders and still answers
// UNKNOWN is telling the reconciler to trade an order it may already have
// filled. Past the retention the honest answer is INDETERMINATE, which
// quarantines.
func TestAForgottenOrderIsIndeterminateNotUnknown(t *testing.T) {
	ctx := context.Background()
	clk := newClock()
	retain := 10 * time.Minute
	v := NewSimVenue("XSIM", WithClock(clk.now), WithRetention(retain))

	old := simOrder("o-old")
	if _, err := v.Execute(ctx, old); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Before the sweep the record is intact and the venue speaks affirmatively
	// about an order it never saw.
	if view, _ := v.QueryOrder(ctx, simOrder("o-never-sent")); view.State != OrderViewUnknown {
		t.Fatalf("before any eviction a miss must be UNKNOWN, got %v", view.State)
	}

	// Age past the retention and write, which is what runs the sweep.
	clk.add(2 * retain)
	if _, err := v.Execute(ctx, simOrder("o-new")); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	view, err := v.QueryOrder(ctx, old)
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if view.State == OrderViewUnknown {
		t.Fatal("the venue dropped this order's record and still answered UNKNOWN — order.Reconcile " +
			"reads UNKNOWN with no venue ack as RE-DRIVE, so this answer fills the fund twice")
	}
	if view.State != OrderViewIndeterminate {
		t.Fatalf("state=%v, want INDETERMINATE (which order.Reconcile quarantines)", view.State)
	}
	if view.Reason == "" {
		t.Error("an INDETERMINATE answer must carry the venue's own words into the quarantine record")
	}

	// The latch covers every miss, not just the evicted id: after an eviction
	// this venue cannot tell "never executed" from "executed and forgotten".
	if view, _ := v.QueryOrder(ctx, simOrder("o-never-sent")); view.State != OrderViewIndeterminate {
		t.Fatalf("a miss after an eviction must be INDETERMINATE, got %v — the venue no longer keeps "+
			"a complete record, so its silence is not evidence", view.State)
	}

	// An order it still HOLDS is unaffected: eviction must not cost the
	// reconciler the answers that are still there.
	if view, _ := v.QueryOrder(ctx, simOrder("o-new")); view.State != OrderViewFilled {
		t.Fatalf("the surviving record must still answer FILLED, got %v", view.State)
	}
}

// TestExecutedRecordsDoNotAccumulate PROVES the bound: many orders over many
// retention windows leave a count set by the ORDER RATE over one window, not by
// how long the process has been up. The shape orderview's
// TestTerminalOrdersDoNotAccumulate uses.
func TestExecutedRecordsDoNotAccumulate(t *testing.T) {
	ctx := context.Background()
	clk := newClock()
	retain := time.Minute
	v := NewSimVenue("XSIM", WithClock(clk.now), WithRetention(retain))

	const orders = 2000
	// 20 full retention windows' worth of order flow, at a steady rate.
	step := retain / (orders / 20)
	for i := 0; i < orders; i++ {
		clk.add(step)
		if _, err := v.Execute(ctx, simOrder(fmt.Sprintf("o-%d", i))); err != nil {
			t.Fatalf("Execute %d: %v", i, err)
		}
	}

	v.mu.Lock()
	held := len(v.executed)
	v.mu.Unlock()

	// The sweep is amortized (at most once per retain), so an entry lives between
	// retain and 2*retain — hence the 2-window ceiling plus a little slack. What
	// this rejects is the pre-#895 behaviour, where held == orders.
	ceiling := 2*(orders/20) + orders/20
	if held > ceiling {
		t.Fatalf("the venue holds %d record(s) after %d orders over 20 retention windows (ceiling %d) — "+
			"the record is still growing with uptime rather than with the order rate", held, orders, ceiling)
	}
	if held == 0 {
		t.Fatal("the venue holds nothing at all — eviction must bound the record, not empty it")
	}
}

// TestRetentionDisabledKeepsEverything pins the escape hatch: a non-positive
// retention is the pre-#895 behaviour, and nothing is forgotten, so the venue
// keeps answering UNKNOWN affirmatively.
func TestRetentionDisabledKeepsEverything(t *testing.T) {
	ctx := context.Background()
	clk := newClock()
	v := NewSimVenue("XSIM", WithClock(clk.now), WithRetention(0))

	for i := 0; i < 50; i++ {
		clk.add(time.Hour)
		if _, err := v.Execute(ctx, simOrder(fmt.Sprintf("o-%d", i))); err != nil {
			t.Fatalf("Execute %d: %v", i, err)
		}
	}
	if view, _ := v.QueryOrder(ctx, simOrder("o-0")); view.State != OrderViewFilled {
		t.Fatalf("with eviction disabled the first order must still be FILLED, got %v", view.State)
	}
	if view, _ := v.QueryOrder(ctx, simOrder("o-never-sent")); view.State != OrderViewUnknown {
		t.Fatalf("with eviction disabled a miss stays affirmatively UNKNOWN, got %v", view.State)
	}
}
