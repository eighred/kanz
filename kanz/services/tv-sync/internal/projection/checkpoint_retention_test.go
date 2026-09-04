package projection

// A RESTORED POD MUST NOT LOSE WHAT RETENTION ATE (#809).
//
// Once account.execs is a window, the fold of everything that fell out of it is
// the only record of the fund's positions and realized P&L before the window
// opened. It lives in account.baseline, so a checkpoint that does not carry it
// restores a pod reporting a book that starts on Tuesday — which is EXEC-M21's
// defect (a trader looking at an empty account while the fund's positions sat
// open at the exchanges) reached from the other direction, and invisible because
// every other field is right.
//
// THESE RUN WITHOUT POSTGRES, deliberately. checkpoint_postgres_test.go holds the
// same round trip against the real store and skips silently when
// TEST_POSTGRES_URL is unset — which is most of the time, on most machines. The
// property being held here is about what the SNAPSHOT contains rather than about
// what the database does with it, so binding it to an unavailable dependency
// would leave the one number that cannot be re-derived unguarded on every laptop.

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// memLog is an in-process Log: enough to exercise Rehydrate and Checkpoint end to
// end, and nothing more.
//
// IT IS NOT A STAND-IN FOR PostgresLog and no test here treats it as one. It has
// no RLS, no transactions and does not honour a cancelled context — pgx does, and
// an in-memory store that ignores it has already hidden a real defect on this
// platform once. What it reproduces faithfully is the only contract these tests
// depend on: append-once by event_id, replay in seq order strictly after a
// watermark, one checkpoint per tenant.
type memLog struct {
	facts []Fact
	seen  map[string]bool
	cp    []byte
	cpSeq int64
	hasCP bool
}

func newMemLog() *memLog { return &memLog{seen: map[string]bool{}} }

var _ Log = (*memLog)(nil)

func (l *memLog) Append(_ context.Context, f Fact) (bool, int64, error) {
	if f.EventID == "" {
		return false, 0, ErrFactHasNoIdentity
	}
	if l.seen[f.EventID] {
		return false, 0, nil
	}
	l.seen[f.EventID] = true
	f.Seq = int64(len(l.facts) + 1)
	l.facts = append(l.facts, f)
	return true, f.Seq, nil
}

func (l *memLog) Replay(_ context.Context, afterSeq int64, fn func(Fact) error) error {
	for _, f := range l.facts {
		if f.Seq <= afterSeq {
			continue
		}
		if err := fn(f); err != nil {
			return err
		}
	}
	return nil
}

func (l *memLog) SaveCheckpoint(_ context.Context, seq int64, payload []byte) error {
	l.cp, l.cpSeq, l.hasCP = payload, seq, true
	return nil
}

func (l *memLog) LoadCheckpoint(context.Context) ([]byte, int64, bool, error) {
	return l.cp, l.cpSeq, l.hasCP, nil
}

// TestTheCheckpointCarriesTheEvictedFold.
//
// A pod folds a long history under a short window, evicts, checkpoints and dies.
// The pod that replaces it must report the SAME positions and the SAME realized
// P&L — not the same as the retained window, the same as the whole history. The
// only thing carrying the difference across is the baseline.
func TestTheCheckpointCarriesTheEvictedFold(t *testing.T) {
	const acct = "fund-alpha"
	ctx := context.Background()
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	log := newMemLog()
	clock := &fakeClock{t: start}
	dying := New(clock.now, nil, WithLog(log, testTenant), WithRetention(6*time.Hour))
	foldHistory(t, dying, clock, testTenant, acct, 40)
	dying.Evict(clock.t)

	wantPos, err := dying.Positions(testTenant, acct, time.Time{})
	if err != nil {
		t.Fatalf("dying positions: %v", err)
	}
	wantSt, err := dying.State(testTenant, acct, time.Time{})
	if err != nil {
		t.Fatalf("dying state: %v", err)
	}
	if held := dying.Resident(); held.Executions >= 40 {
		t.Fatalf("nothing was evicted (%d resident of 40) — the fixture never reaches the case "+
			"this test is about", held.Executions)
	}
	if err := dying.Checkpoint(ctx); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	reborn := New(clock.now, nil, WithLog(log, testTenant), WithRetention(6*time.Hour))
	if err := reborn.Rehydrate(ctx); err != nil {
		t.Fatalf("rehydrate: %v", err)
	}
	if !reborn.BootedFromCheckpoint() {
		t.Fatal("the reborn pod replayed from zero instead of restoring the checkpoint — this test " +
			"would then pass on a checkpoint that carries nothing at all")
	}

	gotPos, err := reborn.Positions(testTenant, acct, time.Time{})
	if err != nil {
		t.Fatalf("reborn positions: %v", err)
	}
	if len(wantPos) != len(gotPos) {
		t.Fatalf("positions: before %d, after restore %d", len(wantPos), len(gotPos))
	}
	for i := range wantPos {
		if wantPos[i] != gotPos[i] {
			t.Fatalf("the restored book is not the book that was checkpointed.\n  before: %+v\n"+
				"   after: %+v\nThe fold of the EVICTED history (account.baseline) is what carries "+
				"the fund's position and realized P&L from before the retention window; a checkpoint "+
				"without it restores a book that starts when the window does — EXEC-M21 (#809)",
				wantPos[i], gotPos[i])
		}
	}
	gotSt, err := reborn.State(testTenant, acct, time.Time{})
	if err != nil {
		t.Fatalf("reborn state: %v", err)
	}
	if *wantSt != *gotSt {
		t.Fatalf("the restored account state differs.\n  before: %+v\n   after: %+v", wantSt, gotSt)
	}

	// AND THE HORIZON CROSSES TOO. Without it the restored pod believes it has
	// forgotten nothing and answers as-of reads it cannot answer — from a window,
	// silently, which is the failure the refusal exists to prevent.
	wantFrom, _ := dying.RetainedFrom(testTenant, acct)
	gotFrom, ok := reborn.RetainedFrom(testTenant, acct)
	if !ok || !gotFrom.Equal(wantFrom) {
		t.Fatalf("retained-from after restore = %s (found=%v), want %s — a restored pod that thinks "+
			"it holds history it does not will answer an as-of read from a partial fold", gotFrom, ok, wantFrom)
	}
}

// TestABootWithNoCheckpointStaysInsideTheWindow.
//
// Replaying from seq 0 is a legitimate posture — a fresh deployment, or a
// truncated checkpoint table. Without eviction DURING the replay the pod would
// materialise the fund's entire history in RAM and only then trim it, so the boot
// most likely to be OOM-killed would be the one straight after an OOM kill: the
// crash loop #809 describes, where each restart takes longer than the last.
func TestABootWithNoCheckpointStaysInsideTheWindow(t *testing.T) {
	const acct = "fund-alpha"
	ctx := context.Background()
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	log := newMemLog()
	clock := &fakeClock{t: start}
	// One fill a minute, so rehydrateEvictEvery facts span far more than the
	// window and the replay must evict while it is still running.
	first := New(clock.now, nil, WithLog(log, testTenant), WithRetention(time.Hour))
	total := rehydrateEvictEvery + 500
	for i := 0; i < total; i++ {
		deliver(t, first, fmt.Sprintf("evt-%d", i), evtFilled,
			filledOrder(fmt.Sprintf("o%d", i), acct, "BTC", orderpb.Side_SIDE_BUY, 1, 100))
		clock.t = clock.t.Add(time.Minute)
	}

	reborn := New(clock.now, nil, WithLog(log, testTenant), WithRetention(time.Hour))
	if err := reborn.Rehydrate(ctx); err != nil {
		t.Fatalf("rehydrate: %v", err)
	}
	if reborn.BootedFromCheckpoint() {
		t.Fatal("this fixture is supposed to have no checkpoint")
	}
	held := reborn.Resident()
	if held.Executions >= total {
		t.Fatalf("the checkpoint-less boot held %d executions of %d — the whole history was "+
			"materialised before anything trimmed it, so the boot after an OOM kill is the one most "+
			"likely to be OOM-killed (#809)", held.Executions, total)
	}
	// It must still be the right book: eviction during replay folds into the
	// baseline exactly as it does in the steady state.
	pos, err := reborn.Positions(testTenant, acct, time.Time{})
	if err != nil {
		t.Fatalf("positions: %v", err)
	}
	wantQty := strconv.Itoa(total)
	if len(pos) != 1 || pos[0].Qty != wantQty {
		t.Fatalf("position after a bounded replay = %+v, want %s BTC — the replay evicted history "+
			"without folding it into the baseline, which loses everything before the window",
			pos, wantQty)
	}
}
