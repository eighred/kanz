package returns

import (
	"context"
	"math"
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/marketdata/store"
)

var base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func d(n int) time.Time { return base.AddDate(0, 0, n) }

func price(v float64) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: int64(math.Round(v * 100)), Exponent: -2}
}

func closeObs(inst string, day int, v float64, knowDay int) store.Observation {
	return store.Observation{
		InstrumentID:    inst,
		ObservationTime: d(day),
		Price:           price(v),
		Kind:            store.PriceKindClose,
		KnowledgeTime:   d(knowDay),
	}
}

func approxEqual(a, b []float64, tol float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Abs(a[i]-b[i]) > tol {
			return false
		}
	}
	return true
}

func TestComputeReturns(t *testing.T) {
	prices := []float64{100, 110, 99}
	if got := computeReturns(prices, ReturnSimple); !approxEqual(got, []float64{0.1, -0.1}, 1e-9) {
		t.Fatalf("simple returns: %v", got)
	}
	want := []float64{math.Log(110.0 / 100), math.Log(99.0 / 110)}
	if got := computeReturns(prices, ReturnLog); !approxEqual(got, want, 1e-9) {
		t.Fatalf("log returns: %v want %v", got, want)
	}
	// A non-positive previous price is skipped, not NaN/Inf.
	if got := computeReturns([]float64{0, 100, 110}, ReturnSimple); !approxEqual(got, []float64{0.1}, 1e-9) {
		t.Fatalf("zero-guard returns: %v", got)
	}
	if got := computeReturns([]float64{100}, ReturnSimple); got != nil {
		t.Fatalf("single price ⇒ no returns, got %v", got)
	}
}

func TestStoreReturnsProvider_PointInTime(t *testing.T) {
	s := store.NewMemory()
	// Closes on days 1..4; day-2 close is later restated (known only on day 9).
	if err := s.Put(context.Background(), []store.Observation{
		closeObs("AAPL", 1, 100, 1),
		closeObs("AAPL", 2, 110, 2),
		closeObs("AAPL", 2, 200, 9), // restatement, known day 9
		closeObs("AAPL", 3, 121, 3),
		closeObs("AAPL", 4, 130, 4),
	}); err != nil {
		t.Fatal(err)
	}
	p := NewStoreReturnsProvider(s, ReturnsConfig{Method: ReturnSimple})

	// As of day 4: only knowledge_time <= 4 visible, so day 2 = 110 (not 200).
	got, err := p.Returns(context.Background(), "AAPL", d(4), 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{
		(110.0 - 100) / 100, // day1→2
		(121.0 - 110) / 110, // day2→3
		(130.0 - 121) / 121, // day3→4
	}
	if !approxEqual(got, want, 1e-9) {
		t.Fatalf("point-in-time returns: got %v want %v", got, want)
	}
}

func TestStoreReturnsProvider_WindowTail(t *testing.T) {
	s := store.NewMemory()
	var obs []store.Observation
	for day := 1; day <= 10; day++ {
		obs = append(obs, closeObs("AAPL", day, 100+float64(day), day))
	}
	if err := s.Put(context.Background(), obs); err != nil {
		t.Fatal(err)
	}
	p := NewStoreReturnsProvider(s, ReturnsConfig{Method: ReturnSimple})
	// Window 3 ⇒ at most 3 returns (the most recent), from the last 4 closes.
	got, err := p.Returns(context.Background(), "AAPL", d(10), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("window 3 ⇒ 3 returns, got %d (%v)", len(got), got)
	}
	// Last return is day9→day10: (110-109)/109.
	if math.Abs(got[2]-(1.0/109)) > 1e-9 {
		t.Fatalf("most-recent return wrong: %v", got[2])
	}
}

func TestStoreReturnsProvider_InsufficientHistory(t *testing.T) {
	s := store.NewMemory()
	if err := s.Put(context.Background(), []store.Observation{closeObs("AAPL", 1, 100, 1)}); err != nil {
		t.Fatal(err)
	}
	got, err := NewStoreReturnsProvider(s, ReturnsConfig{}).Returns(context.Background(), "AAPL", d(5), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("one price ⇒ empty returns (not error), got %v", got)
	}
}
