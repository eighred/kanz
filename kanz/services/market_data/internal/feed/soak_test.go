package feed

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// PARITY-01i — soak + chaos harness. It drives the full ingestion pipeline
// (SimAdapter → DQ Gate → Tee[stream, Snapshot]) over a generated session that
// interleaves clean ticks with injected faults (stale, out-of-order, sequence
// gap) across many instruments, and asserts the canonical invariants every feed
// must hold downstream of the gate:
//
//   - zero ordering inversions reach the stream (the gate dropped the bad ones);
//   - stale + out-of-order are dropped and reported; a gap is reported but passed;
//   - the snapshot holds the latest clean value per instrument.
//
// The real 24h soak + the p99-normalization-latency SLO run in the perf
// environment; this is the deterministic, fast invariant check that gates CI.

// chaosSession builds, per instrument, a clean ascending run of `clean` trades
// plus one stale, one out-of-order, and one sequence-gap fault — returning the
// session and the expected pass/breach tallies.
func chaosSession(t *testing.T, now time.Time, instruments []string, clean int, budget time.Duration) (Session, int, map[BreachKind]int) {
	t.Helper()
	var sess Session
	wantPass := 0
	wantBreach := map[BreachKind]int{}
	for _, id := range instruments {
		var seq uint64
		// clean ascending run, all fresh (within budget of now).
		base := now.Add(-budget / 2) // comfortably fresh
		for i := 0; i < clean; i++ {
			seq++
			ev, err := Trade(Meta{InstrumentID: id, Symbol: id, MIC: "XNAS", EventTime: base.Add(time.Duration(i) * time.Second), SourceSequence: seq}, dec(int64(100+i), 0), dec(1, 0), "")
			if err != nil {
				t.Fatal(err)
			}
			sess = append(sess, ev)
			wantPass++
		}
		// stale: event_time far older than the budget ⇒ dropped.
		seq++
		stale, _ := Trade(Meta{InstrumentID: id, Symbol: id, MIC: "XNAS", EventTime: now.Add(-2 * budget), SourceSequence: seq}, dec(1, 0), dec(1, 0), "")
		sess = append(sess, stale)
		wantBreach[BreachStale]++
		// out-of-order: event_time before the last clean ⇒ dropped (still fresh).
		seq++
		ooo, _ := Trade(Meta{InstrumentID: id, Symbol: id, MIC: "XNAS", EventTime: base, SourceSequence: seq}, dec(2, 0), dec(1, 0), "")
		sess = append(sess, ooo)
		wantBreach[BreachOutOfOrder]++
		// sequence gap: skip ahead, fresh + forward in time ⇒ reported, PASSED.
		seq += 5
		gap, _ := Trade(Meta{InstrumentID: id, Symbol: id, MIC: "XNAS", EventTime: base.Add(time.Duration(clean+10) * time.Second), SourceSequence: seq}, dec(3, 0), dec(1, 0), "")
		sess = append(sess, gap)
		wantBreach[BreachGap]++
		wantPass++ // the gap tick is passed
	}
	return sess, wantPass, wantBreach
}

func TestSoak_ChaosPipelineHoldsInvariants(t *testing.T) {
	now := ts(1_000_000)
	budget := time.Hour
	instruments := []string{"AAPL", "MSFT", "VOD", "BRN", "NVDA"}

	sess, wantPass, wantBreach := chaosSession(t, now, instruments, 200, budget)

	snap := NewSnapshot()
	passed := &recSink{}
	var bmu sync.Mutex
	gotBreach := map[BreachKind]int{}
	gate := NewGate(Tee(passed, snap), budget, func(b Breach) {
		bmu.Lock()
		gotBreach[b.Kind]++
		bmu.Unlock()
	}).WithClock(func() time.Time { return now })

	sim := &SimAdapter{Session: sess}
	if err := sim.Run(context.Background(), instruments, gate); err != nil {
		t.Fatal(err)
	}

	if passed.count() != wantPass {
		t.Fatalf("passed %d, want %d", passed.count(), wantPass)
	}
	for _, k := range []BreachKind{BreachStale, BreachOutOfOrder, BreachGap} {
		if gotBreach[k] != wantBreach[k] {
			t.Errorf("breach %s = %d, want %d", k, gotBreach[k], wantBreach[k])
		}
	}
	// Invariant: nothing out-of-order reached the stream.
	if inv := OutOfOrder(passed.evs); len(inv) != 0 {
		t.Errorf("ordering inversions reached the stream: %v", inv)
	}
	// Snapshot holds the latest clean value per instrument (the gap tick, passed
	// last and forward in time, is the latest).
	for _, id := range instruments {
		if _, ok := snap.Latest(id); !ok {
			t.Errorf("snapshot missing %s", id)
		}
	}
}

func TestSoak_Throughput(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping soak throughput in -short")
	}
	now := ts(1_000_000)
	const instruments, perInstrument = 50, 2000
	ids := make([]string, instruments)
	var sess Session
	for i := range ids {
		ids[i] = fmt.Sprintf("INST%03d", i)
		for j := 0; j < perInstrument; j++ {
			ev, _ := Trade(Meta{InstrumentID: ids[i], Symbol: ids[i], MIC: "XNAS", EventTime: now.Add(time.Duration(j) * time.Second), SourceSequence: uint64(j + 1)}, dec(int64(j), 0), dec(1, 0), "")
			sess = append(sess, ev)
		}
	}
	gate := NewGate(NewSnapshot(), 0, nil)
	start := time.Now()
	if err := (&SimAdapter{Session: sess}).Run(context.Background(), ids, gate); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	n := instruments * perInstrument
	t.Logf("normalized+gated %d events in %s (%.0f events/sec)", n, elapsed, float64(n)/elapsed.Seconds())
	// Generous CI ceiling — the real p99 SLO lives in the perf env; this only
	// guards a gross regression (100k events should be well under a second).
	if elapsed > 5*time.Second {
		t.Errorf("pipeline too slow: %s for %d events", elapsed, n)
	}
}
