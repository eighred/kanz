package compute

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/risk/factormodel"
)

// trendReturns is a deterministic ReturnsProvider: each instrument gets a
// constant drift plus a small oscillation (nonzero vol), so momentum sign and
// volatility are known. Unknown instruments error.
type trendReturns map[string]float64 // instrument → per-period drift

func (tr trendReturns) Returns(_ context.Context, id string, _ time.Time, window int) ([]float64, error) {
	drift, ok := tr[id]
	if !ok {
		return nil, errors.New("no history")
	}
	rets := make([]float64, window)
	for i := range rets {
		rets[i] = drift + 0.002*math.Sin(float64(i)+drift*1000)
	}
	return rets, nil
}

var liveAsOf = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

func TestStoreCharacteristicProvider_DerivesStyleAndSector(t *testing.T) {
	rp := trendReturns{"AAPL": 0.001, "XOM": -0.001}
	sector := func(_ context.Context, id string, _ time.Time) (string, bool) {
		if id == "AAPL" {
			return "GICS:45", true
		}
		return "", false
	}
	p := NewStoreCharacteristicProvider(rp, sector, CharacteristicConfig{Window: 60, MomentumSkip: 5})

	up, ok := p.Characteristics(context.Background(), "AAPL", liveAsOf)
	if !ok {
		t.Fatal("AAPL must be known")
	}
	if up.Style[StyleMomentum] <= 0 {
		t.Errorf("rising drift must give positive momentum, got %v", up.Style[StyleMomentum])
	}
	if up.Style[StyleVolatility] <= 0 {
		t.Errorf("oscillating series must give positive volatility, got %v", up.Style[StyleVolatility])
	}
	if up.Industry != "GICS:45" {
		t.Errorf("industry: got %q want GICS:45", up.Industry)
	}

	down, ok := p.Characteristics(context.Background(), "XOM", liveAsOf)
	if !ok || down.Style[StyleMomentum] >= 0 {
		t.Errorf("falling drift must give negative momentum, got %v (ok=%v)", down.Style[StyleMomentum], ok)
	}

	// Neither history nor classification ⇒ unknown.
	if _, ok := p.Characteristics(context.Background(), "GHOST", liveAsOf); ok {
		t.Error("instrument with no history and no sector must be unknown")
	}
}

func TestLiveModelProvider_FitsAndCachesPerSnapshot(t *testing.T) {
	rp := trendReturns{"A": 0.001, "B": -0.0005, "C": 0.0002}
	chars := NewStoreCharacteristicProvider(rp, nil, CharacteristicConfig{Window: 40, MomentumSkip: 5})
	universeCalls := 0
	universe := func(_ context.Context, _ time.Time) ([]string, error) {
		universeCalls++
		return []string{"A", "B", "C"}, nil
	}
	p := NewLiveModelProvider(
		factormodel.Config{StyleFactors: []string{StyleMomentum, StyleVolatility}, Window: 40},
		universe,
		factormodel.Providers{Characteristics: chars, Returns: rp},
	)
	ctx := context.Background()

	m1, ok := p.Model(ctx, liveAsOf)
	if !ok || m1 == nil {
		t.Fatal("live fit must resolve a model")
	}
	m2, ok := p.Model(ctx, liveAsOf)
	if !ok || m2 != m1 {
		t.Error("same as-of must resolve the SAME cached fit (per-snapshot resolution)")
	}
	if universeCalls != 1 {
		t.Errorf("cached resolution must not refit: universe called %d times", universeCalls)
	}
	m3, ok := p.Model(ctx, liveAsOf.Add(24*time.Hour))
	if !ok || m3 == m1 {
		t.Error("a different as-of must refit")
	}
}

func TestLiveModelProvider_FailuresYieldNoModel(t *testing.T) {
	ctx := context.Background()
	rp := trendReturns{"A": 0.001}
	base := factormodel.Providers{Returns: rp, Characteristics: NewStoreCharacteristicProvider(rp, nil, CharacteristicConfig{Window: 40})}
	cfg := factormodel.Config{StyleFactors: []string{StyleMomentum}, Window: 40}

	failing := NewLiveModelProvider(cfg, func(context.Context, time.Time) ([]string, error) {
		return nil, fmt.Errorf("book unavailable")
	}, base)
	if _, ok := failing.Model(ctx, liveAsOf); ok {
		t.Error("universe error must yield ok=false")
	}

	empty := NewLiveModelProvider(cfg, func(context.Context, time.Time) ([]string, error) {
		return nil, nil
	}, base)
	if _, ok := empty.Model(ctx, liveAsOf); ok {
		t.Error("empty universe must yield ok=false")
	}

	badFit := NewLiveModelProvider(cfg, func(context.Context, time.Time) ([]string, error) {
		return []string{"GHOST"}, nil // no history ⇒ Fit errors
	}, base)
	if _, ok := badFit.Model(ctx, liveAsOf); ok {
		t.Error("fit failure must yield ok=false, not a broken model")
	}
}
