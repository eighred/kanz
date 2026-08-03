package compute

import (
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/risk/domain"
)

// RISK-08 — uncertainty propagation through measure computations.
//
// # The api/v1 contract
//
// `v1.Measure.UncertaintyAbs` is "the absolute one-sigma uncertainty
// band around Value." nil ⇒ no uncertainty was propagated (a real
// signal to the caller, not a "treat as zero" trap).
//
// # Two propagation rules
//
// Different correlation assumptions yield different propagation
// math. The two endpoints are the contract:
//
//   - PropagateSumIndependent — σ = √(Σ σ_i²). Assumes inputs are
//     uncorrelated. Standard textbook propagation; widely used when
//     the underlying values are genuinely independent (different
//     sectors, asset classes, uncorrelated factor exposures).
//
//   - PropagateSumPerfectlyCorrelated — σ = Σ |σ_i|. Assumes
//     correlation = 1. The CONSERVATIVE choice when correlations
//     are unknown — overestimates rather than underestimates.
//
// Plus a scalar rule:
//
//   - PropagateScalar — σ(c·X) = |c| × σ(X). Exact, no assumption.
//
// The measure functions (RISK-07) choose which rule to apply per
// measure. The default for cross-position aggregates is
// **independent**; a future iteration can offer a per-measure mode
// or a correlation matrix.
//
// # nil semantics
//
// Every helper treats nil inputs as "no uncertainty present" and
// skips them. When ALL inputs are nil, propagators return nil so
// the api/v1 caller still sees the "no uncertainty propagated"
// signal — the alternative (returning Decimal{0}) would falsely
// claim zero uncertainty rather than absent uncertainty.

// PropagateSumIndependent returns √(Σ σ_i²) — the propagated
// uncertainty when inputs are uncorrelated. nil inputs treated as
// absent; all-nil returns nil so the caller sees "no uncertainty
// propagated" rather than a spurious zero.
func PropagateSumIndependent(uncertainties ...*commonpb.Decimal) *commonpb.Decimal {
	haveAny := false
	varSum := zeroDecimal()
	for _, u := range uncertainties {
		if u == nil {
			continue
		}
		haveAny = true
		varSum = addDecimal(varSum, mulDecimal(u, u))
	}
	if !haveAny {
		return nil
	}
	return decimalSqrt(varSum)
}

// PropagateSumPerfectlyCorrelated returns Σ |σ_i| — the conservative
// propagated uncertainty when correlations are unknown.
func PropagateSumPerfectlyCorrelated(uncertainties ...*commonpb.Decimal) *commonpb.Decimal {
	haveAny := false
	sum := zeroDecimal()
	for _, u := range uncertainties {
		if u == nil {
			continue
		}
		haveAny = true
		sum = addDecimal(sum, absDecimal(u))
	}
	if !haveAny {
		return nil
	}
	return sum
}

// PropagateScalar returns |c| × σ — the uncertainty of a constant-
// scaled measure. Exact, no correlation assumption. nil scalar or
// uncertainty returns nil (caller sees "no propagation").
func PropagateScalar(scalar, uncertainty *commonpb.Decimal) *commonpb.Decimal {
	if scalar == nil || uncertainty == nil {
		return nil
	}
	return mulDecimal(absDecimal(scalar), absDecimal(uncertainty))
}

// sumUncertaintyInBaseCurrency walks positions whose MarketValue
// currency matches BaseCurrency and propagates their
// MarketValueUncertainty values under the independent assumption.
// Same skip rule as sumInBaseCurrency (domain.Position.InBaseCurrency)
// plus the band's own currency check — a position marked in base
// currency can still carry a band in another, and UncertaintyAbs must
// not silently mix units.
//
// Positions dropped by the FIRST condition appear in the set's
// CurrencyExclusions (#257); ones dropped only by the second do not,
// because the measure VALUE still includes them — only its error bar is
// narrower than the truth. That is understatement of uncertainty, which
// the flag on the set does not cover; it is tracked as its own concern
// rather than folded in here, where it would make the flag mean two
// different things.
//
// Returns nil when no propagation happened (no positions, or all
// positions have nil MarketValueUncertainty) so the measure layer
// can leave v1.Measure.UncertaintyAbs nil.
func sumUncertaintyInBaseCurrency(p *domain.Portfolio) *commonpb.Decimal {
	base := p.BaseCurrency()
	var inputs []*commonpb.Decimal
	for _, pos := range p.Positions() {
		if !pos.InBaseCurrency(base) || !pos.UncertaintyInBaseCurrency(base) {
			continue
		}
		inputs = append(inputs, pos.MarketValueUncertainty.Amount)
	}
	return PropagateSumIndependent(inputs...)
}
