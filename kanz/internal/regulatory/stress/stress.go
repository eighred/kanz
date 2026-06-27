// Package stress is the REG-01c firmwide stress framework — CCAR/DFAST-style
// macro scenarios expanded into instrument shocks, plus reverse stress testing
// (find the scenario severity that breaches a limit).
//
// # Boundary
//
// stress lives outside kanz/internal/risk, so it cannot import the MODEL-01h
// scenario library or the scenario engine. It instead PRODUCES the asset-class
// shocks a macro scenario implies (data), and takes the portfolio loss under a
// scenario as an injected LossFunc — the deployment wires that to the risk
// engine's scenario evaluation through the api/v* surface at the composition
// root. The "the P&L evaluation is an input" seam, the regulatory analog of the
// FRTB sensitivities-are-an-input stance.
package stress

import "sort"

// MacroScenario is a named set of macro-factor shocks (GDP, unemployment, equity,
// rates, credit spread), scaled by a severity multiplier.
type MacroScenario struct {
	Name string
	// Shocks maps a macro factor to its base shock (a level change or return).
	Shocks map[string]float64
	// Severity scales every shock (1.0 == as specified). The dimension reverse
	// stress walks.
	Severity float64
}

// severity returns the effective severity (defaulting to 1).
func (s MacroScenario) severity() float64 {
	if s.Severity == 0 {
		return 1
	}
	return s.Severity
}

// ExpansionModel maps macro factors to asset-class return shocks via betas:
// assetClassShock = Σ_macro beta[assetClass][macro] · macroShock. The factor-
// model analog of a CCAR macro-to-instrument transmission.
type ExpansionModel struct {
	// Beta[assetClass][macroFactor] is the sensitivity of the asset class's
	// return to a unit move in the macro factor.
	Beta map[string]map[string]float64
}

// Expand turns a macro scenario into per-asset-class return shocks (deterministic
// order via the returned map; callers sort for display). Severity scales the
// macro shocks before transmission.
func (e ExpansionModel) Expand(s MacroScenario) map[string]float64 {
	sev := s.severity()
	out := map[string]float64{}
	for assetClass, betas := range e.Beta {
		var shock float64
		for macro, beta := range betas {
			shock += beta * s.Shocks[macro] * sev
		}
		out[assetClass] = shock
	}
	return out
}

// AssetClasses returns the model's asset classes in stable order.
func (e ExpansionModel) AssetClasses() []string {
	out := make([]string, 0, len(e.Beta))
	for ac := range e.Beta {
		out = append(out, ac)
	}
	sort.Strings(out)
	return out
}

// LossFunc returns the portfolio loss (positive = loss) under a scenario scaled
// to the given severity. Injected — a deployment wires it to the risk engine's
// scenario evaluation; tests supply a deterministic function. Assumed monotone
// non-decreasing in severity (a worse macro scenario is a larger loss).
type LossFunc func(severity float64) float64

// ReverseStress finds the smallest severity in [0, maxSeverity] at which the loss
// reaches the breach threshold — the reverse-stress question "how bad must it get
// before we breach?". Returns the breaching severity and ok=true, or maxSeverity
// and ok=false when even the worst searched scenario does not breach. Bisection
// on the monotone loss.
func ReverseStress(loss LossFunc, threshold, maxSeverity float64) (float64, bool) {
	if loss(0) >= threshold {
		return 0, true // already breached with no stress
	}
	if loss(maxSeverity) < threshold {
		return maxSeverity, false // cannot breach within the search range
	}
	lo, hi := 0.0, maxSeverity
	for i := 0; i < 100; i++ {
		mid := 0.5 * (lo + hi)
		if loss(mid) >= threshold {
			hi = mid
		} else {
			lo = mid
		}
		if hi-lo < 1e-9 {
			break
		}
	}
	return hi, true
}
