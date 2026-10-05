package returns

import (
	"context"
	"math"
	"testing"

	"github.com/eighred/kanz/internal/marketdata/store"
)

func TestReturnWindowBounds(t *testing.T) {
	for _, cfg := range []ReturnsConfig{{}, {Window: math.MaxInt}} {
		p := NewStoreReturnsProvider(store.NewMemory(), cfg)
		for _, window := range []int{MaxReturnWindow + 1, math.MaxInt, 0} {
			invalid := window > MaxReturnWindow || (window == 0 && cfg.Window > MaxReturnWindow)
			_, err := p.Returns(context.Background(), "A", d(10), window)
			if (err != nil) != invalid {
				t.Fatalf("scalar cfg=%+v window=%d err=%v", cfg, window, err)
			}
			_, err = p.DatedReturns(context.Background(), "A", d(10), window)
			if (err != nil) != invalid {
				t.Fatalf("dated cfg=%+v window=%d err=%v", cfg, window, err)
			}
		}
	}
}

func TestBoundedReturnsKeepVisibleTail(t *testing.T) {
	s := store.NewMemory()
	for i := 0; i < 500; i++ {
		if err := s.Put(context.Background(), []store.Observation{closeObs("A", i, float64(100+i), i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Put(context.Background(), []store.Observation{closeObs("A", 498, 9999, 600)}); err != nil {
		t.Fatal(err)
	}
	p := NewStoreReturnsProvider(s, ReturnsConfig{})
	got, err := p.Returns(context.Background(), "A", d(500), 2)
	if err != nil || !approxEqual(got, computeReturns([]float64{597, 598, 599}, ReturnSimple), 1e-12) {
		t.Fatalf("scalar: %v %v", got, err)
	}
	series, err := p.DatedReturns(context.Background(), "A", d(500), 2)
	if err != nil || len(series.Observations) != 2 || !series.Observations[0].Start.Equal(d(497)) {
		t.Fatalf("dated: %+v %v", series, err)
	}
}
