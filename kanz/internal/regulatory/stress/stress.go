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

// FactorCoverage records which of the model's macro factors the scenario
// actually carried.
//
// IT TRAVELS WITH THE SHOCKS, never beside them, for the reason the shocks need
// it at all: s.Shocks[macro] on a missing key is 0, so "this factor does not
// move" and "this scenario never mentioned this factor" produce the identical
// vector. The first is a modelling statement; the second is missing data (#623).
//
// The consequence is downstream and one-directional. The vector feeds
// ReverseStress, whose answer — how bad conditions must get before a breach — is
// then understated in the direction the filer benefits from.
type FactorCoverage struct {
	// Required is every macro factor some asset class actually responds to,
	// sorted. A factor whose beta is zero everywhere is NOT required: its absence
	// changes no number, and reporting it would train a reader to skim this.
	Required []string
	// Supplied is the required factors the scenario carried, sorted. AN EXPLICIT
	// ZERO COUNTS: a scenario stating "rates do not move" has supplied that
	// factor, and that is exactly the case this type exists to tell apart.
	Supplied []string
	// Missing is the rest, sorted — the transmission channels that contributed
	// nothing because nobody said what they did.
	Missing []string
}

// Complete reports that the scenario carried every factor the model responds to.
func (c FactorCoverage) Complete() bool { return len(c.Missing) == 0 }

// Fraction is the supplied share of required factors, in [0,1]. A model with no
// live factors is vacuously complete rather than a total failure — nothing is
// missing when nothing is needed.
func (c FactorCoverage) Fraction() float64 {
	if len(c.Required) == 0 {
		return 1
	}
	return float64(len(c.Supplied)) / float64(len(c.Required))
}

// Expand turns a macro scenario into per-asset-class return shocks (deterministic
// order via the returned map; callers sort for display) and the coverage of the
// factors it was computed from. Severity scales the macro shocks before
// transmission.
//
// THE SECOND RETURN IS THE REPAIR (#623). The shocks are unchanged — a missing
// factor still transmits nothing, which is the only arithmetic available — but a
// caller can now tell a deliberate single-factor scenario from a five-factor one
// that arrived with four channels missing. It reports rather than refuses,
// because a single-factor scenario is a legitimate thing to run and an error
// would make the legitimate case unrunnable to catch the broken one.
func (e ExpansionModel) Expand(s MacroScenario) (map[string]float64, FactorCoverage) {
	sev := s.severity()
	out := map[string]float64{}
	required := map[string]bool{}
	for assetClass, betas := range e.Beta {
		var shock float64
		for macro, beta := range betas {
			if beta != 0 {
				required[macro] = true
			}
			shock += beta * s.Shocks[macro] * sev
		}
		out[assetClass] = shock
	}
	return out, coverageOf(required, s.Shocks)
}

// coverageOf splits the required factors by whether the scenario carried them.
// Presence is tested with the comma-ok form, not against zero: an explicit zero
// is data.
func coverageOf(required map[string]bool, shocks map[string]float64) FactorCoverage {
	var cov FactorCoverage
	for macro := range required {
		cov.Required = append(cov.Required, macro)
		if _, ok := shocks[macro]; ok {
			cov.Supplied = append(cov.Supplied, macro)
		} else {
			cov.Missing = append(cov.Missing, macro)
		}
	}
	sort.Strings(cov.Required)
	sort.Strings(cov.Supplied)
	sort.Strings(cov.Missing)
	return cov
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
