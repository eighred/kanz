package volsurface

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/risk/pricing/pit"
)

// THE VOL STORE'S RETENTION IS BOUNDED BY ITS HORIZON, NOT BY UPTIME (#811).
// The third mirror; see curve/retention_test.go for the cost, and pit's package
// doc for why the horizon is anchored on the newest retained version.
func TestStoreRetentionIsBoundedByTheHorizon(t *testing.T) {
	const horizon = 24 * time.Hour
	st := NewStore(WithHorizon(horizon))
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const refreshes = 10 * 24 * 60
	peak := 0
	for i := 0; i < refreshes; i++ {
		st.Put("SPX", base.Add(time.Duration(i)*time.Minute), flatSurface(0.04))
		st.mu.RLock()
		if n := len(st.byUnderlying["SPX"]); n > peak {
			peak = n
		}
		st.mu.RUnlock()
	}
	st.mu.RLock()
	held := len(st.byUnderlying["SPX"])
	st.mu.RUnlock()

	const want = 24*60 + 1
	if held != want {
		t.Fatalf("after %d one-minute refreshes under a %s horizon the store holds %d surfaces, "+
			"want %d — retention still tracks uptime", refreshes, horizon, held, want)
	}
	if peak != want {
		t.Fatalf("retention PEAKED at %d surfaces, want %d — peak memory is still a function of "+
			"how long the pod has run", peak, want)
	}
}

// AND A SURFACE INSIDE THE HORIZON IS STILL THERE. An evicted surface is not a
// crash: compute.Greeks reports SkipNoVol and the position's vega and gamma
// leave the book's measured risk, in the direction that makes a limit pass.
func TestStoreServesAVersionInsideTheHorizon(t *testing.T) {
	const horizon = 7 * 24 * time.Hour
	st := NewStore(WithHorizon(horizon))
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i <= 14; i++ {
		st.Put("SPX", base.AddDate(0, 0, i), flatSurface(float64(i+1)))
	}
	newest := base.AddDate(0, 0, 14)

	got, ok := st.Vol(ctx, "SPX", 100, 1, newest.AddDate(0, 0, -6))
	if !ok {
		t.Fatal("a surface six days inside a 7d horizon did not resolve — the horizon evicted an " +
			"answer a revaluation at that as-of needs")
	}
	if want := math.Sqrt(9); math.Abs(got-want) > 1e-12 {
		t.Fatalf("resolved vol %v, want %v — the read picked up the wrong version", got, want)
	}
	if _, ok := st.Vol(ctx, "SPX", 100, 1, newest.AddDate(0, 0, -8)); ok {
		t.Fatal("an as-of outside the horizon resolved a surface; it must report not-known")
	}
}

// AN UNCONFIGURED STORE GETS THE DOCUMENTED DEFAULT, NOT "FOREVER".
func TestStoreDefaultsToTheSharedHorizon(t *testing.T) {
	for name, st := range map[string]*Store{
		"NewStore()":        NewStore(),
		"WithHorizon(0)":    NewStore(WithHorizon(0)),
		"WithHorizon(-1ns)": NewStore(WithHorizon(-time.Nanosecond)),
	} {
		if st.horizon != pit.DefaultHorizon {
			t.Errorf("%s built a store with horizon %s, want pit.DefaultHorizon %s", name, st.horizon, pit.DefaultHorizon)
		}
	}
}
