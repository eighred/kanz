package projection

// The fold checkpoint, against real Postgres (#809).
//
// The claim this whole change rests on is an equality:
//
//	restore(checkpoint) + fold(facts after it)  ==  fold(all facts)
//
// If it does not hold, a booting pod serves a book the fund never had — which is
// EXEC-M21's failure with an extra step, and the reason #809's original proposal
// (bound the replay to a window) was not implemented: a window makes that equality
// FALSE by construction for anything older than it.
//
// These run against a real database because the whole mechanism is a durable
// round-trip: a fake log would prove the marshalling and not that a checkpoint
// written by one process is read back by another and lands the pod on the same
// book.

import (
	"context"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// TestACheckpointedBootReachesTheSameViewAsAFullReplay is the equality itself.
//
// Two pods fold the SAME history. One replays every fact; the other restores a
// checkpoint taken midway and folds only the tail. Every DTO the Broker API serves
// must match — positions, realized P&L, executions and orders — because those are
// what a trader acts on.
func TestACheckpointedBootReachesTheSameViewAsAFullReplay(t *testing.T) {
	pool := newPool(t, testTenant)
	freshSchema(t, pool)
	ctx := context.Background()
	log := NewPostgresLog(pool)

	// A pod trades, checkpoints MIDWAY, then trades some more and dies.
	live := New(time.Now, nil, WithLog(log, testTenant))
	deliver(t, live, "evt-1", evtFilled, filledOrder("o1", "fund-alpha", "BTC", orderpb.Side_SIDE_BUY, 2, 100))
	deliver(t, live, "evt-2", evtFilled, filledOrder("o2", "fund-alpha", "BTC", orderpb.Side_SIDE_BUY, 3, 110))

	if err := live.Checkpoint(ctx); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	// Facts AFTER the checkpoint. These are the tail the restored pod must fold.
	deliver(t, live, "evt-3", evtFilled, filledOrder("o3", "fund-alpha", "BTC", orderpb.Side_SIDE_SELL, 1, 150))
	deliver(t, live, "evt-4", evtFilled, filledOrder("o4", "fund-alpha", "ETH", orderpb.Side_SIDE_BUY, 5, 20))

	// Pod A: the pre-#809 boot. Replays everything, because it has no checkpoint of
	// its own to find — it reads the one `live` wrote, so to get a full replay we
	// use a projection with no checkpoint at all.
	full := New(time.Now, nil, WithLog(log, testTenant))
	if err := full.replayEverythingForTest(ctx); err != nil {
		t.Fatalf("full replay: %v", err)
	}

	// Pod B: the #809 boot. Restores the checkpoint, folds the tail.
	fromCP := New(time.Now, nil, WithLog(log, testTenant))
	if err := fromCP.Rehydrate(ctx); err != nil {
		t.Fatalf("checkpointed rehydrate: %v", err)
	}
	if !fromCP.BootedFromCheckpoint() {
		t.Fatal("the pod did not resume from the checkpoint it was supposed to find — it replayed " +
			"everything, so this test would compare a full replay against a full replay and prove " +
			"nothing (#809)")
	}

	assertSameView(t, full, fromCP)
}

// TestACheckpointedBootCarriesTheFillDedupSet.
//
// seenFills is not part of the view and is the most dangerous thing to lose: it is
// the dedup for the DUAL fill path — the synchronous venue response and the
// asynchronous websocket echo of the same fill. A fill id missing when its echo
// arrives is a fill folded twice, which doubles a position the fund does not hold.
func TestACheckpointedBootCarriesTheFillDedupSet(t *testing.T) {
	pool := newPool(t, testTenant)
	freshSchema(t, pool)
	ctx := context.Background()
	log := NewPostgresLog(pool)

	live := New(time.Now, nil, WithLog(log, testTenant))
	deliver(t, live, "evt-1", evtFilled, filledOrder("o1", "fund-alpha", "BTC", orderpb.Side_SIDE_BUY, 2, 100))
	if err := live.Checkpoint(ctx); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	reborn := New(time.Now, nil, WithLog(log, testTenant))
	if err := reborn.Rehydrate(ctx); err != nil {
		t.Fatalf("rehydrate: %v", err)
	}

	before, _ := reborn.Positions(testTenant, "fund-alpha", time.Time{})
	if len(before) != 1 {
		t.Fatalf("restored positions = %+v, want one", before)
	}

	// The SAME fill arrives again on the other path — a different envelope id, so
	// the fact log's exactly-once cannot catch it. Only seenFills can.
	deliver(t, reborn, "evt-echo", evtFilled, filledOrder("o1", "fund-alpha", "BTC", orderpb.Side_SIDE_BUY, 2, 100))

	after, _ := reborn.Positions(testTenant, "fund-alpha", time.Time{})
	if len(after) != 1 || after[0].Qty != before[0].Qty {
		t.Fatalf("a re-echoed fill changed the restored position: %s -> %s. The checkpoint did not "+
			"carry seenFills, so the fill was folded twice and the fund now shows a holding it "+
			"does not have (#809).", before[0].Qty, after[0].Qty)
	}
}

// TestAPodWithNoCheckpointStillRebuildsEverything.
//
// The no-checkpoint boot must remain exactly the pre-#809 behaviour: a fresh
// deployment, or one whose checkpoint table was truncated, replays the whole log
// and reaches the same view. The dangerous reading of a missing checkpoint is the
// opposite one — treating it as a restored empty account, which is EXEC-M21.
func TestAPodWithNoCheckpointStillRebuildsEverything(t *testing.T) {
	pool := newPool(t, testTenant)
	freshSchema(t, pool)
	ctx := context.Background()
	log := NewPostgresLog(pool)

	live := New(time.Now, nil, WithLog(log, testTenant))
	deliver(t, live, "evt-1", evtFilled, filledOrder("o1", "fund-alpha", "BTC", orderpb.Side_SIDE_BUY, 2, 100))
	deliver(t, live, "evt-2", evtFilled, filledOrder("o2", "fund-alpha", "BTC", orderpb.Side_SIDE_SELL, 1, 150))
	// Deliberately NO checkpoint.

	reborn := New(time.Now, nil, WithLog(log, testTenant))
	if err := reborn.Rehydrate(ctx); err != nil {
		t.Fatalf("rehydrate: %v", err)
	}
	if reborn.BootedFromCheckpoint() {
		t.Fatal("the pod reported booting from a checkpoint that was never written")
	}

	// COMPARED AGAINST A FULL REPLAY, NOT AGAINST THE LIVE POD. The live pod's
	// timestamps have never been through Postgres, so they still carry nanoseconds
	// that TIMESTAMPTZ cannot store — a property of that process rather than of the
	// book. The reference for any rebuild is what the LOG gives back.
	reference := New(time.Now, nil, WithLog(log, testTenant))
	if err := reference.replayEverythingForTest(ctx); err != nil {
		t.Fatalf("reference replay: %v", err)
	}
	assertSameView(t, reference, reborn)
}

// TestACheckpointIsReplacedRatherThanAccumulated.
//
// One row per tenant. A history of checkpoints would be a retention decision of its
// own, and the older ones answer no question the fact log cannot.
func TestACheckpointIsReplacedRatherThanAccumulated(t *testing.T) {
	pool := newPool(t, testTenant)
	freshSchema(t, pool)
	ctx := context.Background()
	log := NewPostgresLog(pool)

	live := New(time.Now, nil, WithLog(log, testTenant))
	deliver(t, live, "evt-1", evtFilled, filledOrder("o1", "fund-alpha", "BTC", orderpb.Side_SIDE_BUY, 2, 100))
	if err := live.Checkpoint(ctx); err != nil {
		t.Fatalf("first checkpoint: %v", err)
	}
	_, firstSeq, ok, err := log.LoadCheckpoint(ctx)
	if err != nil || !ok {
		t.Fatalf("load after first checkpoint: (%v, %v)", ok, err)
	}

	deliver(t, live, "evt-2", evtFilled, filledOrder("o2", "fund-alpha", "BTC", orderpb.Side_SIDE_BUY, 1, 120))
	if err := live.Checkpoint(ctx); err != nil {
		t.Fatalf("second checkpoint: %v", err)
	}

	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tv_checkpoints`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Errorf("tv_checkpoints holds %d rows, want exactly 1 — a checkpoint per tenant is replaced, "+
			"not accumulated, or the table becomes a retention decision nobody took", rows)
	}
	_, secondSeq, _, _ := log.LoadCheckpoint(ctx)
	if secondSeq <= firstSeq {
		t.Errorf("the second checkpoint's seq (%d) did not advance past the first (%d) — a stale "+
			"watermark makes the next boot re-fold facts it already holds", secondSeq, firstSeq)
	}
}

// TestNothingFoldedWritesNoCheckpoint: a seq-0 checkpoint would record an empty
// view, and an empty restored view is EXEC-M21's defect.
func TestNothingFoldedWritesNoCheckpoint(t *testing.T) {
	pool := newPool(t, testTenant)
	freshSchema(t, pool)
	ctx := context.Background()
	log := NewPostgresLog(pool)

	idle := New(time.Now, nil, WithLog(log, testTenant))
	if err := idle.Checkpoint(ctx); err != nil {
		t.Fatalf("checkpoint on an idle pod: %v", err)
	}
	if _, _, ok, err := log.LoadCheckpoint(ctx); err != nil || ok {
		t.Fatalf("an idle pod wrote a checkpoint (ok=%v, err=%v). Restoring it would give the next "+
			"boot an empty book at seq 0 and skip nothing — harmless today, and exactly the shape "+
			"that reports an empty account when a watermark is ever reset.", ok, err)
	}
}

// countingLog wraps a real log and records the watermark Replay was given, plus
// how many facts it handed back.
//
// THE VIEW CANNOT PROVE THE WATERMARK. The fold is idempotent — seenFills dedups
// the fills and Orders() serves only the latest revision per order — so replaying
// from 0 onto a restored checkpoint produces the IDENTICAL book. That is a good
// property, and it means a DTO comparison is structurally blind to the bound being
// ignored: the pod does all the work again and looks perfect. Only the count of
// facts folded can see it, which is what this exists for. Measured: without it, a
// mutation that dropped the watermark survived every other test in this file.
type countingLog struct {
	Log
	afterSeq int64
	replayed int
}

func (c *countingLog) Replay(ctx context.Context, afterSeq int64, fn func(Fact) error) error {
	c.afterSeq = afterSeq
	return c.Log.Replay(ctx, afterSeq, func(f Fact) error {
		c.replayed++
		return fn(f)
	})
}

// TestARestoredBootFoldsOnlyTheTail is the bound itself.
func TestARestoredBootFoldsOnlyTheTail(t *testing.T) {
	pool := newPool(t, testTenant)
	freshSchema(t, pool)
	ctx := context.Background()
	log := NewPostgresLog(pool)

	live := New(time.Now, nil, WithLog(log, testTenant))
	deliver(t, live, "evt-1", evtFilled, filledOrder("o1", "fund-alpha", "BTC", orderpb.Side_SIDE_BUY, 2, 100))
	deliver(t, live, "evt-2", evtFilled, filledOrder("o2", "fund-alpha", "BTC", orderpb.Side_SIDE_BUY, 3, 110))
	if err := live.Checkpoint(ctx); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	deliver(t, live, "evt-3", evtFilled, filledOrder("o3", "fund-alpha", "BTC", orderpb.Side_SIDE_SELL, 1, 150))

	spy := &countingLog{Log: log}
	reborn := New(time.Now, nil, WithLog(spy, testTenant))
	if err := reborn.Rehydrate(ctx); err != nil {
		t.Fatalf("rehydrate: %v", err)
	}

	if spy.afterSeq == 0 {
		t.Fatal("the restored pod replayed from seq 0. It restored a checkpoint and then re-folded " +
			"every fact the fund has ever produced on top of it — which reaches the correct book, " +
			"because the fold is idempotent, and does exactly the work the checkpoint exists to " +
			"avoid. Nothing about the served view can show this (#809).")
	}
	if spy.replayed != 1 {
		t.Errorf("the restored pod folded %d facts, want exactly 1 — the tail after the checkpoint. "+
			"Anything more means the watermark is behind the state it was taken with.", spy.replayed)
	}
}

// TestARestoredBootAnswersAnAsOfReadTheSameWay is the bitemporal half.
//
// Every read on the Broker API takes an as-of, and it filters on KNOWLEDGE time —
// which is the one value that round-trips through Postgres at a different
// resolution from the payload. No served DTO exposes an execution's knowledge
// time, so a checkpoint that stored it wrongly is invisible to a present-time
// comparison and changes exactly which fills an as-of read returns.
func TestARestoredBootAnswersAnAsOfReadTheSameWay(t *testing.T) {
	pool := newPool(t, testTenant)
	freshSchema(t, pool)
	ctx := context.Background()
	log := NewPostgresLog(pool)

	live := New(time.Now, nil, WithLog(log, testTenant))
	deliver(t, live, "evt-1", evtFilled, filledOrder("o1", "fund-alpha", "BTC", orderpb.Side_SIDE_BUY, 2, 100))
	deliver(t, live, "evt-2", evtFilled, filledOrder("o2", "fund-alpha", "BTC", orderpb.Side_SIDE_BUY, 3, 110))
	if err := live.Checkpoint(ctx); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	deliver(t, live, "evt-3", evtFilled, filledOrder("o3", "fund-alpha", "BTC", orderpb.Side_SIDE_SELL, 1, 150))

	full := New(time.Now, nil, WithLog(log, testTenant))
	if err := full.replayEverythingForTest(ctx); err != nil {
		t.Fatalf("reference replay: %v", err)
	}
	fromCP := New(time.Now, nil, WithLog(log, testTenant))
	if err := fromCP.Rehydrate(ctx); err != nil {
		t.Fatalf("checkpointed rehydrate: %v", err)
	}

	// Walk the as-of boundary across every fill. A checkpoint that stored a
	// knowledge time at a different resolution moves the boundary by a microsecond,
	// which changes the answer for exactly one instant — and that is the instant
	// this loop lands on.
	refExecs, _ := full.Executions(testTenant, "fund-alpha", "", time.Time{})
	if len(refExecs) != 3 {
		t.Fatalf("reference has %d executions, want 3", len(refExecs))
	}
	for _, e := range refExecs {
		at, err := time.Parse(time.RFC3339Nano, e.ExecutedAt)
		if err != nil {
			t.Fatalf("parse %s: %v", e.ExecutedAt, err)
		}
		for _, probe := range []time.Time{at.Add(-time.Nanosecond), at, at.Add(time.Nanosecond)} {
			we, _ := full.Executions(testTenant, "fund-alpha", "", probe)
			ge, _ := fromCP.Executions(testTenant, "fund-alpha", "", probe)
			if len(we) != len(ge) {
				t.Fatalf("as-of %s: a full replay returns %d executions and a restored pod returns "+
					"%d. The checkpoint stored a time at a different resolution from the one the "+
					"log gives back, so the two disagree about what was known at that instant "+
					"(#809).", probe.Format(time.RFC3339Nano), len(we), len(ge))
			}
		}
	}
}

// replayEverythingForTest folds the whole log, ignoring any checkpoint — the
// pre-#809 boot, kept as the reference the checkpointed boot is compared against.
func (p *Projection) replayEverythingForTest(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.log.Replay(ctx, 0, func(f Fact) error {
		p.fold(p.tenant, f.EventType, f.Payload, f.Knowledge)
		p.seq = f.Seq
		return nil
	})
}

// assertSameView compares everything the Broker API serves for the test account.
func assertSameView(t *testing.T, want, got *Projection) {
	t.Helper()
	const acct = "fund-alpha"

	wp, wErr := want.Positions(testTenant, acct, time.Time{})
	gp, gErr := got.Positions(testTenant, acct, time.Time{})
	if (wErr == nil) != (gErr == nil) || len(wp) != len(gp) {
		t.Fatalf("positions: want %d (err=%v), got %d (err=%v)", len(wp), wErr, len(gp), gErr)
	}
	for i := range wp {
		if wp[i] != gp[i] {
			t.Errorf("position %d differs:\n  want %+v\n  got  %+v\n\nThe restored book is not the "+
				"book a full replay produces, so `restore(checkpoint) + tail == fold(all)` does "+
				"not hold and a trader is looking at a fund that never existed (#809).",
				i, wp[i], gp[i])
		}
	}

	ws, _ := want.State(testTenant, acct, time.Time{})
	gs, _ := got.State(testTenant, acct, time.Time{})
	if (ws == nil) != (gs == nil) {
		t.Fatalf("account state: want nil=%v, got nil=%v", ws == nil, gs == nil)
	}
	if ws != nil && *ws != *gs {
		t.Errorf("account state differs:\n  want %+v\n  got  %+v", *ws, *gs)
	}

	we, _ := want.Executions(testTenant, acct, "", time.Time{})
	ge, _ := got.Executions(testTenant, acct, "", time.Time{})
	if len(we) != len(ge) {
		t.Fatalf("executions: want %d, got %d — the restored trade history is a different length", len(we), len(ge))
	}
	for i := range we {
		if we[i] != ge[i] {
			t.Errorf("execution %d differs:\n  want %+v\n  got  %+v", i, we[i], ge[i])
		}
	}

	wo, _ := want.Orders(testTenant, acct, time.Time{})
	go_, _ := got.Orders(testTenant, acct, time.Time{})
	if len(wo) != len(go_) {
		t.Fatalf("orders: want %d, got %d", len(wo), len(go_))
	}
	for i := range wo {
		if wo[i] != go_[i] {
			t.Errorf("order %d differs:\n  want %+v\n  got  %+v", i, wo[i], go_[i])
		}
	}
}
