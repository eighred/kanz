package performance

import (
	"context"
	"testing"
	"time"

	"github.com/kanz-eng/kanz/internal/marketdata/store"
)

func TestBenchmark_WeightedReturn(t *testing.T) {
	b := Benchmark{ID: "BM", Constituents: []Constituent{
		{InstrumentID: "A", Weight: 0.6},
		{InstrumentID: "B", Weight: 0.4},
	}}
	r := b.Return(map[string]float64{"A": 0.10, "B": 0.05})
	near(t, "benchmark return", r, 0.08, 1e-12)
	near(t, "active return", ActiveReturn(0.09, r), 0.01, 1e-12)
}

func TestBenchmark_RenormalizesWeights(t *testing.T) {
	// Weights sum to 0.8 (a constituent dropped); the weighted average must still
	// normalize, not under-count.
	b := Benchmark{Constituents: []Constituent{{InstrumentID: "A", Weight: 0.4}, {InstrumentID: "B", Weight: 0.4}}}
	near(t, "renormalized", b.Return(map[string]float64{"A": 0.10, "B": 0.20}), 0.15, 1e-12)
}

func TestInstrumentReturn_PointInTime(t *testing.T) {
	s := seedStore(t)
	day := func(d int) time.Time { return time.Date(2024, 1, d, 0, 0, 0, 0, time.UTC) }
	r, ok, err := InstrumentReturn(context.Background(), s, "AAA", store.PriceKindClose, day(1), day(10), day(10))
	if err != nil || !ok {
		t.Fatalf("InstrumentReturn ok=%v err=%v", ok, err)
	}
	near(t, "100→110", r, 0.10, 1e-9)

	// A restatement learned after the asOf horizon must not change the return.
	_ = s.Put(context.Background(), []store.Observation{
		{InstrumentID: "AAA", ObservationTime: day(10), Price: px(200), Kind: store.PriceKindClose, KnowledgeTime: day(20)},
	})
	r2, _, _ := InstrumentReturn(context.Background(), s, "AAA", store.PriceKindClose, day(1), day(10), day(10))
	near(t, "no future leakage", r2, 0.10, 1e-9)
}
