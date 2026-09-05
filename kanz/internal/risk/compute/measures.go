package compute

import (
	"sort"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
)

// RISK-07 ships the *framework* for risk-measure computation plus
// four illustrative measures (GrossExposure, NetExposure, a
// parametric VaR placeholder, and a Delta placeholder). A fifth
// measure, HHI, is a real (non-placeholder) concentration measure —
// see its doc comment in concentration.go.
//
// # Honest scope
//
// The engine currently has no historical return time series, no
// volatility model, no factor model, and no per-instrument Greek
// data. Real VaR / sensitivities need all of those. The four
// measures here are deliberate placeholders — the architectural
// deliverable is the **registry pattern** that lets a quant team
// plug in real measure implementations later WITHOUT changing the
// api/v1 contract, the MeasureSet shape, or any caller code. When a
// real VaR model lands, it replaces `VaR99Func` in the registry; no
// other code changes.
//
// # Currency convention
//
// Measure values are dimensionless `*common.v1.Decimal` — the api/v1
// `Measure` type carries no currency field. Money-valued measures
// (GrossExposure, NetExposure, VaR99, Delta) are in the portfolio's
// BaseCurrency, and only positions whose MarketValue is denominated in
// BaseCurrency are summed (there is no FX layer in compute, RISK-06).
//
// Positions in other currencies are therefore excluded, and
// ComputeMeasures records exactly which ones on the returned MeasureSet
// (domain.WithCurrencyExclusions), which the engine turns into
// v1.QualityFlagCurrencyExcluded on the response and the publisher into
// an envelope DEGRADED flag on the FACT.
//
// This used to read "surfacing them as a quality flag is RISK-11's
// degraded-mode concern, not this layer's". That was false and the
// falsehood was load-bearing: RISK-11's Detector.Assess takes only a
// time.Time (internal/risk/degraded.go), so it never sees a position and
// is structurally incapable of noticing one was dropped. Nothing
// surfaced the exclusion anywhere, and a USD-base portfolio holding only
// EUR reported GrossExposure = 0 in ModeNormal with no flags — identical
// to an empty book, and low in the direction that makes a concentration
// limit pass when the full book would breach (#257). The exclusion is
// recorded HERE, at the only layer that can see it.
//
// # Measure naming
//
// Names follow the api/v1 Measure doc convention (`VaR99`, `Delta`,
// `Gamma`) — short, CamelCase, dimensional suffix on aggregates that
// have meaningful variants (VaR99 vs VaR95). Keep names stable: a
// rename is a breaking change for every dashboard / alert wired to
// them, same one-way-door discipline as event_type names.

// MeasureFunc computes one named measure from a portfolio. Pure
// function — no side effects, no state, no I/O. Returns the
// zero-valued Measure (Value=0, UncertaintyAbs=nil) when no
// applicable positions exist; never returns nil or an error
// (errors here would force the api/v1 surface into a partial-
// response branch, which complicates callers for no upside —
// missing data is signalled by a zero-value measure plus a
// quality flag at the response level).
type MeasureFunc func(p *domain.Portfolio) v1.Measure

// Registry is the catalog of measures the engine computes. Entries
// are pure functions keyed by MeasureName — adding a measure means
// inserting one entry; removing means deleting one.
type Registry struct {
	funcs map[v1.MeasureName]MeasureFunc
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{funcs: make(map[v1.MeasureName]MeasureFunc)}
}

// Register adds or replaces a measure. Quants plug in real models
// by calling Register on the engine's registry at startup; tests
// use the same hook to substitute deterministic implementations.
func (r *Registry) Register(name v1.MeasureName, fn MeasureFunc) {
	r.funcs[name] = fn
}

// Names returns the registered measure names in stable lexicographic
// order — useful for filter validation and audit logs.
func (r *Registry) Names() []v1.MeasureName {
	out := make([]v1.MeasureName, 0, len(r.funcs))
	for n := range r.funcs {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// DefaultRegistry returns the RISK-07 baseline registry — the four
// illustrative measures, the real HHI concentration measure, and the
// framework. Engines override by constructing this then calling
// Register with real models.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register(MeasureGrossExposure, GrossExposure)
	r.Register(MeasureNetExposure, NetExposure)
	r.Register(MeasureVaR99, VaR99)
	r.Register(MeasureDelta, Delta)
	r.Register(MeasureHHI, HHI)
	return r
}

// ComputeMeasures runs every registered measure against the
// portfolio and bundles the results into a MeasureSet keyed by
// measure name. The filter narrows the set (nil or empty ⇒ all);
// names in filter that are not in the registry are silently
// dropped (matches v1.MeasureSet.Lookup's miss semantics).
func ComputeMeasures(p *domain.Portfolio, r *Registry, filter []v1.MeasureName) *domain.MeasureSet {
	if r == nil {
		r = DefaultRegistry()
	}
	want := func(name v1.MeasureName) bool {
		if len(filter) == 0 {
			return true
		}
		for _, f := range filter {
			if f == name {
				return true
			}
		}
		return false
	}
	results := make(map[v1.MeasureName]v1.Measure)
	for name, fn := range r.funcs {
		if !want(name) {
			continue
		}
		results[name] = fn(p)
	}
	// Every registered measure aggregates over base-currency positions
	// only, so the exclusion set is a property of the PORTFOLIO, not of
	// any one measure — computed once here and attached to the whole set
	// rather than per measure.
	return domain.NewMeasureSet(p.ID(), p.AsOf(), results,
		domain.WithCurrencyExclusions(p.CurrencyExclusions()))
}

// --- Concrete measures (RISK-07 baseline) ------------------------------

// Names — short, CamelCase, dimensional suffix where meaningful.
const (
	MeasureGrossExposure     v1.MeasureName = "GrossExposure"
	MeasureNetExposure       v1.MeasureName = "NetExposure"
	MeasureVaR99             v1.MeasureName = "VaR99"
	MeasureES99              v1.MeasureName = "ES99"
	MeasureDelta             v1.MeasureName = "Delta"
	MeasureHHI               v1.MeasureName = "HHI"
	MeasureMaxDrawdown       v1.MeasureName = "MaxDrawdown"
	MeasureMaxDrawdownAmount v1.MeasureName = "MaxDrawdownAmount"
)

// GrossExposure is the sum of absolute MarketValue across positions
// denominated in the portfolio's BaseCurrency. Other-currency
// positions are skipped (no FX layer in compute, see RISK-06).
// Uncertainty is propagated as the independent-sum of per-position
// MarketValueUncertainty (RISK-08).
func GrossExposure(p *domain.Portfolio) v1.Measure {
	return v1.Measure{
		Name:           MeasureGrossExposure,
		Value:          sumInBaseCurrency(p, true /*absolute*/),
		UncertaintyAbs: sumUncertaintyInBaseCurrency(p),
		// ARITHMETIC, NOT A MODEL, and saying so is not a formality. This
		// measure can never report InputCoverage — there is no provider to
		// decline — so provenance is the only field on it a consumer can read
		// to tell it apart from the illustrative constants below, which are
		// coverage-less for the same reason and are NOT arithmetic the engine
		// stands behind (#1037).
		Provenance: v1.MeasureProvenance{Method: v1.MethodPortfolioArithmetic},
	}
}

// NetExposure is the signed sum of MarketValue in BaseCurrency. The
// arithmetic counterpart of GrossExposure. Uncertainty propagation
// matches GrossExposure — the variance of a sum equals the sum of
// variances under the independence assumption regardless of the
// sign of each term.
func NetExposure(p *domain.Portfolio) v1.Measure {
	return v1.Measure{
		Name:           MeasureNetExposure,
		Value:          sumInBaseCurrency(p, false /*signed*/),
		UncertaintyAbs: sumUncertaintyInBaseCurrency(p),
		Provenance:     v1.MeasureProvenance{Method: v1.MethodPortfolioArithmetic},
	}
}

// var1pctOfGross is the parametric VaR placeholder factor: 1% of
// gross. A real RISK-07b will replace this with z_α × σ × value
// using a per-instrument volatility model.
var var1pctOfGross = &commonpb.Decimal{Coefficient: 1, Exponent: -2} // 0.01

// VaR99 is a placeholder for 1-day 99% Value-at-Risk. Computed as
// 1% × GrossExposure — illustrative, NOT a calibrated risk number.
// Replace by registering a real implementation against
// MeasureVaR99 before serving the api/v1 surface in production.
// Uncertainty propagation uses the scalar rule (RISK-08):
// σ(VaR) = 0.01 × σ(Gross).
func VaR99(p *domain.Portfolio) v1.Measure {
	gross := sumInBaseCurrency(p, true)
	grossUnc := sumUncertaintyInBaseCurrency(p)
	return v1.Measure{
		Name:           MeasureVaR99,
		Value:          mulDecimal(gross, var1pctOfGross),
		UncertaintyAbs: PropagateScalar(var1pctOfGross, grossUnc),
		// THE PLACEHOLDER DECLARES ITSELF, ON EVERY RESPONSE (#1037).
		//
		// The doc above has said "NOT a calibrated risk number" since RISK-07,
		// and main.go logs a WARN naming this measure once at boot. Neither
		// travels with the number. This value is announced on
		// risk.portfolio.measures_computed under the same name and the same
		// shape as historical-simulation VaR99, the OMS folds it, and
		// compliance.RiskLimitRule checks a mandate limit against it — 1% of
		// gross, which on a leveraged book sits BELOW a real one-day 99% VaR,
		// so the gate admits orders the real number would refuse. riskview
		// refuses on this method, and cannot without it.
		Provenance: v1.MeasureProvenance{
			Method: v1.MethodPlaceholder1PctGross,
			Params: map[string]string{"gross_fraction": "0.01"},
		},
	}
}

// Delta is a placeholder for the portfolio's first-order sensitivity
// to its underlying (spot delta in option-trader terms). Returns the
// signed BaseCurrency exposure — the trivial "every $1 of position
// is $1 of delta to spot" assumption that holds for cash equities
// but is wrong for derivatives. Replace via Register when real
// per-instrument deltas land. Uncertainty propagation matches
// NetExposure (same underlying sum).
func Delta(p *domain.Portfolio) v1.Measure {
	return v1.Measure{
		Name:           MeasureDelta,
		Value:          sumInBaseCurrency(p, false),
		UncertaintyAbs: sumUncertaintyInBaseCurrency(p),
		// AND THIS ONE IS SERVED ON EVERY DEPLOYMENT, not only the ones with no
		// market-data DSN: it is in DefaultRegistry and RegisterGreeks — the
		// only thing that overrides it — has no production caller. So the
		// platform's Delta is NetExposure under another name, everywhere, and
		// kanz_risk_measure_live{measure="Delta"} reads 1 for it.
		Provenance: v1.MeasureProvenance{Method: v1.MethodNetExposurePlaceholder},
	}
}

// sumInBaseCurrency walks positions whose MarketValue is denominated in
// the portfolio's BaseCurrency and sums them, optionally taking absolute
// values. Single helper backing every baseline measure.
//
// The filter rule itself is domain.Position.InBaseCurrency — one
// function for all ten sites that had a copy of it, and the same
// function Portfolio.CurrencyExclusions uses to report what it drops,
// so what is excluded here and what is reported excluded cannot diverge.
func sumInBaseCurrency(p *domain.Portfolio, abs bool) *commonpb.Decimal {
	base := p.BaseCurrency()
	var sum decAccum // O(1) allocs — see decAccum (LATENCY-01c)
	for _, pos := range p.Positions() {
		if !pos.InBaseCurrency(base) {
			continue
		}
		sum.add(pos.MarketValue.Amount, abs)
	}
	return sum.decimal()
}
