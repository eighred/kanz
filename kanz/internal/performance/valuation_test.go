package performance

import (
	"context"
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/marketdata/store"
)

func px(v int64) *commonpb.Decimal { return &commonpb.Decimal{Coefficient: v, Exponent: 0} }

type staticPositions map[string][]Holding

func (m staticPositions) Holdings(_ context.Context, portfolioID string, _, _ time.Time) ([]Holding, error) {
	return m[portfolioID], nil
}

func seedStore(t *testing.T) *store.Memory {
	t.Helper()
	s := store.NewMemory()
	day := func(d int) time.Time { return time.Date(2024, 1, d, 0, 0, 0, 0, time.UTC) }
	obs := []store.Observation{
		{InstrumentID: "AAA", ObservationTime: day(1), Price: px(100), Kind: store.PriceKindClose, KnowledgeTime: day(1)},
		{InstrumentID: "AAA", ObservationTime: day(10), Price: px(110), Kind: store.PriceKindClose, KnowledgeTime: day(10)},
	}
	if err := s.Put(context.Background(), obs); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestValuer_PointInTimeMark(t *testing.T) {
	s := seedStore(t)
	v := NewValuer(s, staticPositions{"P": {{InstrumentID: "AAA", Quantity: 10}}}, store.PriceKindClose)
	day := func(d int) time.Time { return time.Date(2024, 1, d, 0, 0, 0, 0, time.UTC) }

	// As of day 10: 10 shares × 110 = 1100.
	val, err := v.Value(context.Background(), "P", day(10), day(10))
	if err != nil {
		t.Fatal(err)
	}
	near(t, "value day10", val, 1100, 1e-9)

	// As of day 5 (observation horizon): only the day-1 price is ≤ day 5, so the
	// mark is 100 ⇒ 1000.
	val5, _ := v.Value(context.Background(), "P", day(5), day(5))
	near(t, "value day5 uses prior mark", val5, 1000, 1e-9)
}

func TestValuer_NoFutureLeakage(t *testing.T) {
	s := seedStore(t)
	day := func(d int) time.Time { return time.Date(2024, 1, d, 0, 0, 0, 0, time.UTC) }
	// A LATE RESTATEMENT of the day-10 price to 200, learned only on day 20.
	if err := s.Put(context.Background(), []store.Observation{
		{InstrumentID: "AAA", ObservationTime: day(10), Price: px(200), Kind: store.PriceKindClose, KnowledgeTime: day(20)},
	}); err != nil {
		t.Fatal(err)
	}
	v := NewValuer(s, staticPositions{"P": {{InstrumentID: "AAA", Quantity: 10}}}, store.PriceKindClose)

	// Read as of day 10 (knowledge horizon = day 10): the restatement (known day
	// 20) is INVISIBLE — value is still 10×110 = 1100, not 2000. The MODEL-01i
	// no-future-leakage contract for performance.
	asOf10, _ := v.Value(context.Background(), "P", day(10), day(10))
	near(t, "no future leakage", asOf10, 1100, 1e-9)

	// Read with knowledge as of day 21: the restatement is now visible ⇒ 2000.
	asOf21, _ := v.Value(context.Background(), "P", day(10), day(21))
	near(t, "restatement visible after knowledge", asOf21, 2000, 1e-9)
}

func TestBuildSubPeriods_FlowAware(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2024, 1, d, 0, 0, 0, 0, time.UTC) }
	dates := []time.Time{day(1), day(10), day(20)}
	values := []float64{100, 110, 176}
	flows := []Flow{{Time: day(15), Amount: 50}} // within (day10, day20]
	sub := BuildSubPeriods(dates, values, flows)
	if len(sub) != 2 {
		t.Fatalf("want 2 sub-periods, got %d", len(sub))
	}
	near(t, "p1 begin", sub[0].BeginValue, 100, 0)
	near(t, "p2 flow attributed", sub[1].Flow, 50, 0)
	near(t, "TWR", TimeWeightedReturn(sub), 0.21, 1e-12)
}
