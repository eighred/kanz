package risk

import (
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	v1 "github.com/kanz-eng/kanz/internal/risk/api/v1"
	"github.com/kanz-eng/kanz/internal/risk/domain"
)

// fixedNow lets every test set its own clock for the Detector
// without relying on time.Now wall-clock arithmetic in assertions.
func detectorAt(now time.Time) *Detector {
	d := NewDetector()
	d.now = func() time.Time { return now }
	return d
}

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func TestAssess_FreshStateIsModeNormalNoFlags(t *testing.T) {
	d := detectorAt(t0)
	mode, flags := d.Assess(t0.Add(-10 * time.Second)) // 10s old
	if mode != v1.ModeNormal {
		t.Errorf("mode=%v want ModeNormal", mode)
	}
	if len(flags) != 0 {
		t.Errorf("flags=%v want empty (within freshness budget)", flags)
	}
}

func TestAssess_OverFreshnessBudgetIsStaleButNormal(t *testing.T) {
	d := detectorAt(t0)
	// 60s old, freshness budget = 30s, degraded threshold = 5m
	mode, flags := d.Assess(t0.Add(-60 * time.Second))
	if mode != v1.ModeNormal {
		t.Errorf("mode=%v want ModeNormal (below degraded threshold)", mode)
	}
	if len(flags) != 1 || flags[0] != v1.QualityFlagStale {
		t.Errorf("flags=%v want [STALE]", flags)
	}
}

func TestAssess_OverDegradedThresholdIsDegradedAndStale(t *testing.T) {
	d := detectorAt(t0)
	// 10m old, exceeds degraded threshold (5m).
	mode, flags := d.Assess(t0.Add(-10 * time.Minute))
	if mode != v1.ModeDegraded {
		t.Errorf("mode=%v want ModeDegraded", mode)
	}
	if len(flags) != 2 {
		t.Fatalf("flags=%v want 2 entries (DEGRADED + STALE)", flags)
	}
	seen := map[v1.QualityFlag]bool{flags[0]: true, flags[1]: true}
	if !seen[v1.QualityFlagDegraded] || !seen[v1.QualityFlagStale] {
		t.Errorf("flags=%v want both DEGRADED and STALE", flags)
	}
}

func TestAssess_ZeroAsOfIsDegradedRegardlessOfThresholds(t *testing.T) {
	// No state ever applied for this portfolio — even a freshly-
	// started engine should signal degraded so callers don't read
	// "no data" as "zero risk".
	d := detectorAt(t0)
	mode, flags := d.Assess(time.Time{})
	if mode != v1.ModeDegraded {
		t.Errorf("mode=%v want ModeDegraded (zero AsOf ⇒ no state)", mode)
	}
	if len(flags) != 1 || flags[0] != v1.QualityFlagDegraded {
		t.Errorf("flags=%v want [DEGRADED]", flags)
	}
}

func TestAssess_BoundaryEqualsBudgetIsFresh(t *testing.T) {
	// Staleness EQUAL to budget should be FRESH (the boundary is
	// "above the budget" not "at the budget"). Easy off-by-one
	// regression catch.
	d := detectorAt(t0)
	mode, flags := d.Assess(t0.Add(-DefaultFreshnessBudget))
	if mode != v1.ModeNormal {
		t.Errorf("mode=%v want ModeNormal at exact budget", mode)
	}
	if len(flags) != 0 {
		t.Errorf("flags=%v want empty at exact budget", flags)
	}
}

func TestAssess_CustomThresholdsApplied(t *testing.T) {
	d := detectorAt(t0)
	d.FreshnessBudget = 1 * time.Second
	d.DegradedThreshold = 5 * time.Second
	// 3s old: above 1s budget, below 5s threshold ⇒ Normal + STALE
	mode, flags := d.Assess(t0.Add(-3 * time.Second))
	if mode != v1.ModeNormal || len(flags) != 1 {
		t.Errorf("mode=%v flags=%v want (Normal, [STALE])", mode, flags)
	}
	// 10s old: above 5s threshold ⇒ Degraded
	mode, flags = d.Assess(t0.Add(-10 * time.Second))
	if mode != v1.ModeDegraded || len(flags) != 2 {
		t.Errorf("mode=%v flags=%v want (Degraded, 2 flags)", mode, flags)
	}
}

func TestAssess_NegativeOrZeroThresholdsFallBackToDefaults(t *testing.T) {
	d := detectorAt(t0)
	d.FreshnessBudget = 0
	d.DegradedThreshold = -1 * time.Second
	// 10s old — under default 30s budget ⇒ Normal, no flags.
	mode, flags := d.Assess(t0.Add(-10 * time.Second))
	if mode != v1.ModeNormal || len(flags) != 0 {
		t.Errorf("mode=%v flags=%v want (Normal, []) — defaults not applied", mode, flags)
	}
}

func TestHealth_ComposesModeAndStaleness(t *testing.T) {
	d := detectorAt(t0)
	latest := t0.Add(-2 * time.Minute)
	h := d.Health(latest)
	if h.Mode != v1.ModeNormal {
		t.Errorf("Mode=%v want ModeNormal (below degraded threshold)", h.Mode)
	}
	if !h.AsOf.Equal(latest) {
		t.Errorf("AsOf=%v want %v", h.AsOf, latest)
	}
	if h.Staleness != 2*time.Minute {
		t.Errorf("Staleness=%v want 2m", h.Staleness)
	}
}

func TestHealth_ZeroAsOfHasZeroStaleness(t *testing.T) {
	d := detectorAt(t0)
	h := d.Health(time.Time{})
	if h.Mode != v1.ModeDegraded {
		t.Errorf("Mode=%v want ModeDegraded", h.Mode)
	}
	// Staleness is meaningless when AsOf is zero — must not report
	// a wall-clock gap that would mislead the operator.
	if h.Staleness != 0 {
		t.Errorf("Staleness=%v want 0 for zero AsOf", h.Staleness)
	}
}

// --- Cache tests ------------------------------------------------------

func makeExposure(id v1.PortfolioID, asOf time.Time) *domain.ExposureSet {
	return domain.NewExposureSet(id, asOf, []domain.Exposure{
		{Dimension: domain.ExposureByCurrency, Key: "USD",
			Gross: &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 1, Exponent: 0}, CurrencyCode: "USD"},
			Net:   &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 1, Exponent: 0}, CurrencyCode: "USD"}},
	})
}

func TestCache_StoreAndLookupExposure(t *testing.T) {
	c := NewCache()
	es := makeExposure("PORT-1", t0)
	c.StoreExposure("PORT-1", es)
	got, ok := c.LookupExposure("PORT-1")
	if !ok {
		t.Fatal("not found after store")
	}
	if got != es {
		t.Error("returned different pointer than stored")
	}
}

func TestCache_OverwriteReplacesPrevious(t *testing.T) {
	c := NewCache()
	first := makeExposure("PORT-1", t0)
	second := makeExposure("PORT-1", t0.Add(time.Hour))
	c.StoreExposure("PORT-1", first)
	c.StoreExposure("PORT-1", second)
	got, _ := c.LookupExposure("PORT-1")
	if !got.AsOf().Equal(t0.Add(time.Hour)) {
		t.Errorf("AsOf=%v want second-store AsOf", got.AsOf())
	}
}

func TestCache_LookupMissReturnsFalse(t *testing.T) {
	c := NewCache()
	if _, ok := c.LookupExposure("MISSING"); ok {
		t.Error("expected miss")
	}
	if _, ok := c.LookupMeasures("MISSING"); ok {
		t.Error("expected miss")
	}
}

func TestCache_NilStoreIsNoOp(t *testing.T) {
	c := NewCache()
	c.StoreExposure("PORT-1", nil) // must not crash
	c.StoreMeasures("PORT-1", nil)
	if _, ok := c.LookupExposure("PORT-1"); ok {
		t.Error("nil store wrote a phantom entry")
	}
}

func TestCache_MeasuresStoreAndLookup(t *testing.T) {
	c := NewCache()
	ms := domain.NewMeasureSet("PORT-1", t0, map[v1.MeasureName]v1.Measure{
		"VaR99": {Name: "VaR99", Value: &commonpb.Decimal{Coefficient: 100, Exponent: 0}},
	})
	c.StoreMeasures("PORT-1", ms)
	got, ok := c.LookupMeasures("PORT-1")
	if !ok || got != ms {
		t.Errorf("Lookup got=%v ok=%v want stored ms", got, ok)
	}
}

func TestCache_AsOfPropagatedThroughLookup(t *testing.T) {
	// Cached value's AsOf is what Detector.Assess uses — fixture
	// here exercises the orchestrator-side composition path.
	c := NewCache()
	stale := t0.Add(-10 * time.Minute) // beyond degraded threshold
	c.StoreExposure("PORT-1", makeExposure("PORT-1", stale))

	d := detectorAt(t0)
	es, ok := c.LookupExposure("PORT-1")
	if !ok {
		t.Fatal("expected hit")
	}
	mode, flags := d.Assess(es.AsOf())
	if mode != v1.ModeDegraded {
		t.Errorf("cached-AsOf assessment mode=%v want ModeDegraded", mode)
	}
	if len(flags) != 2 {
		t.Errorf("flags=%v want [DEGRADED, STALE]", flags)
	}
}
