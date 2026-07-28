package engine_test

import (
	"context"
	"math"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	domainpb "github.com/kanz-eng/kanz-schemas-go/domain/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

	risk "github.com/eighred/kanz/internal/risk"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/engine"
	"github.com/eighred/kanz/internal/risk/state"
)

// The Detector compares state AsOf against the real wall clock and its
// `now` field is unexported (only the package-internal RISK-13 tests
// mock it). External tests therefore anchor AsOf relative to time.Now()
// with margins far larger than the 30s freshness / 5m degraded
// thresholds, so classification is stable without a mock clock.
func money(coef int64, ccy string) *commonpb.Money {
	return &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: coef}, CurrencyCode: ccy}
}

func decValue(d *commonpb.Decimal) float64 {
	if d == nil {
		return 0
	}
	return float64(d.Coefficient) * math.Pow10(int(d.Exponent))
}

func applyPosition(t *testing.T, s *state.Store, id, instrument string, value int64, asOf time.Time) {
	t.Helper()
	ctx := context.Background()
	// Seed the portfolio's BaseCurrency so USD positions are summed by the
	// base-currency measures (idempotent per portfolio via a fixed key).
	if err := s.ApplyPortfolioRevalued(ctx,
		&envelopepb.Envelope{EventId: "evt-port-" + id, IdempotencyKey: "evt-port-" + id},
		&domainpb.PortfolioState{PortfolioId: id, BaseCurrency: "USD", AsOf: timestamppb.New(asOf)},
	); err != nil {
		t.Fatalf("ApplyPortfolioRevalued: %v", err)
	}
	key := "evt-" + id + "-" + instrument
	err := s.ApplyPositionChanged(ctx,
		&envelopepb.Envelope{EventId: key, IdempotencyKey: key},
		&domainpb.PositionState{
			PortfolioId:  id,
			InstrumentId: instrument,
			MarketValue:  money(value, "USD"),
			AsOf:         timestamppb.New(asOf),
		})
	if err != nil {
		t.Fatalf("ApplyPositionChanged: %v", err)
	}
}

func newEngine() (*engine.EngineImpl, *state.Store) {
	s := state.NewStore()
	return engine.New(s, compute.DefaultRegistry(), risk.NewCache(), risk.NewDetector()), s
}

func TestExposure_FreshStateNoFlags(t *testing.T) {
	e, s := newEngine()
	applyPosition(t, s, "PORT-1", "AAPL", 1000, time.Now().Add(-1*time.Second))

	resp, err := e.Exposure(context.Background(), v1.ExposureRequest{PortfolioID: "PORT-1"})
	if err != nil {
		t.Fatalf("Exposure: %v", err)
	}
	if len(resp.QualityFlags) != 0 {
		t.Errorf("flags=%v want empty (fresh)", resp.QualityFlags)
	}
}

func TestExposure_DegradedWhenStale(t *testing.T) {
	e, s := newEngine()
	applyPosition(t, s, "PORT-1", "AAPL", 1000, time.Now().Add(-10*time.Minute))

	resp, err := e.Exposure(context.Background(), v1.ExposureRequest{PortfolioID: "PORT-1"})
	if err != nil {
		t.Fatalf("Exposure: %v", err)
	}
	if len(resp.QualityFlags) != 2 {
		t.Fatalf("flags=%v want [DEGRADED, STALE]", resp.QualityFlags)
	}
}

func TestExposure_UnknownPortfolioNotFound(t *testing.T) {
	e, _ := newEngine()
	_, err := e.Exposure(context.Background(), v1.ExposureRequest{PortfolioID: "NOPE"})
	if err != v1.ErrPortfolioNotFound {
		t.Fatalf("err=%v want ErrPortfolioNotFound", err)
	}
}

func TestExposure_EmptyIDInvalid(t *testing.T) {
	e, _ := newEngine()
	_, err := e.Exposure(context.Background(), v1.ExposureRequest{})
	if err != v1.ErrInvalidRequest {
		t.Fatalf("err=%v want ErrInvalidRequest", err)
	}
}

// Repeat queries return a stable result and keep the cache warm — the
// store-hit branch that also populates the degraded-fallback cache.
func TestExposure_RepeatQueryStable(t *testing.T) {
	e, s := newEngine()
	asOf := time.Now().Add(-1 * time.Second)
	applyPosition(t, s, "PORT-1", "AAPL", 1000, asOf)

	first, err := e.Exposure(context.Background(), v1.ExposureRequest{PortfolioID: "PORT-1"})
	if err != nil {
		t.Fatalf("first Exposure: %v", err)
	}
	second, err := e.Exposure(context.Background(), v1.ExposureRequest{PortfolioID: "PORT-1"})
	if err != nil {
		t.Fatalf("second Exposure: %v", err)
	}
	if !first.AsOf.Equal(second.AsOf) {
		t.Errorf("AsOf drift between queries: %v vs %v", first.AsOf, second.AsOf)
	}
}

func TestMeasures_FilterNarrowsResponse(t *testing.T) {
	e, s := newEngine()
	applyPosition(t, s, "PORT-1", "AAPL", 1000, time.Now().Add(-1*time.Second))

	resp, err := e.Measures(context.Background(), v1.MeasuresRequest{
		PortfolioID: "PORT-1",
		Measures:    []v1.MeasureName{compute.MeasureGrossExposure},
	})
	if err != nil {
		t.Fatalf("Measures: %v", err)
	}
	if _, ok := resp.Set.Lookup(compute.MeasureGrossExposure); !ok {
		t.Error("GrossExposure missing from filtered response")
	}
	if _, ok := resp.Set.Lookup(compute.MeasureVaR99); ok {
		t.Error("VaR99 present despite filter excluding it")
	}
}

func TestMeasures_EmptyFilterReturnsAll(t *testing.T) {
	e, s := newEngine()
	applyPosition(t, s, "PORT-1", "AAPL", 1000, time.Now().Add(-1*time.Second))

	resp, err := e.Measures(context.Background(), v1.MeasuresRequest{PortfolioID: "PORT-1"})
	if err != nil {
		t.Fatalf("Measures: %v", err)
	}
	for _, n := range compute.DefaultRegistry().Names() {
		if _, ok := resp.Set.Lookup(n); !ok {
			t.Errorf("measure %s missing from unfiltered response", n)
		}
	}
}

func TestEvaluateScenario_ShockProjectsMeasures(t *testing.T) {
	e, s := newEngine()
	applyPosition(t, s, "PORT-1", "AAPL", 1000, time.Now().Add(-1*time.Second))

	base, _ := e.Measures(context.Background(), v1.MeasuresRequest{PortfolioID: "PORT-1"})
	baseGross, _ := base.Set.Lookup(compute.MeasureGrossExposure)

	// -50% parallel shift halves every position's market value.
	resp, err := e.EvaluateScenario(context.Background(), v1.ScenarioRequest{
		PortfolioID: "PORT-1",
		Shocks:      []v1.ScenarioShock{v1.ParallelShift{Pct: &commonpb.Decimal{Coefficient: -5, Exponent: -1}}},
	})
	if err != nil {
		t.Fatalf("EvaluateScenario: %v", err)
	}
	shockedGross, ok := resp.Projected.Lookup(compute.MeasureGrossExposure)
	if !ok {
		t.Fatal("GrossExposure missing from scenario projection")
	}
	if decValue(shockedGross.Value) >= decValue(baseGross.Value) {
		t.Errorf("shocked gross %v not below base %v after -50%% shift",
			decValue(shockedGross.Value), decValue(baseGross.Value))
	}
	// Scenario must not mutate live state — a follow-up query is unchanged.
	after, _ := e.Measures(context.Background(), v1.MeasuresRequest{PortfolioID: "PORT-1"})
	afterGross, _ := after.Set.Lookup(compute.MeasureGrossExposure)
	if decValue(afterGross.Value) != decValue(baseGross.Value) {
		t.Errorf("live state mutated by scenario: %v != %v",
			decValue(afterGross.Value), decValue(baseGross.Value))
	}
}

func TestEvaluateScenario_UnknownPortfolioNotFound(t *testing.T) {
	e, _ := newEngine()
	_, err := e.EvaluateScenario(context.Background(), v1.ScenarioRequest{PortfolioID: "NOPE"})
	if err != v1.ErrPortfolioNotFound {
		t.Fatalf("err=%v want ErrPortfolioNotFound", err)
	}
}

func TestHealth_LatestAsOfAcrossPortfolios(t *testing.T) {
	e, s := newEngine()
	newAsOf := time.Now().Add(-1 * time.Second)
	applyPosition(t, s, "PORT-OLD", "X", 1, time.Now().Add(-1*time.Hour))
	applyPosition(t, s, "PORT-NEW", "Z", 1, newAsOf)

	h, err := e.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.Mode != v1.ModeNormal {
		t.Errorf("Mode=%v want ModeNormal (PORT-NEW within budget)", h.Mode)
	}
	if !h.AsOf.Equal(newAsOf) {
		t.Errorf("AsOf=%v want %v (latest across portfolios)", h.AsOf, newAsOf)
	}
}

func TestHealth_NoStateIsDegraded(t *testing.T) {
	e, _ := newEngine()
	h, err := e.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.Mode != v1.ModeDegraded {
		t.Errorf("Mode=%v want ModeDegraded (no state applied)", h.Mode)
	}
}

func TestCancelledContextRejected(t *testing.T) {
	e, s := newEngine()
	applyPosition(t, s, "PORT-1", "AAPL", 1000, time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Exposure(ctx, v1.ExposureRequest{PortfolioID: "PORT-1"}); err != context.Canceled {
		t.Fatalf("err=%v want context.Canceled", err)
	}
}

// applySnapshot applies a PortfolioSnapshot carrying a durable-log
// LogPosition — the only apply path that sets the portfolio's fold
// coordinate (RISK-05), which WIRE-03 surfaces as SourcePosition.
func applySnapshot(t *testing.T, s *state.Store, id, instrument string, value int64, asOf time.Time, lp *commonpb.LogPosition) {
	t.Helper()
	err := s.ApplyPortfolioSnapshot(context.Background(),
		&envelopepb.Envelope{EventId: "snap-" + id, IdempotencyKey: "snap-" + id},
		&domainpb.PortfolioSnapshot{
			Portfolio: &domainpb.PortfolioState{PortfolioId: id, BaseCurrency: "USD", AsOf: timestamppb.New(asOf)},
			Positions: []*domainpb.PositionState{{
				PortfolioId:  id,
				InstrumentId: instrument,
				MarketValue:  money(value, "USD"),
				AsOf:         timestamppb.New(asOf),
			}},
			LogPosition: lp,
		})
	if err != nil {
		t.Fatalf("ApplyPortfolioSnapshot: %v", err)
	}
}

// WIRE-03: a query served from live state carries the portfolio's applied
// snapshot LogPosition as SourcePosition — the citation seed.
func TestExposure_SourcePositionFromSnapshot(t *testing.T) {
	e, s := newEngine()
	lp := &commonpb.LogPosition{Topic: "risk.state", Partition: 3, Offset: 4242}
	applySnapshot(t, s, "PORT-1", "AAPL", 1000, time.Now().Add(-1*time.Second), lp)

	resp, err := e.Exposure(context.Background(), v1.ExposureRequest{PortfolioID: "PORT-1"})
	if err != nil {
		t.Fatalf("Exposure: %v", err)
	}
	if resp.SourcePosition == nil {
		t.Fatal("SourcePosition nil, want the applied snapshot position")
	}
	if got := resp.SourcePosition; got.Topic != "risk.state" || got.Partition != 3 || got.Offset != 4242 {
		t.Fatalf("SourcePosition = %+v, want risk.state/3/4242", got)
	}
}

func TestMeasures_SourcePositionFromSnapshot(t *testing.T) {
	e, s := newEngine()
	lp := &commonpb.LogPosition{Topic: "risk.state", Partition: 1, Offset: 99}
	applySnapshot(t, s, "PORT-1", "AAPL", 1000, time.Now().Add(-1*time.Second), lp)

	resp, err := e.Measures(context.Background(), v1.MeasuresRequest{PortfolioID: "PORT-1"})
	if err != nil {
		t.Fatalf("Measures: %v", err)
	}
	if resp.SourcePosition == nil || resp.SourcePosition.Offset != 99 {
		t.Fatalf("SourcePosition = %+v, want offset 99", resp.SourcePosition)
	}
}

// A portfolio built only from incremental applies (no positioned snapshot)
// has no log anchor: SourcePosition is nil, not a zero coordinate.
func TestExposure_SourcePositionNilWithoutSnapshot(t *testing.T) {
	e, s := newEngine()
	applyPosition(t, s, "PORT-1", "AAPL", 1000, time.Now().Add(-1*time.Second))

	resp, err := e.Exposure(context.Background(), v1.ExposureRequest{PortfolioID: "PORT-1"})
	if err != nil {
		t.Fatalf("Exposure: %v", err)
	}
	if resp.SourcePosition != nil {
		t.Fatalf("SourcePosition = %+v, want nil (no snapshot applied)", resp.SourcePosition)
	}
}
