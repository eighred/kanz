package credit

import (
	"context"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/risk/pricing/pit"
	"github.com/eighred/kanz/internal/risk/xva"
)

func hazard(h float64) *xva.CreditCurve {
	c := xva.FlatHazard(h, 0.4)
	return &c
}

// THE CREDIT STORE'S RETENTION IS BOUNDED BY ITS HORIZON, NOT BY UPTIME (#811).
// The mirror of curve.TestStoreRetentionIsBoundedByTheHorizon, and the mirror is
// the point: this store is the one that says in its own package doc that it
// copies the other two "down to the method names", so the bound has to be
// verified on all three or the next reader copies whichever one was missed.
func TestStoreRetentionIsBoundedByTheHorizon(t *testing.T) {
	const horizon = 24 * time.Hour
	s := NewStore(WithHorizon(horizon))
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const refreshes = 10 * 24 * 60
	peak := 0
	for i := 0; i < refreshes; i++ {
		s.Put("ACME", base.Add(time.Duration(i)*time.Minute), hazard(0.02))
		s.mu.RLock()
		if n := len(s.byReference["ACME"]); n > peak {
			peak = n
		}
		s.mu.RUnlock()
	}
	s.mu.RLock()
	held := len(s.byReference["ACME"])
	s.mu.RUnlock()

	const want = 24*60 + 1
	if held != want {
		t.Fatalf("after %d one-minute refreshes under a %s horizon the store holds %d curves, "+
			"want %d — retention still tracks uptime", refreshes, horizon, held, want)
	}
	if peak != want {
		t.Fatalf("retention PEAKED at %d curves, want %d — peak memory is still a function of "+
			"how long the pod has run", peak, want)
	}
}

// AND A CURVE INSIDE THE HORIZON IS STILL THERE. Here the cost of getting this
// wrong is the one the store's own comment names: FALSE IS NOT A ZERO CURVE. A
// missing hazard curve must be refused by the caller, so evicting one inside the
// horizon turns a reproducible CVA into a refusal.
func TestStoreServesAVersionInsideTheHorizon(t *testing.T) {
	const horizon = 7 * 24 * time.Hour
	s := NewStore(WithHorizon(horizon))
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i <= 14; i++ {
		s.Put("ACME", base.AddDate(0, 0, i), hazard(0.01*float64(i+1)))
	}
	newest := base.AddDate(0, 0, 14)

	got, ok := s.Curve(ctx, "ACME", newest.AddDate(0, 0, -6))
	if !ok {
		t.Fatal("a curve six days inside a 7d horizon did not resolve — the horizon evicted an " +
			"answer a CVA at that as-of needs")
	}
	if want := hazard(0.09); got.Survival(5) != want.Survival(5) {
		t.Fatalf("resolved S(5)=%v, want %v — the read picked up the wrong version",
			got.Survival(5), want.Survival(5))
	}
	if _, ok := s.Curve(ctx, "ACME", newest.AddDate(0, 0, -8)); ok {
		t.Fatal("an as-of outside the horizon resolved a curve; it must report not-known")
	}
}

// AN UNCONFIGURED STORE GETS THE DOCUMENTED DEFAULT, NOT "FOREVER".
func TestStoreDefaultsToTheSharedHorizon(t *testing.T) {
	for name, s := range map[string]*Store{
		"NewStore()":        NewStore(),
		"WithHorizon(0)":    NewStore(WithHorizon(0)),
		"WithHorizon(-1ns)": NewStore(WithHorizon(-time.Nanosecond)),
	} {
		if s.horizon != pit.DefaultHorizon {
			t.Errorf("%s built a store with horizon %s, want pit.DefaultHorizon %s", name, s.horizon, pit.DefaultHorizon)
		}
	}
}
