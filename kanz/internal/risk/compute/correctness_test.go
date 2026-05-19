package compute_test

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	v1 "github.com/kanz-eng/kanz/internal/risk/api/v1"
	"github.com/kanz-eng/kanz/internal/risk/compute"
	"github.com/kanz-eng/kanz/internal/risk/domain"
)

// RISK-12 — mathematical-property tests on top of RISK-06/07/08's
// unit tests. Each property runs across N seeded random portfolios
// rather than handcrafted fixtures, so a regression that only
// fails for specific input shapes (negative coefficients, mixed
// exponents, single-position portfolios, etc.) still surfaces.
//
// # Why not testing/quick
//
// stdlib testing/quick generates via reflection — works for plain
// types but cannot synthesise *commonpb.Decimal proto pointers
// without a custom Generator on a type we don't own. The seeded-
// loop approach gives the same property-test ergonomics without
// fighting the proto-type-as-input constraint, and the
// deterministic seed means failures are reproducible by re-running
// with the same seed range.
//
// # Property iteration count
//
// propertyIters = 50 — enough to catch input-shape-dependent
// regressions in a few-second CI run; loose enough that no single
// property dominates compute test time. Increase locally to
// stress-test a suspected edge.

const propertyIters = 50

// dValue converts a Decimal to float64 for property comparisons —
// uncertainty bands and order assertions don't need money-grade
// precision, and float64 sidesteps writing a Decimal comparator.
// Same approach RISK-08's uncertainty tests use.
func dValue(d *commonpb.Decimal) float64 {
	if d == nil {
		return 0
	}
	return float64(d.Coefficient) * math.Pow10(int(d.Exponent))
}

// randomPortfolio builds a USD portfolio with n positions and
// randomly-signed MarketValue. Quantities are coarse (±1M.00) so
// the int64 coefficient×coefficient products don't overflow in
// multi-shock chains, which is the same bound RISK-07's overflow
// note flags for the production placeholder formulas.
func randomPortfolio(rng *rand.Rand, n int) *domain.Portfolio {
	p := domain.NewPortfolio("PORT-RAND", "USD")
	p.SetAggregate(domain.AggregateUpdate{
		AsOf:         time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		BaseCurrency: "USD",
	})
	for i := 0; i < n; i++ {
		// MarketValue range roughly -10000.00 .. +10000.00
		coef := rng.Int63n(2_000_000) - 1_000_000
		p.SetPosition(domain.Position{
			InstrumentID: domain.InstrumentID(fmt.Sprintf("INST-%d", i)),
			MarketValue: &commonpb.Money{
				Amount:       &commonpb.Decimal{Coefficient: coef, Exponent: -2},
				CurrencyCode: "USD",
			},
			AsOf: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		})
	}
	return p
}

// --- Mathematical invariants ------------------------------------------

// |NetExposure| ≤ GrossExposure for any portfolio. Triangle
// inequality: |Σx_i| ≤ Σ|x_i|. A regression that breaks this is
// almost always an abs-vs-signed mix-up in compute internals.
func TestProperty_NetAbsLeqGross(t *testing.T) {
	for seed := int64(1); seed <= propertyIters; seed++ {
		rng := rand.New(rand.NewSource(seed))
		p := randomPortfolio(rng, rng.Intn(20)+1)
		gross := dValue(compute.GrossExposure(p).Value)
		net := dValue(compute.NetExposure(p).Value)
		if math.Abs(net) > gross+1e-9 {
			t.Errorf("seed=%d: |Net|=%v > Gross=%v (triangle inequality)", seed, math.Abs(net), gross)
		}
	}
}

// GrossExposure ≥ 0 always — gross is sum-of-abs, never negative.
func TestProperty_GrossNonNegative(t *testing.T) {
	for seed := int64(1); seed <= propertyIters; seed++ {
		rng := rand.New(rand.NewSource(seed))
		p := randomPortfolio(rng, rng.Intn(20)+1)
		g := dValue(compute.GrossExposure(p).Value)
		if g < 0 {
			t.Errorf("seed=%d: Gross=%v negative", seed, g)
		}
	}
}

// VaR99 / GrossExposure = 0.01 exactly (within precision) — the
// RISK-07 placeholder formula. A regression that decouples them
// (e.g. a real VaR model that doesn't match the placeholder
// scale) should INTENTIONALLY break this test, which is the
// failure-as-documentation signal that VaR is no longer the
// placeholder.
func TestProperty_VaR99IsOnePercentOfGross(t *testing.T) {
	for seed := int64(1); seed <= propertyIters; seed++ {
		rng := rand.New(rand.NewSource(seed))
		p := randomPortfolio(rng, rng.Intn(20)+1)
		gross := dValue(compute.GrossExposure(p).Value)
		varV := dValue(compute.VaR99(p).Value)
		if gross == 0 {
			if varV != 0 {
				t.Errorf("seed=%d: VaR=%v with zero Gross", seed, varV)
			}
			continue
		}
		ratio := varV / gross
		if math.Abs(ratio-0.01) > 1e-9 {
			t.Errorf("seed=%d: VaR/Gross=%v want 0.01 (placeholder formula)", seed, ratio)
		}
	}
}

// Delta == NetExposure (RISK-07 placeholder identity). Same
// failure-as-documentation contract as VaR99.
func TestProperty_DeltaEqualsNetExposure(t *testing.T) {
	for seed := int64(1); seed <= propertyIters; seed++ {
		rng := rand.New(rand.NewSource(seed))
		p := randomPortfolio(rng, rng.Intn(20)+1)
		d := dValue(compute.Delta(p).Value)
		n := dValue(compute.NetExposure(p).Value)
		if math.Abs(d-n) > 1e-9 {
			t.Errorf("seed=%d: Delta=%v != NetExposure=%v", seed, d, n)
		}
	}
}

// Permutation invariance — measures must not depend on the order
// positions appear in the portfolio. SetPosition writes to a map
// internally so Go's randomized map iteration is already exercising
// this, but the property test pins the invariant.
func TestProperty_MeasuresInvariantUnderPositionOrder(t *testing.T) {
	for seed := int64(1); seed <= propertyIters; seed++ {
		rng := rand.New(rand.NewSource(seed))
		n := rng.Intn(15) + 2
		// Build the same set twice in different orders.
		positions := make([]domain.Position, n)
		for i := 0; i < n; i++ {
			coef := rng.Int63n(2_000_000) - 1_000_000
			positions[i] = domain.Position{
				InstrumentID: domain.InstrumentID(fmt.Sprintf("INST-%d", i)),
				MarketValue: &commonpb.Money{
					Amount:       &commonpb.Decimal{Coefficient: coef, Exponent: -2},
					CurrencyCode: "USD",
				},
				AsOf: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			}
		}

		p1 := domain.NewPortfolio("PORT-1", "USD")
		p1.SetAggregate(domain.AggregateUpdate{AsOf: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), BaseCurrency: "USD"})
		for _, pos := range positions {
			p1.SetPosition(pos)
		}

		p2 := domain.NewPortfolio("PORT-2", "USD")
		p2.SetAggregate(domain.AggregateUpdate{AsOf: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), BaseCurrency: "USD"})
		rng.Shuffle(len(positions), func(i, j int) {
			positions[i], positions[j] = positions[j], positions[i]
		})
		for _, pos := range positions {
			p2.SetPosition(pos)
		}

		for _, m := range []v1.MeasureName{
			compute.MeasureGrossExposure,
			compute.MeasureNetExposure,
			compute.MeasureVaR99,
			compute.MeasureDelta,
		} {
			s1 := compute.ComputeMeasures(p1, nil, []v1.MeasureName{m})
			s2 := compute.ComputeMeasures(p2, nil, []v1.MeasureName{m})
			a, _ := s1.Lookup(m)
			b, _ := s2.Lookup(m)
			if math.Abs(dValue(a.Value)-dValue(b.Value)) > 1e-9 {
				t.Errorf("seed=%d %s: ordered=%v shuffled=%v (permutation-variant)", seed, m, dValue(a.Value), dValue(b.Value))
			}
		}
	}
}

// Adding a zero-value position is a no-op for every measure.
// Defensive against a future bug where "Position exists" gets
// conflated with "Position contributes."
func TestProperty_ZeroValuePositionIsNoOp(t *testing.T) {
	for seed := int64(1); seed <= propertyIters; seed++ {
		rng := rand.New(rand.NewSource(seed))
		p := randomPortfolio(rng, rng.Intn(10)+1)
		before := compute.ComputeMeasures(p, nil, nil)

		p.SetPosition(domain.Position{
			InstrumentID: "ZERO",
			MarketValue: &commonpb.Money{
				Amount:       &commonpb.Decimal{Coefficient: 0, Exponent: 0},
				CurrencyCode: "USD",
			},
			AsOf: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		})
		after := compute.ComputeMeasures(p, nil, nil)

		for _, m := range before.Names() {
			a, _ := before.Lookup(m)
			b, _ := after.Lookup(m)
			if math.Abs(dValue(a.Value)-dValue(b.Value)) > 1e-9 {
				t.Errorf("seed=%d %s: changed by zero-value position (%v → %v)", seed, m, dValue(a.Value), dValue(b.Value))
			}
		}
	}
}

// Empty portfolio yields zero for every measure (no positions, no
// mark-to-market values, no exposure).
func TestProperty_EmptyPortfolioHasZeroMeasures(t *testing.T) {
	p := domain.NewPortfolio("PORT-EMPTY", "USD")
	p.SetAggregate(domain.AggregateUpdate{
		AsOf: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), BaseCurrency: "USD",
	})
	for _, m := range []v1.MeasureName{
		compute.MeasureGrossExposure,
		compute.MeasureNetExposure,
		compute.MeasureVaR99,
		compute.MeasureDelta,
	} {
		fn := map[v1.MeasureName]compute.MeasureFunc{
			compute.MeasureGrossExposure: compute.GrossExposure,
			compute.MeasureNetExposure:   compute.NetExposure,
			compute.MeasureVaR99:         compute.VaR99,
			compute.MeasureDelta:         compute.Delta,
		}[m]
		v := dValue(fn(p).Value)
		if v != 0 {
			t.Errorf("%s on empty portfolio = %v want 0", m, v)
		}
	}
}

// Currency-bucket Net values sum to the portfolio's total Net
// (single-currency case — no FX). The property pins RISK-06's
// per-currency aggregation: the bucket totals must reconcile to
// what a separate sum-across-positions would produce.
func TestProperty_CurrencyBucketsSumToTotal(t *testing.T) {
	for seed := int64(1); seed <= propertyIters; seed++ {
		rng := rand.New(rand.NewSource(seed))
		p := randomPortfolio(rng, rng.Intn(15)+1)
		es := compute.ComputeExposure(p)

		var bucketSum float64
		for _, e := range es.ByDimension(domain.ExposureByCurrency) {
			bucketSum += dValue(e.Net.Amount)
		}
		total := dValue(compute.NetExposure(p).Value)
		if math.Abs(bucketSum-total) > 1e-9 {
			t.Errorf("seed=%d: Σ currency-bucket Net = %v != total Net = %v", seed, bucketSum, total)
		}
	}
}

// --- Uncertainty invariants -------------------------------------------

// Cauchy-Schwarz: √(Σσ²) ≤ Σ|σ| for any non-negative σ_i. The
// independent-correlation propagation must never exceed the
// perfectly-correlated propagation — if it does, the conservative-
// vs-textbook semantics are reversed.
func TestProperty_IndependentUncertaintyLeqCorrelated(t *testing.T) {
	for seed := int64(1); seed <= propertyIters; seed++ {
		rng := rand.New(rand.NewSource(seed))
		n := rng.Intn(10) + 1
		inputs := make([]*commonpb.Decimal, n)
		for i := 0; i < n; i++ {
			inputs[i] = &commonpb.Decimal{Coefficient: rng.Int63n(1000), Exponent: -2}
		}
		ind := dValue(compute.PropagateSumIndependent(inputs...))
		cor := dValue(compute.PropagateSumPerfectlyCorrelated(inputs...))
		if ind > cor+1e-6 {
			t.Errorf("seed=%d: independent=%v > correlated=%v (Cauchy-Schwarz violated)", seed, ind, cor)
		}
	}
}

// PropagateScalar(c, σ) = |c| × |σ| exactly. No rounding tolerance
// beyond float64 floor since this is exact multiplication, not a
// sqrt path.
func TestProperty_ScalarUncertaintyExact(t *testing.T) {
	for seed := int64(1); seed <= propertyIters; seed++ {
		rng := rand.New(rand.NewSource(seed))
		c := &commonpb.Decimal{Coefficient: rng.Int63n(1000) - 500, Exponent: -2}
		sigma := &commonpb.Decimal{Coefficient: rng.Int63n(1000), Exponent: -2}
		want := math.Abs(dValue(c)) * math.Abs(dValue(sigma))
		got := dValue(compute.PropagateScalar(c, sigma))
		if math.Abs(got-want) > 1e-9 {
			t.Errorf("seed=%d: scalar propagation got=%v want=%v", seed, got, want)
		}
	}
}

// Uncertainty propagation must be order-invariant (sums commute).
func TestProperty_PropagationOrderInvariant(t *testing.T) {
	for seed := int64(1); seed <= propertyIters; seed++ {
		rng := rand.New(rand.NewSource(seed))
		n := rng.Intn(8) + 2
		a := make([]*commonpb.Decimal, n)
		for i := 0; i < n; i++ {
			a[i] = &commonpb.Decimal{Coefficient: rng.Int63n(500), Exponent: -2}
		}
		b := make([]*commonpb.Decimal, n)
		copy(b, a)
		rng.Shuffle(n, func(i, j int) { b[i], b[j] = b[j], b[i] })

		x := dValue(compute.PropagateSumIndependent(a...))
		y := dValue(compute.PropagateSumIndependent(b...))
		if math.Abs(x-y) > 1e-6 {
			t.Errorf("seed=%d: ordered=%v shuffled=%v (sum should commute)", seed, x, y)
		}
	}
}

// Deterministic recomputation — same portfolio twice yields
// numerically identical measures. Catches any nondeterminism in
// the compute path (e.g. accidental map-iteration dependency on a
// random walk).
func TestProperty_ComputeIsDeterministic(t *testing.T) {
	for seed := int64(1); seed <= propertyIters; seed++ {
		rng := rand.New(rand.NewSource(seed))
		p := randomPortfolio(rng, rng.Intn(10)+1)
		first := compute.ComputeMeasures(p, nil, nil)
		second := compute.ComputeMeasures(p, nil, nil)
		for _, name := range first.Names() {
			a, _ := first.Lookup(name)
			b, _ := second.Lookup(name)
			if dValue(a.Value) != dValue(b.Value) {
				t.Errorf("seed=%d %s: nondeterministic (%v vs %v)", seed, name, dValue(a.Value), dValue(b.Value))
			}
		}
	}
}
