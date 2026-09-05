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

// THE FIT CADENCE, AND WHY EACH ARM IS LOAD-BEARING (#1039).
//
// The fit used to key on the exact requested as-of, so a book receiving events a
// second apart produced a distinct model instance per event. That is defensible
// while the model is a transient and indefensible once it is an artifact
// somebody cites: model_id + as_of would name a different thing every recompute.

func cadenceProvider(t *testing.T, cadence time.Duration, observed *[]*factormodel.Model, universeCalls *int) *LiveModelProvider {
	t.Helper()
	rp := trendReturns{"A": 0.001, "B": -0.0005, "C": 0.0002}
	universe := func(_ context.Context, _ time.Time) ([]string, error) {
		*universeCalls++
		return []string{"A", "B", "C"}, nil
	}
	return NewLiveModelProvider(
		factormodel.Config{Type: factormodel.Statistical, StatFactors: 2, Window: 40},
		universe,
		factormodel.Providers{Returns: rp},
		WithFitCadence(cadence),
		WithFitObserver(func(_ context.Context, m *factormodel.Model) { *observed = append(*observed, m) }),
	)
}

func TestLiveModelProvider_FitsOncePerCadencePeriod(t *testing.T) {
	ctx := context.Background()
	var observed []*factormodel.Model
	calls := 0
	p := cadenceProvider(t, 24*time.Hour, &observed, &calls)

	first, ok := p.Model(ctx, liveAsOf)
	if !ok {
		t.Fatal("first evaluation must resolve a model")
	}
	// Later in the SAME period — a different portfolio's as-of, minutes or hours
	// on. One instance must serve them all, or "the model that priced this" names
	// a fit per recompute.
	for _, d := range []time.Duration{time.Minute, 3 * time.Hour, 23*time.Hour + 59*time.Minute} {
		got, ok := p.Model(ctx, liveAsOf.Add(d))
		if !ok {
			t.Fatalf("evaluation at +%v must resolve a model", d)
		}
		if got != first {
			t.Errorf("evaluation at +%v refitted: one cadence period must resolve ONE instance", d)
		}
	}
	if calls != 1 {
		t.Errorf("universe called %d times in one period, want 1 — the cadence is not bounding the fit", calls)
	}
	if len(observed) != 1 {
		t.Fatalf("fit observer fired %d times, want 1 — the artifact record must carry one "+
			"message per instance, not one per evaluation", len(observed))
	}
	if observed[0] != first {
		t.Error("the observed model is not the one served — the published artifact would name an " +
			"instance no measure used")
	}

	// The next period refits, and records the new instance.
	next, ok := p.Model(ctx, liveAsOf.Add(24*time.Hour))
	if !ok || next == first {
		t.Error("a new cadence period must produce a new instance")
	}
	if len(observed) != 2 {
		t.Errorf("fit observer fired %d times across two periods, want 2", len(observed))
	}
	if !next.AsOf.Equal(liveAsOf.Add(24 * time.Hour)) {
		t.Errorf("instance as_of=%v want %v — the as-of must be the instant the estimator read "+
			"to, never a period boundary it did not", next.AsOf, liveAsOf.Add(24*time.Hour))
	}
}

func TestLiveModelProvider_AnEarlierAsOfInThePeriodIsNotServedALaterFit(t *testing.T) {
	ctx := context.Background()
	var observed []*factormodel.Model
	calls := 0
	p := cadenceProvider(t, 24*time.Hour, &observed, &calls)

	late, _ := p.Model(ctx, liveAsOf.Add(9*time.Hour))
	early, ok := p.Model(ctx, liveAsOf.Add(time.Hour))
	if !ok {
		t.Fatal("an earlier evaluation in the period must still resolve a model")
	}
	// THE NO-LOOK-AHEAD ARM. Reusing the 09:00 fit for an 01:00 valuation prices
	// the book off data its own state had not seen.
	if early == late {
		t.Fatal("an evaluation at +1h was served a model fitted at +9h — the model read data " +
			"the priced state had not seen")
	}
	if early.AsOf.After(liveAsOf.Add(time.Hour)) {
		t.Errorf("model as_of=%v postdates the evaluation as_of=%v", early.AsOf, liveAsOf.Add(time.Hour))
	}
	// And the period's instance is not displaced by the out-of-order request:
	// the model must not walk backwards for everybody else.
	again, _ := p.Model(ctx, liveAsOf.Add(11*time.Hour))
	if again != late {
		t.Error("the period's instance was replaced by an out-of-order earlier fit")
	}
}

func TestLiveModelProvider_CadenceRefusesToFitEveryTime(t *testing.T) {
	var observed []*factormodel.Model
	calls := 0
	// Zero and negative select the default rather than "fit every evaluation" —
	// there is deliberately no way to spell the state #1039 found.
	for _, d := range []time.Duration{0, -time.Second} {
		p := cadenceProvider(t, d, &observed, &calls)
		if got := p.epoch(liveAsOf.Add(time.Hour)); got != p.epoch(liveAsOf) {
			t.Errorf("cadence %v put two same-day as-ofs in different periods — a zero or "+
				"negative cadence must fall back to DefaultFitCadence", d)
		}
	}
}
