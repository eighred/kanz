package curve

import (
	"context"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/pit"
)

func flat(z float64) *Curve {
	c, err := NewZeroCurve([]float64{1, 5}, []float64{z, z}, Continuous, LinearZero)
	if err != nil {
		panic(err)
	}
	return c
}

// THE CURVE STORE'S RETENTION IS BOUNDED BY ITS HORIZON, NOT BY UPTIME (#811).
//
// Before this, every calibration refresh appended a version and nothing removed
// one: `grep -rn "delete(" internal/risk/pricing` returned nothing. The
// composition root registers an intraday AND a nightly job per currency
// (services/risk-engine/cmd/risk-engine/main.go), so at the one-minute cadence
// RISK_ENGINE_CALIBRATION_INTERVAL accepts, the store held ~525k *Curve values
// per currency per year and the pod's pricing inputs degraded with how long it
// had been up.
func TestStoreRetentionIsBoundedByTheHorizon(t *testing.T) {
	const horizon = 24 * time.Hour
	s := NewStore(WithHorizon(horizon))
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Ten days of one-minute refreshes — ten times the horizon, so a store that
	// grew more slowly rather than stopping would still fail here.
	const refreshes = 10 * 24 * 60
	peak := 0
	for i := 0; i < refreshes; i++ {
		s.Put("USD", base.Add(time.Duration(i)*time.Minute), flat(0.02))
		s.mu.RLock()
		if n := len(s.byCurrency["USD"]); n > peak {
			peak = n
		}
		s.mu.RUnlock()
	}
	s.mu.RLock()
	held := len(s.byCurrency["USD"])
	s.mu.RUnlock()

	const want = 24*60 + 1 // the newest, plus the horizon's worth behind it
	if held != want {
		t.Fatalf("after %d one-minute refreshes under a %s horizon the store holds %d curves, "+
			"want %d — retention still tracks uptime", refreshes, horizon, held, want)
	}
	if peak != want {
		t.Fatalf("retention PEAKED at %d curves, want %d: the store grew past its horizon and was "+
			"trimmed afterwards, so peak memory is still a function of how long the pod has run",
			peak, want)
	}
}

// AND A CURVE INSIDE THE HORIZON IS STILL THERE. A bound that evicted the curve
// a valuation needs would swap an unbounded store for a wrong risk number: a
// bond whose currency has no curve is SKIPPED, which reports DV01 zero and is
// indistinguishable from a book holding no bonds.
func TestStoreServesAVersionInsideTheHorizon(t *testing.T) {
	const horizon = 7 * 24 * time.Hour
	s := NewStore(WithHorizon(horizon))
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i <= 14; i++ {
		s.Put("USD", base.AddDate(0, 0, i), flat(0.01*float64(i)))
	}
	newest := base.AddDate(0, 0, 14)

	got, ok := s.Curve(ctx, "USD", newest.AddDate(0, 0, -6))
	if !ok {
		t.Fatal("a curve six days inside a 7d horizon did not resolve — the horizon evicted an " +
			"answer a valuation at that as-of needs")
	}
	if want := 0.08; !nearly(got.Zero(1), want) {
		t.Fatalf("resolved a curve at z=%v, want %v — the read picked up the wrong version", got.Zero(1), want)
	}
	if _, ok := s.Curve(ctx, "USD", newest.AddDate(0, 0, -8)); ok {
		t.Fatal("an as-of outside the horizon resolved a curve; it must report not-known so the " +
			"caller refuses rather than pricing off a curve from a week after the state it holds")
	}
}

// AN UNCONFIGURED STORE GETS THE DOCUMENTED DEFAULT, NOT "FOREVER" — the whole
// point of routing every option through pit.Horizon.
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

func nearly(a, b float64) bool { return a-b < 1e-12 && b-a < 1e-12 }
