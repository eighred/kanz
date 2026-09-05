package v1

import "time"

// MeasureProvenance names the MODEL behind one measure value (#1037).
//
// # The failure it exists to break
//
// A measure carried a name and a number and nothing else. "VaR99" was served by
// compute.VaR99 — 0.01 × GrossExposure, an illustrative constant whose own doc
// says "NOT a calibrated risk number" — and by varmodel.Historical, historical
// simulation over a price panel. The two were BYTE-IDENTICAL on the wire. Which
// one a deployment served turned on RISK_ENGINE_MARKETDATA_DATABASE_URL, which no
// manifest in infra/ sets, so the shipped rollout answered every VaR99 query with
// 1% of gross for the life of the pod.
//
// That number reached ORDER ADMISSION. The OMS folds the announcement and
// compliance.RiskLimitRule checks a mandate's VaR limit against it, and on a
// leveraged or volatile book 1% of gross is materially BELOW a real one-day 99%
// VaR — so the gate admitted orders it should have refused. The one signal built
// to answer "is this measure real", kanz_risk_measure_live, reads 1 in both
// configurations because its question is whether the NAME is registered.
//
// # Why a struct and not a string on Measure
//
// Presence is the signal, the same argument InputCoverage already won. The zero
// value means "this producer declares no model" — NOT "the number is
// model-derived", and NOT "it is a placeholder". Every measure this platform
// published before this type existed declares nothing, so absence is the state
// the estate is already in and a consumer must treat it as an unanswered
// question rather than as either verdict.
type MeasureProvenance struct {
	// Method is the model that produced Value. Empty ⇒ undeclared.
	Method MeasureMethod
	// ModelID keys the fitted model where one applies, matching
	// factor.v1.FactorModel.model_id. Empty for methods that fit no model —
	// portfolio arithmetic and the placeholders — which is most of them.
	ModelID string
	// ModelAsOf is when the model behind ModelID was fitted. It is NOT the
	// measure's as-of: a VaR computed this morning off a factor model fitted
	// last quarter is stale in a way the set's AsOf cannot express. Zero ⇒ no
	// fitted model.
	ModelAsOf time.Time
	// Params are the model parameters an operator needs to reproduce the number
	// — lookback, confidence, seed. LOW CARDINALITY BY CONTRACT: they travel on
	// every measure of every FACT and a reader may group by them, so never
	// per-position data and never the inputs themselves.
	Params map[string]string
}

// Declared reports whether this producer named a model at all. The three-valued
// read — declared placeholder / declared model / undeclared — is what a consumer
// branches on; collapsing it to a bool at the call site is how the undeclared
// case gets silently folded into one of the other two.
func (p MeasureProvenance) Declared() bool { return p.Method != "" }

// MeasureMethod is the closed, low-cardinality vocabulary of models a measure
// may name. It is a named type rather than a bare string so IsPlaceholder has
// exactly one home: the risk engine sets it and the OMS gate reads it, and a
// second copy of "which methods are real" is how those two come to disagree.
type MeasureMethod string

// The vocabulary. Add a constant here — never a bare string at a producer —
// because test/arch/placeholder_declares_itself_test.go resolves every declared
// method through this list and IsPlaceholder below, and an unlisted method is
// unclassified: it folds, and it gates order admission.
const (
	// MethodHistoricalSimulation revalues the book under each past return
	// scenario and reports the loss at the configured confidence. A real model.
	MethodHistoricalSimulation MeasureMethod = "historical_simulation"

	// MethodMonteCarlo simulates correlated returns from the fitted covariance
	// and reports the quantile of the simulated P&L. A real model.
	MethodMonteCarlo MeasureMethod = "monte_carlo"

	// MethodPortfolioArithmetic is a sum, ratio or index computed directly from
	// the book's own market values with no model and no provider —
	// GrossExposure, NetExposure, HHI. NOT a placeholder: the engine stands
	// behind the arithmetic, which is exactly why these measures are also exempt
	// from InputCoverage (nothing can decline for them).
	MethodPortfolioArithmetic MeasureMethod = "portfolio_arithmetic"

	// MethodOptionPricingGreeks is a dollar-Greek summed from per-position
	// option pricing (Black-Scholes closed form, or a binomial tree bumped).
	// A real model, and distinct from MethodNetExposurePlaceholder below — the
	// SAME measure name, Delta, is served by both depending on whether
	// RegisterGreeks ran.
	MethodOptionPricingGreeks MeasureMethod = "option_pricing_greeks"

	// MethodPlaceholder1PctGross is 0.01 × GrossExposure served under the name
	// VaR99. An illustrative constant with no volatility model behind it, and
	// the answer every deployment with no market-data DSN serves.
	MethodPlaceholder1PctGross MeasureMethod = "placeholder_1pct_gross"

	// MethodNetExposurePlaceholder is the signed base-currency sum served under
	// the name Delta — the "every $1 of position is $1 of delta to spot"
	// assumption, which holds for cash equities and is wrong for derivatives.
	// It is in compute.DefaultRegistry, so unlike the VaR placeholder it is
	// served on EVERY deployment, not only the ones missing a DSN.
	MethodNetExposurePlaceholder MeasureMethod = "net_exposure_placeholder"
)

// placeholderMethods is the set of methods that are illustrative rather than
// calibrated. ONE DEFINITION, read by the risk engine's posture gauge and by the
// OMS's admission gate.
//
// A SET AND NOT A PREFIX MATCH. The obvious `strings.HasPrefix(m, "placeholder")`
// misses net_exposure_placeholder, which is the one served on every deployment —
// and a naming convention is not a thing a control may depend on.
var placeholderMethods = map[MeasureMethod]bool{
	MethodPlaceholder1PctGross:   true,
	MethodNetExposurePlaceholder: true,
}

// IsPlaceholder reports whether this method is an illustrative stand-in rather
// than a model.
//
// FALSE FOR THE EMPTY METHOD, deliberately and load-bearingly. An undeclared
// measure is not a declared placeholder: every measure published before this
// field existed carries no provenance, and treating absence as a placeholder
// would fail every risk-limit mandate closed on the first deploy of this change.
// The cost of that choice is stated where it matters — a producer that forgets
// to declare is invisible to the gate, which is why the arch guard requires the
// declaration at the literal rather than trusting the runtime to notice.
func (m MeasureMethod) IsPlaceholder() bool { return placeholderMethods[m] }

// PlaceholderMethods returns the placeholder vocabulary in no particular order.
// For operators and observability — a caller deciding whether to act on one
// number must ask IsPlaceholder about that number, not scan a list.
func PlaceholderMethods() []MeasureMethod {
	out := make([]MeasureMethod, 0, len(placeholderMethods))
	for m := range placeholderMethods {
		out = append(out, m)
	}
	return out
}
