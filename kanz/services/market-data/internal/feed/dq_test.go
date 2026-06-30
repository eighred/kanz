package feed

import (
	"context"
	"sync"
	"testing"
	"time"

	marketpb "github.com/kanz-eng/kanz-schemas-go/market/v1"
)

func trade(t *testing.T, id string, sec int64, seq uint64) *marketpb.MarketDataEvent {
	t.Helper()
	ev, err := Trade(meta(id, sec, seq), dec(100, 0), dec(1, 0), "")
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// recSink records what passed the gate.
type recSink struct {
	mu  sync.Mutex
	evs []*marketpb.MarketDataEvent
}

func (r *recSink) Publish(_ context.Context, ev *marketpb.MarketDataEvent) error {
	r.mu.Lock()
	r.evs = append(r.evs, ev)
	r.mu.Unlock()
	return nil
}
func (r *recSink) count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.evs) }

func TestGate_DropsStale(t *testing.T) {
	now := ts(1000)
	rec := &recSink{}
	var breaches []Breach
	g := NewGate(rec, 60*time.Second, func(b Breach) { breaches = append(breaches, b) }).WithClock(func() time.Time { return now })

	// fresh (event_time 990, lag 10s ≤ 60s) passes; stale (event_time 900, lag 100s) dropped.
	_ = g.Publish(context.Background(), trade(t, "AAPL", 990, 1))
	_ = g.Publish(context.Background(), trade(t, "AAPL", 900, 2))
	if rec.count() != 1 {
		t.Fatalf("passed %d, want 1 (stale dropped)", rec.count())
	}
	if len(breaches) != 1 || breaches[0].Kind != BreachStale {
		t.Fatalf("breaches = %v, want one STALE", breaches)
	}
}

func TestGate_DropsOutOfOrder(t *testing.T) {
	now := ts(1000)
	rec := &recSink{}
	var breaches []Breach
	g := NewGate(rec, 0, func(b Breach) { breaches = append(breaches, b) }).WithClock(func() time.Time { return now })

	_ = g.Publish(context.Background(), trade(t, "AAPL", 500, 1))
	_ = g.Publish(context.Background(), trade(t, "AAPL", 499, 9)) // event_time regresses ⇒ dropped (seq not tracked)
	_ = g.Publish(context.Background(), trade(t, "AAPL", 501, 2)) // forward again, seq follows the last passed ⇒ passes clean
	if rec.count() != 2 {
		t.Fatalf("passed %d, want 2 (out-of-order dropped)", rec.count())
	}
	if len(breaches) != 1 || breaches[0].Kind != BreachOutOfOrder {
		t.Fatalf("breaches = %v, want one OUT_OF_ORDER", breaches)
	}
}

func TestGate_ReportsButPassesGap(t *testing.T) {
	now := ts(1000)
	rec := &recSink{}
	var breaches []Breach
	g := NewGate(rec, 0, func(b Breach) { breaches = append(breaches, b) }).WithClock(func() time.Time { return now })

	_ = g.Publish(context.Background(), trade(t, "AAPL", 1, 1))
	_ = g.Publish(context.Background(), trade(t, "AAPL", 2, 4)) // seq jumps 1→4: a gap
	if rec.count() != 2 {
		t.Fatalf("passed %d, want 2 (a valid gapped tick is passed, not dropped)", rec.count())
	}
	if len(breaches) != 1 || breaches[0].Kind != BreachGap {
		t.Fatalf("breaches = %v, want one GAP", breaches)
	}
}

func TestGate_IndependentPerInstrument(t *testing.T) {
	now := ts(1000)
	rec := &recSink{}
	g := NewGate(rec, 0, nil).WithClock(func() time.Time { return now })
	// AAPL and MSFT interleave; each ordered within itself.
	for _, ev := range []*marketpb.MarketDataEvent{
		trade(t, "AAPL", 1, 1), trade(t, "MSFT", 5, 1), trade(t, "AAPL", 2, 2), trade(t, "MSFT", 6, 2),
	} {
		_ = g.Publish(context.Background(), ev)
	}
	if rec.count() != 4 {
		t.Fatalf("passed %d, want 4 (per-instrument ordering independent)", rec.count())
	}
}

func TestSnapshotAndTee(t *testing.T) {
	snap := NewSnapshot()
	rec := &recSink{}
	tee := Tee(rec, snap)

	_ = tee.Publish(context.Background(), trade(t, "AAPL", 1, 1))
	_ = tee.Publish(context.Background(), trade(t, "AAPL", 2, 2)) // newer — replaces latest
	_ = tee.Publish(context.Background(), trade(t, "MSFT", 1, 1))

	if rec.count() != 3 {
		t.Errorf("tee fanned %d to rec, want 3", rec.count())
	}
	got, ok := snap.Latest("AAPL")
	if !ok || got.GetEventTime().AsTime() != ts(2) {
		t.Errorf("latest AAPL = %v ok=%v, want event_time 2", got.GetEventTime().AsTime(), ok)
	}
	if _, ok := snap.Latest("NVDA"); ok {
		t.Error("unknown instrument should not resolve")
	}
	if insts := snap.Instruments(); len(insts) != 2 || insts[0] != "AAPL" || insts[1] != "MSFT" {
		t.Errorf("instruments = %v, want [AAPL MSFT]", insts)
	}
}
