package compute

import (
	"context"
	decutil "github.com/eighred/kanz/internal/dec"
	"math"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/liquidity"
)

type staticLiquidity map[string]liquidity.LiquiditySpec

func (m staticLiquidity) Liquidity(_ context.Context, id string, _ time.Time) (liquidity.LiquiditySpec, bool) {
	s, ok := m[id]
	return s, ok
}

func liqTestBook(t *testing.T) (*domain.Portfolio, liquidity.Provider) {
	t.Helper()
	asOf := time.Date(2026, 6, 27, 0, 0, 0, 0, time.UTC)
	p := domain.NewPortfolio("p1", "USD")
	p.SetPosition(domain.Position{InstrumentID: "FAST", Quantity: dec(100_000, 0), MarketValue: &commonpb.Money{Amount: dec(1_000_000, 0), CurrencyCode: "USD"}, AsOf: asOf})
	p.SetPosition(domain.Position{InstrumentID: "SLOW", Quantity: dec(100_000, 0), MarketValue: &commonpb.Money{Amount: dec(500_000, 0), CurrencyCode: "USD"}, AsOf: asOf})
	prov := staticLiquidity{
		"FAST": {ADV: 1_000_000, Spread: 0.0005}, // 0.5 days
		"SLOW": {ADV: 100_000, Spread: 0.0010},   // 5 days
	}
	return p, prov
}

func TestRegisterLiquidityRisk_LVaRNeverBelowVaR(t *testing.T) {
	p, prov := liqTestBook(t)
	r := DefaultRegistry()
	RegisterLiquidityRisk(context.Background(), r, prov, liquidity.DefaultModel(), nil) // nil ⇒ VaR99 placeholder

	ms := ComputeMeasures(p, r, nil)
	v, ok := ms.Lookup(MeasureVaR99)
	if !ok {
		t.Fatal("VaR99 missing")
	}
	lv, ok := ms.Lookup(MeasureLVaR99)
	if !ok {
		t.Fatal("LVaR99 missing")
	}
	if decutil.Float64Or(lv.Value, 0) < decutil.Float64Or(v.Value, 0) {
		t.Fatalf("LVaR99 must be ≥ VaR99: %.2f < %.2f", decutil.Float64Or(lv.Value, 0), decutil.Float64Or(v.Value, 0))
	}
	// With a real liquidation cost the two diverge (LVaR strictly above VaR).
	if decutil.Float64Or(lv.Value, 0) <= decutil.Float64Or(v.Value, 0) {
		t.Fatalf("LVaR99 should exceed VaR99 given a positive liquidation cost")
	}
}

func TestRegisterLiquidityRisk_Horizon(t *testing.T) {
	p, prov := liqTestBook(t)
	r := DefaultRegistry()
	RegisterLiquidityRisk(context.Background(), r, prov, liquidity.DefaultModel(), nil)

	ms := ComputeMeasures(p, r, nil)
	h, ok := ms.Lookup(MeasureLiquidationHorizon)
	if !ok {
		t.Fatal("LiquidationHorizon missing")
	}
	// (1,000,000×0.5 + 500,000×5)/1,500,000 = 2.0 days.
	if d := math.Abs(decutil.Float64Or(h.Value, 0) - 2.0); d > 1e-3 {
		t.Fatalf("weighted horizon: got %.4f want 2.0", decutil.Float64Or(h.Value, 0))
	}
}

func TestRegisterLiquidityRisk_StressWidens(t *testing.T) {
	p, prov := liqTestBook(t)
	base := DefaultRegistry()
	RegisterLiquidityRisk(context.Background(), base, prov, liquidity.DefaultModel(), nil)
	baseMS := ComputeMeasures(p, base, nil)

	stressed := DefaultRegistry()
	stress := liquidity.Stress{SpreadMult: 3, ADVMult: 1.0 / 3.0}
	RegisterLiquidityRisk(context.Background(), stressed, stress.Wrap(prov), liquidity.DefaultModel(), nil)
	stressedMS := ComputeMeasures(p, stressed, nil)

	bh, _ := baseMS.Lookup(MeasureLiquidationHorizon)
	sh, _ := stressedMS.Lookup(MeasureLiquidationHorizon)
	if decutil.Float64Or(sh.Value, 0) <= decutil.Float64Or(bh.Value, 0) {
		t.Fatalf("stress must widen horizon: %.4f !> %.4f", decutil.Float64Or(sh.Value, 0), decutil.Float64Or(bh.Value, 0))
	}
	bl, _ := baseMS.Lookup(MeasureLVaR99)
	sl, _ := stressedMS.Lookup(MeasureLVaR99)
	if decutil.Float64Or(sl.Value, 0) <= decutil.Float64Or(bl.Value, 0) {
		t.Fatalf("stress must raise LVaR: %.2f !> %.2f", decutil.Float64Or(sl.Value, 0), decutil.Float64Or(bl.Value, 0))
	}
}

// ===== THE HORIZON-ONLY REGISTRATION PATH (#509) =====
//
// LiquidationHorizon reads ADV, which this repository MEASURES. LVaR99 reads a
// spread, which nothing here persists — so under a no-spread provider
// LiquidationCost is exactly 0 and LVaR99 is exactly VaR99, on every book,
// forever. These prove the honest measure can reach the wire WITHOUT the
// degenerate one riding along, and that the omission is counted rather than
// silent.

// servesSpread wraps a static provider with an explicit SpreadServing answer —
// the shape liquiditysource.Provider presents to this package.
type servesSpread struct {
	staticLiquidity
	serves bool
}

func (s servesSpread) ServesSpread() bool { return s.serves }

// liqObserver collects skip reports so a test can assert the degradation was
// made VISIBLE, not merely that it happened.
type liqObserver struct {
	instr  []string
	reason []string
}

func (o *liqObserver) opt() LiquidityOption {
	return WithLiquidityObserver(func(id, reason string) {
		o.instr = append(o.instr, id)
		o.reason = append(o.reason, reason)
	})
}

func (o *liqObserver) count(reason string) int {
	n := 0
	for _, r := range o.reason {
		if r == reason {
			n++
		}
	}
	return n
}

func TestAProviderThatServesNoSpreadRegistersTheHorizonAndNotLVaR(t *testing.T) {
	p, base := liqTestBook(t)
	prov := servesSpread{staticLiquidity: base.(staticLiquidity), serves: false}

	r := DefaultRegistry()
	RegisterLiquidityRisk(context.Background(), r, prov, liquidity.DefaultModel(), nil)

	ms := ComputeMeasures(p, r, nil)
	if _, ok := ms.Lookup(MeasureLiquidationHorizon); !ok {
		t.Error("LiquidationHorizon must still be served — it is ADV-driven and needs no spread")
	}
	if _, ok := ms.Lookup(MeasureLVaR99); ok {
		t.Error("LVaR99 must NOT be registered for a provider that serves no spread: it would " +
			"equal VaR99 on every book forever")
	}
}

func TestAProviderThatServesASpreadRegistersBothMeasures(t *testing.T) {
	p, base := liqTestBook(t)
	prov := servesSpread{staticLiquidity: base.(staticLiquidity), serves: true}

	r := DefaultRegistry()
	RegisterLiquidityRisk(context.Background(), r, prov, liquidity.DefaultModel(), nil)

	ms := ComputeMeasures(p, r, nil)
	if _, ok := ms.Lookup(MeasureLiquidationHorizon); !ok {
		t.Error("LiquidationHorizon missing")
	}
	if _, ok := ms.Lookup(MeasureLVaR99); !ok {
		t.Error("LVaR99 must be registered when the provider can serve a real spread")
	}
}

// A provider that makes no claim keeps today's behaviour. Every hand-built test
// provider and every liquidity.Stress wrapper is in this case, so the default
// must not silently drop a measure from them.
func TestAProviderThatMakesNoSpreadClaimStillGetsBothMeasures(t *testing.T) {
	p, prov := liqTestBook(t)
	if _, claims := prov.(SpreadServing); claims {
		t.Fatal("the static test provider must not implement SpreadServing, or this proves nothing")
	}

	r := DefaultRegistry()
	RegisterLiquidityRisk(context.Background(), r, prov, liquidity.DefaultModel(), nil)

	ms := ComputeMeasures(p, r, nil)
	if _, ok := ms.Lookup(MeasureLVaR99); !ok {
		t.Error("a provider that declares nothing must keep both measures")
	}
}

// An ABSENT measure is the quietest failure this package has: filterMeasures
// drops unknown names, so a client asking for LVaR99 gets 200 with LVaR99
// missing, indistinguishable from never having asked.
func TestTheOmittedLVaRIsReportedOnceAtRegistration(t *testing.T) {
	_, base := liqTestBook(t)
	prov := servesSpread{staticLiquidity: base.(staticLiquidity), serves: false}

	var obs liqObserver
	RegisterLiquidityRisk(context.Background(), DefaultRegistry(), prov, liquidity.DefaultModel(), nil, obs.opt())

	if n := obs.count(SkipNoSpreadSource); n != 1 {
		t.Fatalf("SkipNoSpreadSource fired %d times, want exactly 1 (registration is a one-shot event); saw %v", n, obs.reason)
	}
	if obs.instr[0] != "" {
		t.Errorf("instrumentID = %q, want empty — this is a property of the wiring, not of an instrument", obs.instr[0])
	}
}

func TestAProviderThatServesASpreadReportsNothingAtRegistration(t *testing.T) {
	_, base := liqTestBook(t)
	prov := servesSpread{staticLiquidity: base.(staticLiquidity), serves: true}

	var obs liqObserver
	RegisterLiquidityRisk(context.Background(), DefaultRegistry(), prov, liquidity.DefaultModel(), nil, obs.opt())

	if n := obs.count(SkipNoSpreadSource); n != 0 {
		t.Errorf("SkipNoSpreadSource fired %d times for a spread-serving provider, want 0", n)
	}
}

// ===== THE ZEROS THAT ARE NOT ANSWERS =====

// The worst zero in this package. Every instrument refused — a wrong venue, an
// empty store, a desk that entered no spread — makes WeightedDays 0, which reads
// as "this book liquidates instantly": the MOST liquid answer the measure can
// give, for a book about which nothing is known.
func TestABookWhereNothingResolvedReportsZeroDaysAsASkipRatherThanAnAnswer(t *testing.T) {
	p, _ := liqTestBook(t)
	var obs liqObserver

	r := DefaultRegistry()
	RegisterLiquidityRisk(context.Background(), r, staticLiquidity{}, liquidity.DefaultModel(), nil, obs.opt())
	ms := ComputeMeasures(p, r, []v1.MeasureName{MeasureLiquidationHorizon})

	h, ok := ms.Lookup(MeasureLiquidationHorizon)
	if !ok {
		t.Fatal("LiquidationHorizon missing")
	}
	if got := decutil.Float64Or(h.Value, 0); got != 0 {
		t.Fatalf("horizon = %v, want 0 — the premise of this test is that the zero is served", got)
	}
	if n := obs.count(SkipNoLiquidHorizon); n != 1 {
		t.Fatalf("SkipNoLiquidHorizon fired %d times, want 1: a zero horizon over nothing must not "+
			"look like a zero horizon over a liquid book; saw %v", n, obs.reason)
	}
}

// A book with no positions liquidates in zero days, and that IS the answer.
func TestAnEmptyBookReportsZeroDaysWithoutASkip(t *testing.T) {
	p := domain.NewPortfolio("empty", "USD")
	var obs liqObserver

	r := DefaultRegistry()
	RegisterLiquidityRisk(context.Background(), r, staticLiquidity{}, liquidity.DefaultModel(), nil, obs.opt())
	ComputeMeasures(p, r, []v1.MeasureName{MeasureLiquidationHorizon})

	if n := obs.count(SkipNoLiquidHorizon); n != 0 {
		t.Errorf("SkipNoLiquidHorizon fired %d times for an empty book, want 0 — a flat book "+
			"really does unwind instantly, and crying wolf here would drown the real signal", n)
	}
}

// An ADV-zero name is EXCLUDED from the weighted horizon (a +Inf horizon has no
// Decimal representation), so a book with one unliquidatable name reports the
// comfortable horizon of the names that can be sold.
func TestAnIlliquidNameIsReportedRatherThanQuietlyLeftOutOfTheHorizon(t *testing.T) {
	p, _ := liqTestBook(t)
	var obs liqObserver

	prov := staticLiquidity{
		"FAST": {ADV: 1_000_000, Spread: 0.0005},
		"SLOW": {ADV: 0, Spread: 0.0010}, // cannot be liquidated at all
	}
	r := DefaultRegistry()
	RegisterLiquidityRisk(context.Background(), r, prov, liquidity.DefaultModel(), nil, obs.opt())
	ComputeMeasures(p, r, []v1.MeasureName{MeasureLiquidationHorizon})

	if n := obs.count(SkipIlliquid); n != 1 {
		t.Fatalf("SkipIlliquid fired %d times, want 1; saw %v", n, obs.reason)
	}
	for i, reason := range obs.reason {
		if reason == SkipIlliquid && obs.instr[i] != "SLOW" {
			t.Errorf("SkipIlliquid attributed to %q, want %q", obs.instr[i], "SLOW")
		}
	}
	if n := obs.count(SkipNoLiquidHorizon); n != 0 {
		t.Errorf("SkipNoLiquidHorizon fired %d times, want 0 — FAST is liquid, so the horizon "+
			"does rest on something", n)
	}
}

// A deployment that declared it supplies spreads, whose table covers nothing on
// this book, serves an LVaR99 that equals VaR99 exactly. Registration-time
// SkipNoSpreadSource cannot see this one; only the evaluation can.
func TestAnLVaRThatEqualsVaRExactlyIsReported(t *testing.T) {
	p, _ := liqTestBook(t)
	var obs liqObserver

	prov := servesSpread{staticLiquidity: staticLiquidity{
		"FAST": {ADV: 1_000_000, Spread: 0},
		"SLOW": {ADV: 100_000, Spread: 0},
	}, serves: true}
	r := DefaultRegistry()
	RegisterLiquidityRisk(context.Background(), r, prov, liquidity.DefaultModel(), nil, obs.opt())
	ms := ComputeMeasures(p, r, []v1.MeasureName{MeasureLVaR99})

	if _, ok := ms.Lookup(MeasureLVaR99); !ok {
		t.Fatal("LVaR99 missing")
	}
	if n := obs.count(SkipNoLiquidationCost); n != 1 {
		t.Fatalf("SkipNoLiquidationCost fired %d times, want 1: an LVaR identical to VaR is the "+
			"degeneracy with no downstream symptom; saw %v", n, obs.reason)
	}
}

func TestARealLiquidationCostIsNotReportedAsDegenerate(t *testing.T) {
	p, prov := liqTestBook(t)
	var obs liqObserver

	r := DefaultRegistry()
	RegisterLiquidityRisk(context.Background(), r, prov, liquidity.DefaultModel(), nil, obs.opt())
	ComputeMeasures(p, r, []v1.MeasureName{MeasureLVaR99})

	if n := obs.count(SkipNoLiquidationCost); n != 0 {
		t.Errorf("SkipNoLiquidationCost fired %d times on a book with real spreads, want 0", n)
	}
}

// ===== THE nil baseVaR TRAP =====

// nil claimed to mean "the registry's current VaR99" and captured the
// package-level compute.VaR99 — the RISK-07 1%-of-gross PLACEHOLDER. Because
// varmodel.Register overrides MeasureVaR99 IN THE REGISTRY, a nil here built
// LVaR on the placeholder while the engine served the historical model: two
// different VaR numbers in one response, and LVaR99 >= VaR99 no longer
// guaranteed.
//
// THE OVERRIDE IS REGISTERED AFTER RegisterLiquidityRisk ON PURPOSE. Resolving
// at evaluation time is what makes the call order stop mattering; an
// early-binding fix would pass a test that registered the model first and fail
// the real composition root.
func TestANilBaseVaRTracksTheRegistrysVaRModelRatherThanThePlaceholder(t *testing.T) {
	p, prov := liqTestBook(t)
	r := DefaultRegistry()
	RegisterLiquidityRisk(context.Background(), r, prov, liquidity.DefaultModel(), nil)

	// The stand-in for varmodel.Register: a real VaR model, registered after.
	const modelVaR = 1_000_000.0
	r.Register(MeasureVaR99, func(*domain.Portfolio) v1.Measure {
		return v1.Measure{Name: MeasureVaR99, Value: floatToDecimal(modelVaR, liqVaRExp)}
	})

	ms := ComputeMeasures(p, r, nil)
	v, _ := ms.Lookup(MeasureVaR99)
	lv, ok := ms.Lookup(MeasureLVaR99)
	if !ok {
		t.Fatal("LVaR99 missing")
	}
	if got := decutil.Float64Or(v.Value, 0); math.Abs(got-modelVaR) > 1e-6 {
		t.Fatalf("VaR99 = %v, want the registered model's %v — the premise of this test", got, modelVaR)
	}
	if got := decutil.Float64Or(lv.Value, 0); got < modelVaR {
		t.Fatalf("LVaR99 = %.2f is BELOW the registry's VaR99 %.2f — nil baseVaR captured the "+
			"1%%-of-gross placeholder instead of the model the engine serves", got, modelVaR)
	}
}

// An explicit baseVaR still wins: the registry is consulted only for nil.
func TestAnExplicitBaseVaRIsNotOverriddenByTheRegistry(t *testing.T) {
	p, prov := liqTestBook(t)
	r := DefaultRegistry()
	const explicit = 42_000.0
	RegisterLiquidityRisk(context.Background(), r, prov, liquidity.DefaultModel(),
		func(*domain.Portfolio) v1.Measure {
			return v1.Measure{Name: MeasureVaR99, Value: floatToDecimal(explicit, liqVaRExp)}
		})
	r.Register(MeasureVaR99, func(*domain.Portfolio) v1.Measure {
		return v1.Measure{Name: MeasureVaR99, Value: floatToDecimal(9_000_000, liqVaRExp)}
	})

	ms := ComputeMeasures(p, r, []v1.MeasureName{MeasureLVaR99})
	lv, _ := ms.Lookup(MeasureLVaR99)
	if got := decutil.Float64Or(lv.Value, 0); got >= 9_000_000 {
		t.Fatalf("LVaR99 = %.2f — an explicitly passed baseVaR must not be replaced by the registry's", got)
	}
	if got := decutil.Float64Or(lv.Value, 0); got < explicit {
		t.Fatalf("LVaR99 = %.2f is below the explicit base %.2f", got, explicit)
	}
}
