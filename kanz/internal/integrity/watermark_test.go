package integrity_test

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

	"github.com/kanz-eng/kanz/internal/integrity"
)

func wmEnv(eventType, partitionKey string, eventTime time.Time) *envelopepb.Envelope {
	return &envelopepb.Envelope{
		EventType:    eventType,
		PartitionKey: partitionKey,
		EventTime:    timestamppb.New(eventTime),
	}
}

var wmBase = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func tradeAt(et time.Time) *envelopepb.Envelope {
	return wmEnv("market.equity.trade", "AAPL", et)
}

// --- Baseline + on-time -----------------------------------------------

func TestWatermark_FirstEventSeedsWatermark(t *testing.T) {
	w := integrity.NewWatermarkTracker(5 * time.Second)
	r := w.Observe(tradeAt(wmBase))
	if r.Status != integrity.WatermarkFirstSeen {
		t.Errorf("Status=%v want first_seen", r.Status)
	}
	if r.Late() {
		t.Error("first event reported late")
	}
}

func TestWatermark_AdvancingEventsAreOnTime(t *testing.T) {
	w := integrity.NewWatermarkTracker(5 * time.Second)
	w.Observe(tradeAt(wmBase))
	for i := 1; i <= 4; i++ {
		r := w.Observe(tradeAt(wmBase.Add(time.Duration(i) * time.Second)))
		if r.Status != integrity.WatermarkOnTime {
			t.Errorf("i=%d Status=%v want on_time", i, r.Status)
		}
	}
}

// An event within AllowedLateness behind the frontier is still on-time
// (normal reordering grace).
func TestWatermark_WithinAllowedLatenessIsOnTime(t *testing.T) {
	w := integrity.NewWatermarkTracker(5 * time.Second)
	w.Observe(tradeAt(wmBase.Add(10 * time.Second))) // frontier=base+10, watermark=base+5
	r := w.Observe(tradeAt(wmBase.Add(6 * time.Second)))
	if r.Status != integrity.WatermarkOnTime {
		t.Errorf("Status=%v want on_time (within grace)", r.Status)
	}
	// Boundary: exactly at the watermark is on-time (not late).
	r = w.Observe(tradeAt(wmBase.Add(5 * time.Second)))
	if r.Status != integrity.WatermarkOnTime {
		t.Errorf("at-watermark Status=%v want on_time", r.Status)
	}
}

// --- Late -------------------------------------------------------------

func TestWatermark_OlderThanWatermarkIsLate(t *testing.T) {
	w := integrity.NewWatermarkTracker(5 * time.Second)
	w.Observe(tradeAt(wmBase.Add(10 * time.Second))) // watermark = base+5
	r := w.Observe(tradeAt(wmBase.Add(2 * time.Second)))
	if r.Status != integrity.WatermarkLate {
		t.Fatalf("Status=%v want late", r.Status)
	}
	if !r.Late() {
		t.Error("Late() false for late event")
	}
	if r.Lateness != 3*time.Second { // watermark(base+5) - event(base+2)
		t.Errorf("Lateness=%v want 3s", r.Lateness)
	}
	if !r.Watermark.Equal(wmBase.Add(5 * time.Second)) {
		t.Errorf("Watermark=%v want base+5s", r.Watermark)
	}
}

func TestWatermark_LateEventDoesNotMoveWatermarkBackward(t *testing.T) {
	w := integrity.NewWatermarkTracker(5 * time.Second)
	w.Observe(tradeAt(wmBase.Add(10 * time.Second))) // frontier=base+10
	w.Observe(tradeAt(wmBase.Add(1 * time.Second)))  // late, must not lower frontier
	// Watermark still base+5: an event at base+5 is on-time.
	r := w.Observe(tradeAt(wmBase.Add(5 * time.Second)))
	if r.Status != integrity.WatermarkOnTime {
		t.Errorf("Status=%v want on_time (frontier held at base+10)", r.Status)
	}
}

// --- Stream isolation -------------------------------------------------

func TestWatermark_IsolatedPerSubjectAndPartition(t *testing.T) {
	w := integrity.NewWatermarkTracker(5 * time.Second)
	w.Observe(wmEnv("market.equity.trade", "AAPL", wmBase.Add(time.Hour)))

	// Different partition: its own watermark, so an early event is just
	// the baseline, not late.
	if r := w.Observe(wmEnv("market.equity.trade", "MSFT", wmBase)); r.Status != integrity.WatermarkFirstSeen {
		t.Errorf("MSFT Status=%v want first_seen (isolated)", r.Status)
	}
	// Different subject: own watermark too.
	if r := w.Observe(wmEnv("market.equity.quote", "AAPL", wmBase)); r.Status != integrity.WatermarkFirstSeen {
		t.Errorf("quote Status=%v want first_seen (isolated)", r.Status)
	}
}

// --- Config + unknowns ------------------------------------------------

func TestWatermark_NonPositiveLatenessFallsBackToDefault(t *testing.T) {
	w := integrity.NewWatermarkTracker(0) // → DefaultAllowedLateness (5s)
	w.Observe(tradeAt(wmBase.Add(10 * time.Second)))
	// watermark = base+10 - 5 = base+5; an event at base+1 is late.
	if r := w.Observe(tradeAt(wmBase.Add(1 * time.Second))); !r.Late() {
		t.Error("expected late with default allowed-lateness")
	}
	// An event at base+7 is on-time (within grace).
	if r := w.Observe(tradeAt(wmBase.Add(7 * time.Second))); r.Status != integrity.WatermarkOnTime {
		t.Errorf("Status=%v want on_time", r.Status)
	}
}

func TestWatermark_NilAndMissingEventTimeAreUnknown(t *testing.T) {
	w := integrity.NewWatermarkTracker(5 * time.Second)
	if r := w.Observe(nil); r.Status != integrity.WatermarkUnknown {
		t.Errorf("nil Status=%v want unknown", r.Status)
	}
	noET := &envelopepb.Envelope{EventType: "market.equity.trade", PartitionKey: "AAPL"}
	if r := w.Observe(noET); r.Status != integrity.WatermarkUnknown {
		t.Errorf("missing event_time Status=%v want unknown", r.Status)
	}
}

// --- LATE flag routing ------------------------------------------------

func TestMarkLate_StampsFlagIdempotently(t *testing.T) {
	env := tradeAt(wmBase)
	if integrity.IsLate(env) {
		t.Fatal("fresh envelope already flagged late")
	}
	integrity.MarkLate(env)
	if !integrity.IsLate(env) {
		t.Error("MarkLate did not set QUALITY_FLAG_LATE")
	}
	integrity.MarkLate(env) // idempotent
	count := 0
	for _, qf := range env.QualityFlags {
		if qf == envelopepb.QualityFlag_QUALITY_FLAG_LATE {
			count++
		}
	}
	if count != 1 {
		t.Errorf("QUALITY_FLAG_LATE present %d times want 1 (not idempotent)", count)
	}
}

func TestMarkLate_PreservesExistingFlags(t *testing.T) {
	env := tradeAt(wmBase)
	env.QualityFlags = []envelopepb.QualityFlag{envelopepb.QualityFlag_QUALITY_FLAG_DEGRADED}
	integrity.MarkLate(env)
	if !integrity.IsLate(env) {
		t.Error("LATE not added")
	}
	hasDegraded := false
	for _, qf := range env.QualityFlags {
		if qf == envelopepb.QualityFlag_QUALITY_FLAG_DEGRADED {
			hasDegraded = true
		}
	}
	if !hasDegraded {
		t.Error("MarkLate clobbered an existing flag")
	}
}

func TestMarkLate_NilSafe(t *testing.T) {
	integrity.MarkLate(nil) // must not panic
	if integrity.IsLate(nil) {
		t.Error("IsLate(nil) should be false")
	}
}
