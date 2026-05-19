package risk

// RISK-13 — cross-package integration tests for degraded-mode
// behavior + uncertainty propagation. Distinct from the
// individual-component tests in compute/, state/, degraded_test.go
// — those exercise one package in isolation; this file exercises
// the *composition* the future orchestrator will perform:
// state.Store → compute → Cache → Detector → response flags.
//
// Lives in `package risk` (internal test) so it can drive
// Detector.now for time-mocking without expanding the public
// surface with a NewDetectorWithClock constructor.
//
// # Scope vs the orchestrator
//
// The orchestrator (which doesn't exist yet — separate task)
// owns the actual EngineImpl that satisfies v1.Engine. These tests
// inline the composition logic to (a) show the integration works,
// (b) double as documentation of the pattern the orchestrator
// should implement. When the orchestrator lands, these tests
// should be rewritten against EngineImpl.Exposure / .Measures /
// .Health directly — the composition shifts from test code to
// production code.

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	domainpb "github.com/kanz-eng/kanz-schemas-go/domain/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

	v1 "github.com/kanz-eng/kanz/internal/risk/api/v1"
	"github.com/kanz-eng/kanz/internal/risk/compute"
	"github.com/kanz-eng/kanz/internal/risk/domain"
	"github.com/kanz-eng/kanz/internal/risk/state"
)

var integT0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func integDetector(now time.Time) *Detector {
	d := NewDetector()
	d.now = func() time.Time { return now }
	return d
}

func integMoney(coef int64, exp int32, ccy string) *commonpb.Money {
	return &commonpb.Money{
		Amount:       &commonpb.Decimal{Coefficient: coef, Exponent: exp},
		CurrencyCode: ccy,
	}
}

// applyState is a tiny wrapper around the state.Store apply path
// that simulates one RISK-04 ingest hop — used to construct realistic
// per-portfolio state without spelling out envelope plumbing in
// every test.
func applyState(t *testing.T, s *state.Store, id string, asOf time.Time, positions ...*domainpb.PositionState) {
	t.Helper()
	ctx := context.Background()
	err := s.ApplyPortfolioRevalued(ctx, &envelopepb.Envelope{
		EventId: "evt-port-" + id, IdempotencyKey: "evt-port-" + id,
	}, &domainpb.PortfolioState{
		PortfolioId:  id,
		BaseCurrency: "USD",
		AsOf:         timestamppb.New(asOf),
	})
	if err != nil {
		t.Fatalf("ApplyPortfolioRevalued: %v", err)
	}
	for i, pos := range positions {
		key := "evt-pos-" + id + "-" + pos.InstrumentId
		// distinct idempotency_key per call so dedup doesn't swallow
		_ = i
		if err := s.ApplyPositionChanged(ctx,
			&envelopepb.Envelope{EventId: key, IdempotencyKey: key},
			pos,
		); err != nil {
			t.Fatalf("ApplyPositionChanged: %v", err)
		}
	}
}

// computeExposureWithFlags is the canonical orchestrator composition
// pattern: try live, on success cache + tag via Detector; on
// "portfolio missing" fall back to cache. Production orchestrator
// will own this logic — for now it lives here so the integration
// tests exercise the documented flow.
func computeExposureWithFlags(
	store *state.Store, cache *Cache, det *Detector, id v1.PortfolioID, computeLive bool,
) (*domain.ExposureSet, v1.Mode, []v1.QualityFlag, bool) {
	var es *domain.ExposureSet
	if computeLive {
		p, found := store.Lookup(id)
		if found {
			es = compute.ComputeExposure(p)
			cache.StoreExposure(id, es)
		}
	}
	if es == nil {
		c, ok := cache.LookupExposure(id)
		if !ok {
			return nil, v1.ModeDegraded, []v1.QualityFlag{v1.QualityFlagDegraded}, false
		}
		es = c
	}
	mode, flags := det.Assess(es.AsOf())
	return es, mode, flags, true
}

// --- Degraded-mode integration scenarios ------------------------------

// Fresh state ⇒ no flags, ModeNormal — the happy path showing
// every piece composes correctly when nothing is stale.
func TestIntegration_FreshStateNoFlags(t *testing.T) {
	store := state.NewStore()
	cache := NewCache()
	det := integDetector(integT0)

	applyState(t, store, "PORT-1", integT0.Add(-10*time.Second), &domainpb.PositionState{
		PortfolioId:  "PORT-1",
		InstrumentId: "AAPL",
		MarketValue:  integMoney(1000, 0, "USD"),
		AsOf:         timestamppb.New(integT0.Add(-10 * time.Second)),
	})

	_, mode, flags, ok := computeExposureWithFlags(store, cache, det, "PORT-1", true)
	if !ok {
		t.Fatal("compute failed")
	}
	if mode != v1.ModeNormal {
		t.Errorf("mode=%v want ModeNormal", mode)
	}
	if len(flags) != 0 {
		t.Errorf("flags=%v want empty (fresh state)", flags)
	}
}

// State older than the freshness budget but younger than the
// degraded threshold ⇒ STALE flag but mode stays Normal — the
// intermediate "trustworthy but slightly old" signal.
func TestIntegration_OverBudgetStaleNotDegraded(t *testing.T) {
	store := state.NewStore()
	cache := NewCache()
	det := integDetector(integT0)

	applyState(t, store, "PORT-1", integT0.Add(-1*time.Minute), &domainpb.PositionState{
		PortfolioId:  "PORT-1",
		InstrumentId: "AAPL",
		MarketValue:  integMoney(1000, 0, "USD"),
		AsOf:         timestamppb.New(integT0.Add(-1 * time.Minute)),
	})

	_, mode, flags, _ := computeExposureWithFlags(store, cache, det, "PORT-1", true)
	if mode != v1.ModeNormal {
		t.Errorf("mode=%v want ModeNormal", mode)
	}
	if len(flags) != 1 || flags[0] != v1.QualityFlagStale {
		t.Errorf("flags=%v want [STALE]", flags)
	}
}

// State older than the degraded threshold ⇒ ModeDegraded + both
// flags. The "engine is now unreliable, caller decides whether to
// act" signal.
func TestIntegration_OverDegradedThresholdFlipsMode(t *testing.T) {
	store := state.NewStore()
	cache := NewCache()
	det := integDetector(integT0)

	applyState(t, store, "PORT-1", integT0.Add(-10*time.Minute), &domainpb.PositionState{
		PortfolioId:  "PORT-1",
		InstrumentId: "AAPL",
		MarketValue:  integMoney(1000, 0, "USD"),
		AsOf:         timestamppb.New(integT0.Add(-10 * time.Minute)),
	})

	_, mode, flags, _ := computeExposureWithFlags(store, cache, det, "PORT-1", true)
	if mode != v1.ModeDegraded {
		t.Errorf("mode=%v want ModeDegraded", mode)
	}
	if len(flags) != 2 {
		t.Fatalf("flags=%v want [DEGRADED, STALE]", flags)
	}
}

// Cache fallback: the canonical degraded path. Live compute is
// skipped (simulating dependency failure / budget exceeded); the
// cached value carries its own AsOf which the Detector evaluates
// against the current wall clock. Demonstrates Cache + Detector
// composing correctly under failure.
func TestIntegration_CacheFallbackTagsDegradedFromCachedAsOf(t *testing.T) {
	store := state.NewStore()
	cache := NewCache()

	// Step 1: at T-10m, state is fresh and we compute + cache.
	det := integDetector(integT0.Add(-10 * time.Minute))
	applyState(t, store, "PORT-1", integT0.Add(-10*time.Minute), &domainpb.PositionState{
		PortfolioId:  "PORT-1",
		InstrumentId: "AAPL",
		MarketValue:  integMoney(1000, 0, "USD"),
		AsOf:         timestamppb.New(integT0.Add(-10 * time.Minute)),
	})
	_, _, _, _ = computeExposureWithFlags(store, cache, det, "PORT-1", true)

	// Step 2: at T0 (10 minutes later), live compute is unavailable.
	// Cache returns the value cached at T-10m; Detector tags it.
	det2 := integDetector(integT0)
	es, mode, flags, ok := computeExposureWithFlags(store, cache, det2, "PORT-1", false /*no live*/)
	if !ok {
		t.Fatal("expected cache hit")
	}
	if !es.AsOf().Equal(integT0.Add(-10 * time.Minute)) {
		t.Errorf("cached AsOf=%v want T-10m", es.AsOf())
	}
	if mode != v1.ModeDegraded {
		t.Errorf("mode=%v want ModeDegraded (cache age exceeds threshold)", mode)
	}
	if len(flags) != 2 {
		t.Errorf("flags=%v want [DEGRADED, STALE]", flags)
	}
}

// Unknown portfolio (no state ever applied, no cache entry) — the
// "true degraded, no fallback" case. Returns degraded with the
// "not found" signal the orchestrator translates to
// ErrPortfolioNotFound.
func TestIntegration_UnknownPortfolioReturnsDegradedNotFound(t *testing.T) {
	store := state.NewStore()
	cache := NewCache()
	det := integDetector(integT0)

	_, mode, flags, ok := computeExposureWithFlags(store, cache, det, "UNKNOWN", true)
	if ok {
		t.Error("expected not-found path")
	}
	if mode != v1.ModeDegraded {
		t.Errorf("mode=%v want ModeDegraded", mode)
	}
	if len(flags) != 1 || flags[0] != v1.QualityFlagDegraded {
		t.Errorf("flags=%v want [DEGRADED]", flags)
	}
}

// Engine.Health across multiple portfolios — latest AsOf is the
// max over all aggregates (per the v1.Health doc). A single
// healthy portfolio carries the engine even when others are stale,
// which is intentional: the engine is healthy if ingestion is
// alive somewhere, even if specific portfolios are quiet.
func TestIntegration_HealthAggregatesLatestAsOfAcrossPortfolios(t *testing.T) {
	store := state.NewStore()
	det := integDetector(integT0)

	// Three portfolios, varying staleness.
	applyState(t, store, "PORT-OLD", integT0.Add(-1*time.Hour), &domainpb.PositionState{
		PortfolioId: "PORT-OLD", InstrumentId: "X", MarketValue: integMoney(1, 0, "USD"),
		AsOf: timestamppb.New(integT0.Add(-1 * time.Hour)),
	})
	applyState(t, store, "PORT-MID", integT0.Add(-2*time.Minute), &domainpb.PositionState{
		PortfolioId: "PORT-MID", InstrumentId: "Y", MarketValue: integMoney(1, 0, "USD"),
		AsOf: timestamppb.New(integT0.Add(-2 * time.Minute)),
	})
	applyState(t, store, "PORT-NEW", integT0.Add(-5*time.Second), &domainpb.PositionState{
		PortfolioId: "PORT-NEW", InstrumentId: "Z", MarketValue: integMoney(1, 0, "USD"),
		AsOf: timestamppb.New(integT0.Add(-5 * time.Second)),
	})

	var latest time.Time
	for _, id := range store.IDs() {
		p, _ := store.Lookup(id)
		if p.AsOf().After(latest) {
			latest = p.AsOf()
		}
	}
	h := det.Health(latest)
	if h.Mode != v1.ModeNormal {
		t.Errorf("Mode=%v want ModeNormal (PORT-NEW within budget)", h.Mode)
	}
	if !h.AsOf.Equal(integT0.Add(-5 * time.Second)) {
		t.Errorf("AsOf=%v want T-5s (latest across portfolios)", h.AsOf)
	}
}

// --- Uncertainty propagation integration -----------------------------

// Uncertainty flows from per-position MarketValueUncertainty
// through the compute pipeline into measures' UncertaintyAbs. The
// wire proto (domain.v1.PositionState) does not yet carry an
// uncertainty field, so the engine populates Position
// .MarketValueUncertainty post-apply (RISK-11 staleness synthesis
// or a future schema addition). Here we set it directly to
// demonstrate the algorithm is wired end-to-end.
func TestIntegration_UncertaintyPropagatesThroughCompute(t *testing.T) {
	store := state.NewStore()
	applyState(t, store, "PORT-1", integT0, &domainpb.PositionState{
		PortfolioId:  "PORT-1",
		InstrumentId: "A",
		MarketValue:  integMoney(100, 0, "USD"),
		AsOf:         timestamppb.New(integT0),
	}, &domainpb.PositionState{
		PortfolioId:  "PORT-1",
		InstrumentId: "B",
		MarketValue:  integMoney(200, 0, "USD"),
		AsOf:         timestamppb.New(integT0),
	})

	// Post-apply, populate uncertainty (simulating RISK-11's role).
	p, _ := store.Lookup("PORT-1")
	for _, pos := range p.Positions() {
		pos.MarketValueUncertainty = integMoney(3, 0, "USD")
		p.SetPosition(pos)
	}

	measures := compute.ComputeMeasures(p, nil, nil)
	gross, _ := measures.Lookup(compute.MeasureGrossExposure)
	if gross.UncertaintyAbs == nil {
		t.Fatal("Gross UncertaintyAbs nil (propagation broken)")
	}
	// Two positions, σ_1=σ_2=3 → √(9+9) = √18 ≈ 4.243
	got := float64(gross.UncertaintyAbs.Coefficient) * pow10ForExp(gross.UncertaintyAbs.Exponent)
	want := 4.2426 // √18
	if abs(got-want) > 0.01 {
		t.Errorf("Gross UncertaintyAbs=%v want ≈ %v (√(3² + 3²))", got, want)
	}
}

// Without per-position uncertainty inputs, propagated
// UncertaintyAbs MUST stay nil (not zero) on every measure — the
// "no signal" vs "calibrated zero" distinction documented at the
// api/v1 boundary.
func TestIntegration_NoUncertaintyInputsLeavesAllUncertaintyAbsNil(t *testing.T) {
	store := state.NewStore()
	applyState(t, store, "PORT-1", integT0, &domainpb.PositionState{
		PortfolioId:  "PORT-1",
		InstrumentId: "A",
		MarketValue:  integMoney(100, 0, "USD"),
		AsOf:         timestamppb.New(integT0),
	})

	p, _ := store.Lookup("PORT-1")
	measures := compute.ComputeMeasures(p, nil, nil)
	for _, n := range measures.Names() {
		m, _ := measures.Lookup(n)
		if m.UncertaintyAbs != nil {
			t.Errorf("measure %s UncertaintyAbs=%v want nil (no inputs ⇒ no propagation signal)", n, m.UncertaintyAbs)
		}
	}
}

// Mixed uncertainty inputs — some positions with, some without —
// propagate using only the populated ones. Defends the "skip nil"
// rule in PropagateSumIndependent from a regression that would
// treat nil as zero (and falsely tighten the band).
func TestIntegration_MixedUncertaintyInputsSkipNil(t *testing.T) {
	store := state.NewStore()
	applyState(t, store, "PORT-1", integT0, &domainpb.PositionState{
		PortfolioId: "PORT-1", InstrumentId: "WithUnc", MarketValue: integMoney(100, 0, "USD"), AsOf: timestamppb.New(integT0),
	}, &domainpb.PositionState{
		PortfolioId: "PORT-1", InstrumentId: "WithoutUnc", MarketValue: integMoney(50, 0, "USD"), AsOf: timestamppb.New(integT0),
	})

	p, _ := store.Lookup("PORT-1")
	withUnc, _ := p.Position("WithUnc")
	withUnc.MarketValueUncertainty = integMoney(4, 0, "USD")
	p.SetPosition(withUnc)
	// "WithoutUnc" stays at MarketValueUncertainty = nil.

	gross, _ := compute.ComputeMeasures(p, nil, nil).Lookup(compute.MeasureGrossExposure)
	if gross.UncertaintyAbs == nil {
		t.Fatal("UncertaintyAbs nil despite one input carrying uncertainty")
	}
	// Only one σ=4 contributes → √16 = 4. The nil-input one is skipped.
	got := float64(gross.UncertaintyAbs.Coefficient) * pow10ForExp(gross.UncertaintyAbs.Exponent)
	if abs(got-4.0) > 0.001 {
		t.Errorf("UncertaintyAbs=%v want 4 (only WithUnc contributes; nil must be skipped, not treated as 0)", got)
	}
}

// --- tiny helpers (kept local to avoid cross-test-file coupling) ----

func pow10ForExp(e int32) float64 {
	out := 1.0
	if e >= 0 {
		for i := int32(0); i < e; i++ {
			out *= 10
		}
		return out
	}
	for i := int32(0); i < -e; i++ {
		out /= 10
	}
	return out
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
